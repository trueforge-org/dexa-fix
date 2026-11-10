package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/shared/mtls"
)

func testInit(t *testing.T) (initOptions, map[string]string) {
	t.Helper()
	root := t.TempDir()
	dirs := map[string]string{}
	for _, n := range []string{"host", "api", "web", "worker", "body", "db", "dbsetup", "valkey"} {
		dirs[n] = filepath.Join(root, n)
	}
	return initOptions{
		hostDir: dirs["host"], apiDir: dirs["api"], webDir: dirs["web"], workerDir: dirs["worker"], bodyDir: dirs["body"],
		dbDir: dirs["db"], dbsetupDir: dirs["dbsetup"], valkeyDir: dirs["valkey"],
		dbHost: "db", valkeyHost: "valkey", apiNames: []string{"api"}, bodyNames: []string{"body"}, validity: 30 * 24 * time.Hour, uid: -1, gid: -1,
	}, dirs
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func mode(t *testing.T, dir, name string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// Compose has no secrets: block, so a plain `docker compose up` works: every container reads what it
// needs from a volume of its own, and only that.
func TestSecretsInitFillsTheVolumes(t *testing.T) {
	o, d := testInit(t)
	if err := o.run(); err != nil {
		t.Fatal(err)
	}
	host := func(n string) string { return readFile(t, d["host"], n) }
	same := map[string][][2]string{ // dir: {file in the host dir, file in the volume}
		"db":      {{"db_admin_password", "db_admin_password"}},
		"valkey":  {{"valkey_password", "valkey_password"}},
		"dbsetup": {{"db_password", "db_password"}, {"body_db_password", "body_db_password"}},
		"api":     {{"dsn", "dsn"}, {"encryption_key", "encryption_key"}},
		"body":    {{"body_dsn", "body_dsn"}, {"body_encryption_key", "body_encryption_key"}},
	}
	for dir, files := range same {
		for _, f := range files {
			if got := readFile(t, d[dir], f[1]); got != host(f[0]) {
				t.Errorf("%s/%s differs from the host file %s", dir, f[1], f[0])
			}
			if got := mode(t, d[dir], f[1]); got != 0o400 {
				t.Errorf("%s/%s mode %o, want 0400", dir, f[1], got)
			}
		}
	}
	// db-setup connects as the superuser to the maintenance database; the roles do not exist yet.
	if got, want := readFile(t, d["dbsetup"], "admin_dsn"), "postgres://postgres:"+host("db_admin_password")+"@db:5432/postgres?sslmode=disable"; got != want {
		t.Errorf("admin_dsn %q, want %q", got, want)
	}
	// Nothing reaches a container that does not need it.
	for dir, banned := range map[string][]string{
		"db":      {"db_password", "body_db_password", "encryption_key", "body_encryption_key", "admin_dsn", "valkey_password"},
		"valkey":  {"db_admin_password", "db_password", "encryption_key", "dsn"},
		"dbsetup": {"encryption_key", "body_encryption_key", "valkey_password", "dsn", "body_dsn"},
		"api":     {"db_admin_password", "admin_dsn", "body_dsn", "body_encryption_key"},
		"body":    {"db_admin_password", "admin_dsn", "dsn", "encryption_key"},
		"web":     {"dsn", "encryption_key", "body_dsn", "db_admin_password"},
		"worker":  {"dsn", "encryption_key", "body_dsn", "db_admin_password"},
	} {
		for _, n := range banned {
			if _, err := os.Stat(filepath.Join(d[dir], n)); err == nil {
				t.Errorf("%s/%s must not exist", dir, n)
			}
		}
	}

	// A password changed by hand on the host reaches the volumes at the next run.
	if err := os.WriteFile(filepath.Join(d["host"], "db_admin_password"), []byte(strings.Repeat("a", 40)+"\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := o.run(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, d["db"], "db_admin_password"); got != strings.Repeat("a", 40) {
		t.Errorf("db/db_admin_password %q did not follow the host file", got)
	}
	if !strings.Contains(readFile(t, d["dbsetup"], "admin_dsn"), ":"+strings.Repeat("a", 40)+"@") {
		t.Error("admin_dsn did not follow the host file")
	}
}

func TestSecretsInitCreatesEverything(t *testing.T) {
	o, d := testInit(t)
	if err := o.run(); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"db_admin_password", "db_password", "dsn", "encryption_key", "body_db_password", "body_dsn", "body_encryption_key", "valkey_password"} {
		if got := mode(t, d["host"], n); got != 0o444 {
			t.Errorf("host/%s mode %o, want 0444 (the containers run as other users)", n, got)
		}
	}
	if dsn := readFile(t, d["host"], "dsn"); !strings.HasPrefix(dsn, "postgres://dexafix:"+readFile(t, d["host"], "db_password")+"@db:5432/dexafix") {
		t.Errorf("dsn %q does not use the password on disk", dsn)
	}
	if dsn := readFile(t, d["host"], "body_dsn"); !strings.Contains(dsn, "dexafix_body:") || !strings.Contains(dsn, "search_path=body") {
		t.Errorf("body_dsn %q", dsn)
	}
	// The optional Valkey server: the password is kept on the host, the URL (with the password in it)
	// is for the api process only.
	if url := readFile(t, d["api"], fileValkeyURL); url != "redis://:"+readFile(t, d["host"], "valkey_password")+"@valkey:6379/0" {
		t.Errorf("valkey_url %q does not follow the password", url)
	}
	if got := mode(t, d["api"], fileValkeyURL); got != 0o400 {
		t.Errorf("valkey_url mode %o, want 0400", got)
	}
	for _, dir := range []string{"web", "worker", "body"} {
		if _, err := os.Stat(filepath.Join(d[dir], fileValkeyURL)); err == nil {
			t.Errorf("%s must not get the Valkey URL", dir)
		}
	}
	// The two doors that two processes share carry the same secret in both places.
	if readFile(t, d["api"], fileServiceSecret) != readFile(t, d["web"], fileServiceSecret) ||
		readFile(t, d["api"], fileWorkerSecret) != readFile(t, d["worker"], fileWorkerSecret) ||
		readFile(t, d["api"], fileBodySecret) != readFile(t, d["body"], fileBodySecret) {
		t.Error("a shared secret differs between its two processes")
	}
	if a, b, c := readFile(t, d["api"], fileServiceSecret), readFile(t, d["api"], fileWorkerSecret), readFile(t, d["api"], fileBodySecret); a == b || b == c || a == c {
		t.Error("the three doors must have three different secrets")
	}
	// Isolation: each process gets only what it needs.
	for dir, banned := range map[string][]string{
		"web":    {fileWorkerSecret, fileBodySecret, fileBodyTokenKey, fileBodyTokenPub},
		"worker": {fileServiceSecret, fileBodySecret, fileBodyTokenKey, fileBodyTokenPub},
		"body":   {fileServiceSecret, fileWorkerSecret, fileBodyTokenKey},
	} {
		for _, n := range banned {
			if _, err := os.Stat(filepath.Join(d[dir], n)); err == nil {
				t.Errorf("%s/%s must not exist", dir, n)
			}
		}
	}
	if pub := readFile(t, d["body"], fileBodyTokenPub); pub == "" || pub == readFile(t, d["api"], fileBodyTokenKey) {
		t.Error("the body service must get the public key, never the private one")
	}
	for dir, names := range map[string][]string{
		"api":    {fileServiceSecret, fileMTLSKey, fileBodyMTLSKey, fileBodyTokenKey},
		"web":    {fileMTLSKey},
		"worker": {fileMTLSKey},
		"body":   {fileMTLSKey, fileBodyTokenPub},
	} {
		for _, n := range names {
			if got := mode(t, d[dir], n); got != 0o400 {
				t.Errorf("%s/%s mode %o, want 0400", dir, n, got)
			}
		}
	}
	// The api's server key is for the api only; web and workers hold a client key.
	if readFile(t, d["api"], fileMTLSKey) == readFile(t, d["web"], fileMTLSKey) {
		t.Error("the web process must not get the api's private key")
	}
	if readFile(t, d["web"], fileMTLSKey) != readFile(t, d["worker"], fileMTLSKey) {
		t.Error("web and workers share one client certificate")
	}
	// Each link has a CA of its own.
	if readFile(t, d["api"], fileMTLSCA) == readFile(t, d["body"], fileMTLSCA) {
		t.Error("the web link and the body link must not share a CA")
	}
	// Leftover temporary files are never left behind.
	for _, dir := range d {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".") {
				t.Errorf("%s/%s left behind", dir, e.Name())
			}
		}
	}
}

