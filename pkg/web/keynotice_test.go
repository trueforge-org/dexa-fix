package web

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/importer"
	"github.com/trueforge-org/dexa-fix/pkg/importer/withings"
	"github.com/trueforge-org/dexa-fix/pkg/store"
	"github.com/trueforge-org/dexa-fix/pkg/store/sqlstore"
)

func TestKeyViewDescribesTheKey(t *testing.T) {
	const day = 24 * time.Hour
	old := time.Now().UTC().AddDate(0, 0, -100)
	wantSince := old.Format(dateLayout)

	t.Run("not encrypted is not shown", func(t *testing.T) {
		s := &Server{KeyReminder: 30 * day}
		v := s.keyView(store.Stats{Encrypted: false})
		if v.Shown {
			t.Errorf("Shown = true for an unencrypted database")
		}
	})

	t.Run("old key is due and dated", func(t *testing.T) {
		s := &Server{KeyReminder: 30 * day}
		v := s.keyView(store.Stats{Encrypted: true, KeyID: "k1", KeySince: old})
		if !v.Shown || v.ID != "k1" {
			t.Errorf("Shown/ID = %v/%q, want true/%q", v.Shown, v.ID, "k1")
		}
		if !v.Due {
			t.Errorf("Due = false for a 100 day old key with a 30 day reminder")
		}
		if v.AgeDays < 99 || v.AgeDays > 101 {
			t.Errorf("AgeDays = %d, want about 100", v.AgeDays)
		}
		if v.Since != wantSince {
			t.Errorf("Since = %q, want %q", v.Since, wantSince)
		}
	})

	t.Run("zero reminder never makes the key due", func(t *testing.T) {
		s := &Server{KeyReminder: 0}
		v := s.keyView(store.Stats{Encrypted: true, KeyID: "k1", KeySince: old})
		if v.Due {
			t.Errorf("Due = true with KeyReminder 0")
		}
		if v.AgeDays < 99 || v.AgeDays > 101 {
			t.Errorf("AgeDays = %d, want about 100", v.AgeDays)
		}
	})

	t.Run("previous key asks for a rekey", func(t *testing.T) {
		s := &Server{KeyReminder: 30 * day}
		v := s.keyView(store.Stats{Encrypted: true, KeyID: "k2", KeySince: old, KeyPrevious: true})
		if !v.Rekey {
			t.Errorf("Rekey = false when KeyPrevious is set")
		}
	})

	t.Run("unknown key date", func(t *testing.T) {
		s := &Server{KeyReminder: 30 * day}
		v := s.keyView(store.Stats{Encrypted: true, KeyID: "k1"})
		if v.Since != "" {
			t.Errorf("Since = %q, want empty", v.Since)
		}
		if v.Due {
			t.Errorf("Due = true without a known key date")
		}
	})
}

// newKeyNoticeServer is newServerAndStore with encryption turned on, so the Admin page shows the key.
func newKeyNoticeServer(t *testing.T) (*httptest.Server, *sqlstore.Store) {
	t.Helper()
	key, err := sqlstore.ParseKey(strings.Repeat("ab", sqlstore.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := sqlstore.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlstore.Open(context.Background(), sqlstore.DriverSQLite, "file:"+t.Name()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)", sqlstore.WithCipher(cipher))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.PutSetting(context.Background(), settingSignup, valueEnabled); err != nil {
		t.Fatal(err)
	}
	srv, err := New(st, importer.NewRegistry(withings.New(withings.Config{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	testServers.Store(ts, srv)
	return ts, st
}

func TestAdminShowsEncryptionKey(t *testing.T) {
	ts, _ := newKeyNoticeServer(t)
	c := noRedirectClient()
	signup(t, ts, c, "root")
	code, b := body(t, c, ts.URL+"/admin")
	if code != 200 {
		t.Fatalf("admin page: %d", code)
	}
	if !strings.Contains(b, "Encryption key") {
		t.Fatal("admin page does not show the encryption key")
	}
}
