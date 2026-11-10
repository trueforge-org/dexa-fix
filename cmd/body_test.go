package cmd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	bodybackend "github.com/trueforge-org/dexafix/pkg/backend/body"
	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/shared/bodyapi"
	"github.com/trueforge-org/dexafix/pkg/shared/mtls"
	"github.com/trueforge-org/dexafix/pkg/store/sqlstore"
)

func bodyFlags(t *testing.T, kv ...string) *cobra.Command {
	t.Helper()
	c := &cobra.Command{Use: "api"}
	addBodyClientFlags(c)
	for i := 0; i < len(kv); i += 2 {
		if err := c.Flags().Set(kv[i], kv[i+1]); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

// writeSigningKey makes a key file with the real command and returns its path.
func writeSigningKey(t *testing.T) string {
	t.Helper()
	var out bytes.Buffer
	c := &cobra.Command{Use: "body-token-key"}
	c.SetOut(&out)
	if err := secretsBodyTokenKeyCmd.RunE(c, nil); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "body-token.key")
	if err := os.WriteFile(p, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBodyClientRules(t *testing.T) {
	secret := writeSecretFile(t, strings.Repeat("b", 40)+"\n")
	key := writeSigningKey(t)

	// Without --body-url the body service runs inside the process, and the other flags make no sense.
	if c, _, remote, err := bodyClient(bodyFlags(t)); err != nil || remote || c != nil {
		t.Fatalf("no url: %v %v %v", c, remote, err)
	}
	for _, kv := range [][]string{
		{"body-secret-file", secret}, {"body-token-key-file", key}, {"body-ca-file", "ca.pem"},
		{"body-client-cert-file", "c.pem"}, {"body-client-key-file", "c.key"}, {"body-server-name", "body"},
	} {
		if _, _, _, err := bodyClient(bodyFlags(t, kv...)); err == nil || !strings.Contains(err.Error(), "--body-url") {
			t.Errorf("%s without a url: %v", kv[0], err)
		}
	}

	for name, tc := range map[string]struct {
		kv   []string
		want string
	}{
		"a bad address":                {[]string{"body-url", "body:8082"}, "--body-url"},
		"a bad scheme":                 {[]string{"body-url", "ftp://body:8082"}, "--body-url"},
		"no secret":                    {[]string{"body-url", "http://body:8082"}, "body-secret-file"},
		"no signing key":               {[]string{"body-url", "http://body:8082", "body-secret-file", secret}, "body-token-key-file"},
		"a short secret":               {[]string{"body-url", "http://body:8082", "body-secret-file", writeSecretFile(t, "short\n"), "body-token-key-file", key}, "at least"},
		"a key that is not a key":      {[]string{"body-url", "http://body:8082", "body-secret-file", secret, "body-token-key-file", writeSecretFile(t, "not a key at all\n")}, "not a signing key"},
		"https without mutual TLS":     {[]string{"body-url", "https://body:8082", "body-secret-file", secret, "body-token-key-file", key}, "mutual TLS"},
		"https with one file of three": {[]string{"body-url", "https://body:8082", "body-secret-file", secret, "body-token-key-file", key, "body-ca-file", "ca.pem"}, "mutual TLS"},
		"http with certificate files":  {[]string{"body-url", "http://body:8082", "body-secret-file", secret, "body-token-key-file", key, "body-ca-file", "ca.pem"}, "https://"},
	} {
		if _, _, _, err := bodyClient(bodyFlags(t, tc.kv...)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v (want %q)", name, err, tc.want)
		}
	}

	c, secrets, remote, err := bodyClient(bodyFlags(t, "body-url", "http://body:8082", "body-secret-file", secret, "body-token-key-file", key))
	if err != nil || !remote || c == nil || len(secrets) != 1 {
		t.Fatalf("a good configuration: %v %v %v", c, remote, err)
	}
}

func TestSecretsOfTheThreeDoorsMustDiffer(t *testing.T) {
	ok := map[string][]string{"service": {"s1"}, "worker": {"w1"}, "body": {"b1"}}
	if err := distinctSecrets(ok); err != nil {
		t.Fatalf("three different secrets: %v", err)
	}
	if err := distinctSecrets(map[string][]string{"service": {"x"}, "body": {"x"}}); err == nil || !strings.Contains(err.Error(), "service secret and the body secret") {
		t.Errorf("service = body: %v", err)
	}
	if err := distinctSecrets(map[string][]string{"worker": {"x"}, "body": {"y", "x"}}); err == nil {
		t.Error("a spare secret that is the worker's was accepted")
	}
	if err := distinctSecrets(map[string][]string{"service": {"x"}, "worker": {"x"}}); err == nil {
		t.Error("service = worker was accepted")
	}
	// One door may list the same secret twice.
	if err := distinctSecrets(map[string][]string{"body": {"x", "x"}}); err != nil {
		t.Errorf("a repeated line of one door: %v", err)
	}
	if err := distinctSecrets(nil); err != nil {
		t.Errorf("no secrets: %v", err)
	}
}

func TestBodyServerTLSNeedsAllThreeFiles(t *testing.T) {
	newCmd := func(kv ...string) *cobra.Command {
		c := &cobra.Command{Use: "body"}
		addBodyServerTLSFlags(c)
		for i := 0; i < len(kv); i += 2 {
			if err := c.Flags().Set(kv[i], kv[i+1]); err != nil {
				t.Fatal(err)
			}
		}
		return c
	}
	if cfg, err := bodyServerTLS(newCmd()); err != nil || cfg != nil {
		t.Errorf("no files: %v %v", cfg, err)
	}
	if _, err := bodyServerTLS(newCmd("body-tls-cert-file", "a.pem")); err == nil || !strings.Contains(err.Error(), "together") {
		t.Errorf("one file: %v", err)
	}
	if _, err := bodyServerTLS(newCmd("body-tls-cert-file", "a.pem", "body-tls-key-file", "a.key", "body-tls-client-ca-file", "missing.pem")); err == nil {
		t.Error("files that do not exist were accepted")
	}
}

// The key pair made by the two commands works: the public key checks what the private key signs.
func TestSecretsBodyTokenKeyAndPublic(t *testing.T) {
	keyPath := writeSigningKey(t)
	var out bytes.Buffer
	c := &cobra.Command{Use: "body-token-public"}
	c.Flags().String("key-file", keyPath, "")
	c.SetOut(&out)
	if err := secretsBodyTokenPublicCmd.RunE(c, nil); err != nil {
		t.Fatal(err)
	}
	pubs, err := bodyapi.ParsePublicKeys(out.String())
	if err != nil || len(pubs) != 1 {
		t.Fatalf("public key: %v", err)
	}
	priv, err := readSigningKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !priv.Public().(ed25519.PublicKey).Equal(pubs[0]) {
		t.Error("the public key does not belong to the private one")
	}
	now := time.Now()
	if _, err := bodyapi.NewVerifier(pubs[0]).Verify(bodyapi.NewSigner(priv).Sign(7, bodyapi.ScopeRead, nil, now), now); err != nil {
		t.Errorf("a token of the key does not verify: %v", err)
	}
	// Two keys differ, and a missing file is refused.
	if writeSigningKey(t) == keyPath {
		t.Fatal("same path")
	}
	c2 := &cobra.Command{Use: "body-token-public"}
	c2.Flags().String("key-file", "", "")
	if err := secretsBodyTokenPublicCmd.RunE(c2, nil); err == nil {
		t.Error("no --key-file was accepted")
	}
	c2.Flags().Set("key-file", filepath.Join(t.TempDir(), "missing"))
	if err := secretsBodyTokenPublicCmd.RunE(c2, nil); err == nil {
		t.Error("a missing key file was accepted")
	}
}

func TestSecretsBodySecretIsLongEnough(t *testing.T) {
	var out bytes.Buffer
	c := &cobra.Command{Use: "body-secret"}
	c.SetOut(&out)
	if err := secretsBodySecretCmd.RunE(c, nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); len(got) < minServiceSecretLen {
		t.Errorf("secret %q is too short", got)
	}
}

func TestSecretsMTLSForTheBodyLinkHasItsOwnFilesAndCA(t *testing.T) {
	mk := func(link string) string {
		dir := filepath.Join(t.TempDir(), "set")
		c := &cobra.Command{Use: "mtls"}
		c.Flags().String("dir", dir, "")
		c.Flags().StringSlice("name", []string{"body"}, "")
		c.Flags().String("link", link, "")
		var out bytes.Buffer
		c.SetOut(&out)
		c.SetErr(&bytes.Buffer{})
		if err := secretsMTLSCmd.RunE(c, nil); err != nil {
			t.Fatal(err)
		}
		if link == "body" && !strings.Contains(out.String(), "--body-tls-cert-file") {
			t.Errorf("the instructions do not name the body flags: %s", out.String())
		}
		return dir
	}
	body, web := mk("body"), mk("web")
	for name, mode := range map[string]os.FileMode{"ca.pem": 0o644, "body.pem": 0o644, "body.key": 0o600, "api-client.pem": 0o644, "api-client.key": 0o600} {
		fi, err := os.Stat(filepath.Join(body, name))
		if err != nil || fi.Mode().Perm() != mode {
			t.Errorf("%s: %v %v", name, fi, err)
		}
	}
	// Every link has a CA of its own: nothing of the web link opens the body service.
	a, _ := os.ReadFile(filepath.Join(body, "ca.pem"))
	b, _ := os.ReadFile(filepath.Join(web, "ca.pem"))
	if bytes.Equal(a, b) {
		t.Error("the two links share a CA")
	}
	c := &cobra.Command{Use: "mtls"}
	c.Flags().String("dir", t.TempDir(), "")
	c.Flags().StringSlice("name", []string{"x"}, "")
	c.Flags().String("link", "elsewhere", "")
	c.SetErr(&bytes.Buffer{})
	if err := secretsMTLSCmd.RunE(c, nil); err == nil || !strings.Contains(err.Error(), "--link") {
		t.Errorf("an unknown link: %v", err)
	}
}

// serve body refuses to start without what ties it to the api process.
func TestServeBodyNeedsItsSecretAndPublicKey(t *testing.T) {
	if err := runServe(t, "body", "--listen", ""); err == nil || !strings.Contains(err.Error(), "--listen") {
		t.Errorf("no listener: %v", err)
	}
	err := runServe(t, "body", "--listen", ":0")
	if err == nil || !strings.Contains(err.Error(), "body-secret-file") {
		t.Errorf("no secret: %v", err)
	}
	secret := writeSecretFile(t, strings.Repeat("c", 40)+"\n")
	err = runServe(t, "body", "--listen", ":0", "--body-secret-file", secret)
	if err == nil || !strings.Contains(err.Error(), "body-token-public-key-file") {
		t.Errorf("no public key: %v", err)
	}
	err = runServe(t, "body", "--listen", ":0", "--body-secret-file", secret, "--body-token-public-key-file", writeSecretFile(t, "garbage\n"))
	if err == nil || !strings.Contains(err.Error(), "public key") {
		t.Errorf("a file that is not a public key: %v", err)
	}
	// The private key is not accepted where the public one belongs: it would hand the service the
	// power to make tokens.
	err = runServe(t, "body", "--listen", ":0", "--body-secret-file", secret, "--body-token-public-key-file", writeSigningKey(t))
	if err == nil || !strings.Contains(err.Error(), "PRIVATE") {
		t.Errorf("the file of the private key as a public key: %v", err)
	}
}

// The body link over mutual TLS, end to end: the api side built from its flags reaches a real body
// service behind the certificates `secrets mtls --link body` made, and a certificate of another
// link (made by another CA) is refused.
func TestBodyLinkOverMutualTLS(t *testing.T) {
	mk := func(link string) string {
		dir := filepath.Join(t.TempDir(), link)
		c := &cobra.Command{Use: "mtls"}
		c.Flags().String("dir", dir, "")
		c.Flags().StringSlice("name", []string{"127.0.0.1"}, "")
		c.Flags().String("link", link, "")
		c.SetOut(&bytes.Buffer{})
		c.SetErr(&bytes.Buffer{})
		if err := secretsMTLSCmd.RunE(c, nil); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	bodySet, webSet := mk("body"), mk("web")

	ctx := context.Background()
	st, err := sqlstore.OpenBody(ctx, sqlstore.DriverSQLite, "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	_, pub, priv := bodyapi.GenerateKey()
	secretFile := writeSecretFile(t, strings.Repeat("d", 40)+"\n")
	svc, err := bodybackend.New(st, strings.Repeat("d", 40), bodyapi.NewVerifier(pub))
	if err != nil {
		t.Fatal(err)
	}
	srvCfg, err := mtls.ServerConfig(filepath.Join(bodySet, "body.pem"), filepath.Join(bodySet, "body.key"), filepath.Join(bodySet, "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(svc.Handler())
	ts.TLS = srvCfg
	ts.StartTLS()
	t.Cleanup(ts.Close)

	keyFile := filepath.Join(t.TempDir(), "k")
	if err := os.WriteFile(keyFile, []byte(bodyapi.EncodePrivateKey(priv)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	clientFor := func(set, certName, keyName string) (*bodyapi.Client, error) {
		c, _, remote, err := bodyClient(bodyFlags(t, "body-url", ts.URL, "body-secret-file", secretFile, "body-token-key-file", keyFile,
			"body-ca-file", filepath.Join(set, "ca.pem"), "body-client-cert-file", filepath.Join(set, certName), "body-client-key-file", filepath.Join(set, keyName)))
		if err == nil && !remote {
			t.Fatal("not remote")
		}
		return c, err
	}

	good, err := clientFor(bodySet, "api-client.pem", "api-client.key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := good.Stats(ctx); err != nil {
		t.Fatalf("the right certificates: %v", err)
	}
	// The web link's CA and certificates open nothing here: not as the CA to trust, not as the client.
	if other, err := clientFor(webSet, "web.pem", "web.key"); err != nil {
		t.Fatal(err)
	} else if _, err := other.Stats(ctx); !errors.Is(err, bodyapi.ErrUnavailable) {
		t.Errorf("certificates of another link: %v", err)
	}
	// Plain HTTP to a TLS service, and a client without a certificate, fail too.
	plain, _ := bodyapi.New(strings.Replace(ts.URL, "https://", "http://", 1), bodyapi.HTTPClient(nil), strings.Repeat("d", 40), bodyapi.NewSigner(priv))
	if _, err := plain.Stats(ctx); err == nil {
		t.Error("plain HTTP to a mutual TLS service was answered")
	}
	trust, _ := mtls.ClientConfig(filepath.Join(bodySet, "ca.pem"), filepath.Join(bodySet, "api-client.pem"), filepath.Join(bodySet, "api-client.key"), "")
	trust.Certificates, trust.GetClientCertificate = nil, nil // trusts the service, but has no certificate to show
	noCert, _ := bodyapi.New(ts.URL, bodyapi.HTTPClient(trust), strings.Repeat("d", 40), bodyapi.NewSigner(priv))
	if _, err := noCert.Stats(ctx); err == nil {
		t.Error("a client without a certificate was answered")
	}
}

// db rekey --body rotates the key of a body database of its own, and the main database of such a
// deployment is kept free of body tables by --without-body.
func TestRekeyBodyAndWithoutBodyOnTheCommandLine(t *testing.T) {
	t.Cleanup(func() { // cobra keeps flag values between runs; later tests must not see these
		dbRekeyCmd.Flags().Set("body", "false")
		dbRekeyCmd.Flags().Set("dry-run", "false")
		dbRekeyCmd.Flags().Set("without-body", "false")
		dbMigrateCmd.Flags().Set("without-body", "false")
	})
	dir := t.TempDir()
	bodyDSN := "file:" + filepath.Join(dir, "body.db")
	mainDSN := "file:" + filepath.Join(dir, "main.db")
	oldKey, newKey := strings.Repeat("33", 32), strings.Repeat("44", 32)
	ctx := context.Background()

	k, _ := sqlstore.ParseKey(oldKey)
	c, _ := sqlstore.NewCipher(k)
	bs, err := sqlstore.OpenBody(ctx, sqlstore.DriverSQLite, bodyDSN, sqlstore.WithCipher(c))
	if err != nil {
		t.Fatal(err)
	}
	w := 80.0
	if _, err := bs.UpsertScaleReadings(ctx, 1, []models.ScaleReading{{MeasuredAt: time.Now(), WeightKg: &w, Notes: "n"}}); err != nil {
		t.Fatal(err)
	}
	bs.Close()

	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		RootCmd.SetOut(&out)
		RootCmd.SetArgs(args)
		err := RootCmd.ExecuteContext(ctx)
		return out.String(), err
	}
	t.Setenv(encryptionKeyEnv, newKey)
	t.Setenv(encryptionKeyPreviousEnv, oldKey)
	none := filepath.Join(dir, "none.key")
	out, err := run("db", "rekey", "--body", "--dsn", bodyDSN, "--encryption-key-file", none, "--dry-run")
	if err != nil || !strings.Contains(out, "scale readings and DEXA scans to re-encrypt: 1") {
		t.Fatalf("dry run: %q %v", out, err)
	}
	if out, err = run("db", "rekey", "--body", "--dsn", bodyDSN, "--encryption-key-file", none, "--dry-run=false"); err != nil || !strings.Contains(out, "re-encrypted: 1") {
		t.Fatalf("rekey: %q %v", out, err)
	}
	if out, _ = run("db", "rekey", "--body", "--dsn", bodyDSN, "--encryption-key-file", none, "--dry-run=false"); !strings.Contains(out, "re-encrypted: 0") || !strings.Contains(out, "already under the current key: 1") {
		t.Fatalf("second rekey: %q", out)
	}
	if _, err := os.Stat(none); !os.IsNotExist(err) {
		t.Fatal("a key file was created")
	}
	t.Setenv(encryptionKeyPreviousEnv, "")
	nk, _ := sqlstore.ParseKey(newKey)
	nc, _ := sqlstore.NewCipher(nk)
	only, err := sqlstore.OpenBody(ctx, sqlstore.DriverSQLite, bodyDSN, sqlstore.WithCipher(nc))
	if err != nil {
		t.Fatalf("the new key alone: %v", err)
	}
	if rs, err := only.ListScaleReadings(ctx, 1); err != nil || len(rs) != 1 || rs[0].Notes != "n" {
		t.Fatalf("after rotation: %v %v", rs, err)
	}
	only.Close()

	// db migrate on the main database of a deployment with a body service must not create the tables.
	if _, err := run("db", "migrate", "up", "--dsn", mainDSN, "--without-body"); err != nil {
		t.Fatal(err)
	}
	main, err := sqlstore.Open(ctx, sqlstore.DriverSQLite, mainDSN, sqlstore.WithoutMigrate(), sqlstore.WithoutBody())
	if err != nil {
		t.Fatal(err)
	}
	defer main.Close()
	if v, err := main.SchemaVersion(ctx); err != nil || v == 0 {
		t.Fatalf("the main database was not migrated: %d %v", v, err)
	}
	withBody, err := sqlstore.Open(ctx, sqlstore.DriverSQLite, mainDSN, sqlstore.WithoutMigrate())
	if err != nil {
		t.Fatal(err)
	}
	defer withBody.Close()
	if pending, err := withBody.BodyStore.HasPending(ctx); err != nil || !pending {
		t.Errorf("db migrate --without-body created the body tables (pending = %v, %v)", pending, err)
	}
}
