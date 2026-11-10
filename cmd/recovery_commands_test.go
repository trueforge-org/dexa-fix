package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mainbackend "github.com/trueforge-org/dexafix/pkg/backend/main"
	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/store"
	"github.com/trueforge-org/dexafix/pkg/store/sqlstore"
)

// recoveryDB is a database with a key, as a server would have made it, and the flags that open it.
type recoveryDB struct {
	st    *sqlstore.Store
	flags []string
}

func newRecoveryDB(t *testing.T) recoveryDB {
	t.Helper()
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "k.key")
	key := strings.Repeat("5a", 32)
	if err := os.WriteFile(keyFile, []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	k, _ := sqlstore.ParseKey(key)
	c, _ := sqlstore.NewCipher(k)
	dsn := "file:" + filepath.Join(dir, "d.db")
	st, err := sqlstore.Open(context.Background(), sqlstore.DriverSQLite, dsn, sqlstore.WithCipher(c))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return recoveryDB{st, []string{"--dsn", dsn, "--encryption-key-file", keyFile}}
}

func (d recoveryDB) with(extra ...string) []string {
	return append(append([]string{}, extra...), d.flags...)
}

func TestUserReset2FARemovesTheSecondStepAndNamesTheAccount(t *testing.T) {
	ctx := context.Background()
	d := newRecoveryDB(t)
	u, err := d.st.CreateUserWithIdentity(ctx, "alice", "alice@example.com", models.Identity{Provider: models.ProviderLocal, Secret: "h"})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.st.PutPendingTOTP(ctx, u.ID, "JBSWY3DPEHPK3PXP"); err != nil {
		t.Fatal(err)
	}
	if err := d.st.ConfirmTOTP(ctx, u.ID, 1); err != nil {
		t.Fatal(err)
	}

	// The address is matched the way sign-in matches it: case does not matter.
	out, _, err := run(t, d.with("user", "reset-2fa", "Alice@Example.com")...)
	if err != nil || !strings.Contains(out, "two-step verification removed for alice") {
		t.Fatalf("reset-2fa: %q %v", out, err)
	}
	if _, err := d.st.GetTOTP(ctx, u.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the second step is still there: %v", err)
	}
}