// The certificates it writes are the ones the processes load, and they work together.
func TestSecretsInitCertificatesWork(t *testing.T) {
	o, d := testInit(t)
	if err := o.run(); err != nil {
		t.Fatal(err)
	}
	p := func(dir, name string) string { return filepath.Join(d[dir], name) }
	if _, err := mtls.ServerConfig(p("api", fileMTLSCert), p("api", fileMTLSKey), p("api", fileMTLSCA)); err != nil {
		t.Errorf("api server: %v", err)
	}
	if _, err := mtls.ClientConfig(p("web", fileMTLSCA), p("web", fileMTLSCert), p("web", fileMTLSKey), "api"); err != nil {
		t.Errorf("web client: %v", err)
	}
	if _, err := mtls.ServerConfig(p("body", fileMTLSCert), p("body", fileMTLSKey), p("body", fileMTLSCA)); err != nil {
		t.Errorf("body server: %v", err)
	}
	if _, err := mtls.ClientConfig(p("api", fileBodyMTLSCA), p("api", fileBodyMTLSCert), p("api", fileBodyMTLSKey), "body"); err != nil {
		t.Errorf("api client of the body service: %v", err)
	}
}

// A second run keeps every password, key and shared secret, and replaces the certificates.
func TestSecretsInitKeepsSecretsAndRenewsCertificates(t *testing.T) {
	o, d := testInit(t)
	if err := o.run(); err != nil {
		t.Fatal(err)
	}
	keptHost := []string{"db_admin_password", "db_password", "dsn", "encryption_key", "body_db_password", "body_dsn", "body_encryption_key", "valkey_password"}
	before := map[string]string{}
	for _, n := range keptHost {
		before["host/"+n] = readFile(t, d["host"], n)
	}
	for dir, names := range map[string][]string{
		"api": {fileServiceSecret, fileWorkerSecret, fileBodySecret, fileBodyTokenKey, fileValkeyURL},
		"web": {fileServiceSecret}, "worker": {fileWorkerSecret}, "body": {fileBodySecret, fileBodyTokenPub},
	} {
		for _, n := range names {
			before[dir+"/"+n] = readFile(t, d[dir], n)
		}
	}
	oldCA, oldKey := readFile(t, d["api"], fileMTLSCA), readFile(t, d["web"], fileMTLSKey)

	if err := o.run(); err != nil {
		t.Fatal(err)
	}
	for k, v := range before {
		dir, name, _ := strings.Cut(k, "/")
		if got := readFile(t, d[dir], name); got != v {
			t.Errorf("%s changed on the second run", k)
		}
	}
	if readFile(t, d["api"], fileMTLSCA) == oldCA || readFile(t, d["web"], fileMTLSKey) == oldKey {
		t.Error("the certificates must be new at every run")
	}
	// And the new set still works together.
	p := func(dir, name string) string { return filepath.Join(d[dir], name) }
	if _, err := mtls.ClientConfig(p("web", fileMTLSCA), p("web", fileMTLSCert), p("web", fileMTLSKey), "api"); err != nil {
		t.Errorf("renewed web client: %v", err)
	}
}

