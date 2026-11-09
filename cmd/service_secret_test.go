package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSecretFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "service.secret")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadServiceSecrets(t *testing.T) {
	cur, prev := strings.Repeat("a", 40), strings.Repeat("b", 40)
	got, err := loadServiceSecrets(writeSecretFile(t, "# rotated 2026-11\n"+cur+"\n\n"+prev+"\n"))
	if err != nil || len(got) != 2 || got[0] != cur || got[1] != prev {
		t.Fatalf("%v %v", got, err)
	}
	for name, content := range map[string]string{
		"short":   "tooshort\n",
		"empty":   "\n# only a comment\n",
		"onshort": strings.Repeat("a", 40) + "\nshort\n",
	} {
		if _, err := loadServiceSecrets(writeSecretFile(t, content)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := loadServiceSecrets(""); err == nil {
		t.Error("empty path accepted")
	}
	if _, err := loadServiceSecrets(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing file accepted")
	}
}

func TestServiceSecretCommandPrintsAUsableSecret(t *testing.T) {
	var out bytes.Buffer
	serviceSecretCmd.SetOut(&out)
	if err := serviceSecretCmd.RunE(serviceSecretCmd, nil); err != nil {
		t.Fatal(err)
	}
	s := strings.TrimSpace(out.String())
	if len(s) != 64 {
		t.Fatalf("secret %q", s)
	}
	if _, err := loadServiceSecrets(writeSecretFile(t, s+"\n")); err != nil {
		t.Errorf("the printed secret is not accepted: %v", err)
	}
	var out2 bytes.Buffer
	serviceSecretCmd.SetOut(&out2)
	serviceSecretCmd.RunE(serviceSecretCmd, nil)
	if out.String() == out2.String() {
		t.Error("two runs printed the same secret")
	}
}