func TestUserReset2FAFailsClearly(t *testing.T) {
	ctx := context.Background()
	t.Run("an address no account has", func(t *testing.T) {
		d := newRecoveryDB(t)
		_, _, err := run(t, d.with("user", "reset-2fa", "nobody@example.com")...)
		if err == nil || !strings.Contains(err.Error(), `no account has the e-mail address "nobody@example.com"`) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("no address", func(t *testing.T) {
		d := newRecoveryDB(t)
		if _, _, err := run(t, d.with("user", "reset-2fa")...); err == nil {
			t.Fatal("a missing address was accepted")
		}
	})
	t.Run("a password in the connection string", func(t *testing.T) {
		_, _, err := run(t, "user", "reset-2fa", "a@example.com", "--dsn", "postgres://u:hunter2hunter2@localhost/x")
		if err == nil || !strings.Contains(err.Error(), "contains a password") {
			t.Fatalf("%v", err)
		}
	})
	t.Run("an encryption key that is not a key", func(t *testing.T) {
		d := newRecoveryDB(t)
		bad := writeSecretFile(t, "not a key\n")
		_, _, err := run(t, "user", "reset-2fa", "a@example.com", "--dsn", d.flags[1], "--encryption-key-file", bad)
		if err == nil || !strings.Contains(err.Error(), bad) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("a database that was never migrated", func(t *testing.T) {
		d := newRecoveryDB(t)
		_, _, err := run(t, "user", "reset-2fa", "a@example.com", "--dsn", newDSN(t), "--encryption-key-file", d.flags[3])
		if err == nil || !strings.Contains(err.Error(), "migrat") {
			t.Fatalf("%v", err)
		}
	})
	t.Run("an account whose e-mail address cannot be opened", func(t *testing.T) {
		d := newRecoveryDB(t)
		if _, err := d.st.CreateUserWithIdentity(ctx, "bob", "bob@example.com", models.Identity{Provider: models.ProviderLocal, Secret: "h"}); err != nil {
			t.Fatal(err)
		}
		if err := d.st.ExecForTests(ctx, "UPDATE users SET email = 'enc:damaged'"); err != nil {
			t.Fatal(err)
		}
		_, _, err := run(t, d.with("user", "reset-2fa", "bob@example.com")...)
		if err == nil || strings.Contains(err.Error(), "no account has") {
			t.Fatalf("a failing lookup was reported as a missing account: %v", err)
		}
	})
	t.Run("a second step that cannot be deleted", func(t *testing.T) {
		d := newRecoveryDB(t)
		u, err := d.st.CreateUserWithIdentity(ctx, "bob", "bob@example.com", models.Identity{Provider: models.ProviderLocal, Secret: "h"})
		if err != nil {
			t.Fatal(err)
		}
		if err := d.st.PutPendingTOTP(ctx, u.ID, "JBSWY3DPEHPK3PXP"); err != nil {
			t.Fatal(err)
		}
		if err := d.st.ExecForTests(ctx, "CREATE TRIGGER keep_totp BEFORE DELETE ON user_totp BEGIN SELECT RAISE(ABORT, 'kept'); END"); err != nil {
			t.Fatal(err)
		}
		_, _, err = run(t, d.with("user", "reset-2fa", "bob@example.com")...)
		if err == nil || !strings.Contains(err.Error(), "kept") {
			t.Fatalf("a failed delete was reported as done: %v", err)
		}
	})
}

func TestUserReset2FARunsWithoutAContextFromTheRunner(t *testing.T) {
	resetFlags(t)
	d := newRecoveryDB(t)
	userReset2FACmd.SetContext(nil)
	userReset2FACmd.Flags().Set("dsn", d.flags[1])
	userReset2FACmd.Flags().Set("encryption-key-file", d.flags[3])
	err := userReset2FACmd.RunE(userReset2FACmd, []string{"nobody@example.com"})
	if err == nil || !strings.Contains(err.Error(), "no account has") {
		t.Fatalf("%v", err)
	}
}

func TestSettingsResetNetworkClearsTheSettingsThatLockedYouOut(t *testing.T) {
	ctx := context.Background()
	d := newRecoveryDB(t)
	for _, name := range append(append([]string{}, mainbackend.NetworkSettingNames...), "smtp_host") {
		if err := d.st.PutSetting(ctx, name, "x"); err != nil {
			t.Fatal(err)
		}
	}
	out, _, err := run(t, d.with("settings", "reset-network")...)
	if err != nil || !strings.Contains(out, "cleared; restart the server") {
		t.Fatalf("reset-network: %q %v", out, err)
	}
	for _, name := range mainbackend.NetworkSettingNames {
		if _, err := d.st.GetSetting(ctx, name); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s is still set: %v", name, err)
		}
	}
	if v, err := d.st.GetSetting(ctx, "smtp_host"); err != nil || v != "x" {
		t.Errorf("a setting that did not lock anyone out was changed: %q %v", v, err)
	}
}

func TestSettingsResetNetworkFailsClearly(t *testing.T) {
	ctx := context.Background()
	t.Run("a password in the connection string", func(t *testing.T) {
		_, _, err := run(t, "settings", "reset-network", "--dsn", "postgres://u:hunter2hunter2@localhost/x")
		if err == nil || !strings.Contains(err.Error(), "contains a password") {
			t.Fatalf("%v", err)
		}
	})
	t.Run("an encryption key that is not a key", func(t *testing.T) {
		d := newRecoveryDB(t)
		bad := writeSecretFile(t, "not a key\n")
		_, _, err := run(t, "settings", "reset-network", "--dsn", d.flags[1], "--encryption-key-file", bad)
		if err == nil || !strings.Contains(err.Error(), bad) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("a database that was never migrated", func(t *testing.T) {
		d := newRecoveryDB(t)
		_, _, err := run(t, "settings", "reset-network", "--dsn", newDSN(t), "--encryption-key-file", d.flags[3])
		if err == nil || !strings.Contains(err.Error(), "migrat") {
			t.Fatalf("%v", err)
		}
	})
	t.Run("a settings table that cannot be written", func(t *testing.T) {
		d := newRecoveryDB(t)
		if err := d.st.PutSetting(ctx, mainbackend.NetworkSettingNames[0], "x"); err != nil {
			t.Fatal(err)
		}
		if err := d.st.ExecForTests(ctx, "CREATE TRIGGER keep_settings BEFORE DELETE ON app_settings BEGIN SELECT RAISE(ABORT, 'kept'); END"); err != nil {
			t.Fatal(err)
		}
		_, _, err := run(t, d.with("settings", "reset-network")...)
		if err == nil || !strings.Contains(err.Error(), "kept") {
			t.Fatalf("a failed delete was reported as done: %v", err)
		}
	})
}

func TestSettingsResetNetworkRunsWithoutAContextFromTheRunner(t *testing.T) {
	resetFlags(t)
	d := newRecoveryDB(t)
	settingsResetNetworkCmd.SetContext(nil)
	settingsResetNetworkCmd.Flags().Set("dsn", d.flags[1])
	settingsResetNetworkCmd.Flags().Set("encryption-key-file", d.flags[3])
	var out strings.Builder
	settingsResetNetworkCmd.SetOut(&out)
	if err := settingsResetNetworkCmd.RunE(settingsResetNetworkCmd, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "cleared") {
		t.Errorf("output: %q", out.String())
	}
}