// A password put there by hand is used, and the connection string follows it.
func TestSecretsInitUsesPasswordsThatExist(t *testing.T) {
	o, d := testInit(t)
	if err := os.MkdirAll(d["host"], 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d["host"], "db_password"), []byte("handmade\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := o.run(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, d["host"], "dsn"); !strings.Contains(got, "dexafix:handmade@db") {
		t.Errorf("dsn %q", got)
	}
}

func TestSecretsInitRefusesBadInput(t *testing.T) {
	o, _ := testInit(t)
	o.hostDir = ""
	if err := o.run(); err == nil {
		t.Error("--host-dir is required")
	}
	o, _ = testInit(t)
	o.apiDir = ""
	if err := o.run(); err == nil {
		t.Error("the other directories need --api-dir")
	}
	// Two doors with one secret are refused instead of quietly opening each other's door.
	o, d := testInit(t)
	if err := o.run(); err != nil {
		t.Fatal(err)
	}
	same := readFile(t, d["api"], fileServiceSecret) + "\n"
	if err := os.WriteFile(filepath.Join(d["api"], fileWorkerSecret), []byte(same), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := o.run(); err == nil || !strings.Contains(err.Error(), "differ") {
		t.Errorf("equal secrets must be refused, got %v", err)
	}
	for _, bad := range []string{"x", "568:x", "-1"} {
		if _, _, err := parseOwner(bad); err == nil {
			t.Errorf("--chown %q must be refused", bad)
		}
	}
	if u, g, err := parseOwner("568"); err != nil || u != 568 || g != 568 {
		t.Errorf("parseOwner(568) = %d %d %v", u, g, err)
	}
}
