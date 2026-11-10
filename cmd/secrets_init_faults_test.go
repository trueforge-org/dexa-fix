package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every file init makes, once, so that a test can spoil each of them in turn.
func initFiles(t *testing.T) (all []struct{ key, name string }) {
	t.Helper()
	o, dirs := testInit(t)
	if err := o.run(); err != nil {
		t.Fatal(err)
	}
	for key, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			all = append(all, struct{ key, name string }{key, e.Name()})
		}
	}
	return all
}

// A folder where a file belongs cannot be read as a secret and cannot be replaced by one: init stops
// and names the file, whichever of its files it is, instead of going on with half a set.
func TestSecretsInitStopsAndNamesAFileThatIsAFolder(t *testing.T) {
	for _, f := range initFiles(t) {
		t.Run(f.key+"/"+f.name, func(t *testing.T) {
			o, dirs := testInit(t)
			spoiled := filepath.Join(dirs[f.key], f.name)
			if err := os.MkdirAll(filepath.Join(spoiled, "inside"), 0o700); err != nil {
				t.Fatal(err)
			}
			err := o.run()
			if err == nil || !strings.Contains(err.Error(), f.name) {
				t.Fatalf("want an error that names %s, got %v", f.name, err)
			}
		})
	}
}

func TestSecretsInitStopsWhenAFolderCannotBeMade(t *testing.T) {
	o, _ := testInit(t)
	file := filepath.Join(t.TempDir(), "in-the-way")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	o.hostDir = filepath.Join(file, "host")
	if err := o.run(); err == nil {
		t.Fatal("a folder below a file was made")
	}
}

func TestSecretsInitStopsWhenTheBodyTokenKeyIsDamaged(t *testing.T) {
	o, dirs := testInit(t)
	if err := o.run(); err != nil {
		t.Fatal(err)
	}
	replaceFile(t, filepath.Join(dirs["api"], fileBodyTokenKey), "not a key\n", 0o400)
	if err := o.run(); err == nil {
		t.Fatal("a damaged signing key was accepted")
	}
}

func TestSecretsInitStopsWhenTheCertificatesCannotBeMade(t *testing.T) {
	o, _ := testInit(t)
	o.validity = 0
	if err := o.run(); err == nil || !strings.Contains(err.Error(), "validity") {
		t.Fatalf("%v", err)
	}
}

func TestSecretsInitSaysWhatItDoes(t *testing.T) {
	o, dirs := testInit(t)
	var log bytes.Buffer
	o.log = &log
	if err := o.run(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "created   "+filepath.Join(dirs["host"], "encryption_key")) {
		t.Errorf("the first run did not say what it created:\n%s", log.String())
	}
	log.Reset()
	if err := o.run(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "kept      "+filepath.Join(dirs["host"], "encryption_key")) ||
		!strings.Contains(log.String(), "replaced  "+filepath.Join(dirs["api"], fileMTLSCert)) {
		t.Errorf("the second run did not say what it kept and what it replaced:\n%s", log.String())
	}
}

func TestSecretsInitFilesCannotBeWrittenInAFolderThatIsGone(t *testing.T) {
	o, _ := testInit(t)
	gone := filepath.Join(t.TempDir(), "gone")
	if _, err := o.keep(gone, "x", false, func() string { return "v" }); err == nil {
		t.Error("keep wrote into a folder that does not exist")
	}
	if err := o.put(gone, "x", "v"); err == nil {
		t.Error("put wrote into a folder that does not exist")
	}
	if err := o.share(gone, "x", "v"); err == nil {
		t.Error("share wrote into a folder that does not exist")
	}
}

func TestSecretsInitSharesNothingWithAFolderThatHoldsAFolderInsteadOfTheFile(t *testing.T) {
	o, _ := testInit(t)
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := o.share(dir, "x", "v"); err == nil {
		t.Error("a folder was read as the file")
	}
	if err := o.share("", "x", "v"); err != nil {
		t.Errorf("an empty folder name means: skip. %v", err)
	}
}

func TestSecretsInitAsRootStopsWhenItCannotGiveAFolderAway(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("giving files away needs root")
	}
	asEuid(t, 0)
	o, _ := testInit(t)
	o.uid, o.gid = 12345, 12345
	if err := o.adopt(filepath.Join(t.TempDir(), "gone")); err == nil || !strings.Contains(err.Error(), "give") {
		t.Errorf("a folder that is not there: %v", err)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := o.adopt(file); err == nil {
		t.Error("a file was read as a folder")
	}
	if err := o.adopt(""); err != nil {
		t.Errorf("an empty name means: skip. %v", err)
	}
}

func TestSecretsInitCommandMakesTheFilesAndChecksItsFlags(t *testing.T) {
	root := t.TempDir()
	dir := func(n string) string { return filepath.Join(root, n) }
	args := []string{"secrets", "init", "--host-dir", dir("host"), "--api-dir", dir("api"), "--web-dir", dir("web"),
		"--worker-dir", dir("worker"), "--body-dir", dir("body"), "--db-dir", dir("db"), "--dbsetup-dir", dir("dbsetup"),
		"--valkey-dir", dir("valkey"), "--db-host", "postgres", "--mtls-days", "30"}
	_, errText, err := run(t, args...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errText, "created") {
		t.Errorf("the command said nothing about what it created: %q", errText)
	}
	if dsn := readFile(t, dir("host"), "dsn"); !strings.Contains(dsn, "@postgres:5432/") {
		t.Errorf("the connection string ignores --db-host: %q", dsn)
	}
	if _, _, err := run(t, append(args, "--mtls-days", "0")...); err == nil || !strings.Contains(err.Error(), "--mtls-days") {
		t.Errorf("zero days of validity: %v", err)
	}
	if _, _, err := run(t, append(args, "--chown", "x")...); err == nil || !strings.Contains(err.Error(), "--chown") {
		t.Errorf("an owner that is not one: %v", err)
	}
}
