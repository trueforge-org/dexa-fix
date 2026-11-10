package cmd

import (
	"slices"
	"testing"
)

func TestIgnoredWorkerEnvSkipsOnlyTheImageDefault(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if got := ignoredWorkerEnv(env(map[string]string{"DEXAFIX_ENCRYPTION_KEY_FILE": "/config/dexafix.key"})); len(got) != 0 {
		t.Errorf("the image's own default must not be reported: %v", got)
	}
	got := ignoredWorkerEnv(env(map[string]string{
		"DEXAFIX_ENCRYPTION_KEY_FILE": "/secrets/api/encryption_key", // a real key given to a worker
		"DEXAFIX_SERVICE_SECRET_FILE": "/secrets/web/service_secret",
		"DEXAFIX_ENCRYPTION_KEY":      "hex",
	}))
	want := []string{"DEXAFIX_ENCRYPTION_KEY", "DEXAFIX_ENCRYPTION_KEY_FILE", "DEXAFIX_SERVICE_SECRET_FILE"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got := ignoredWorkerEnv(env(nil)); len(got) != 0 {
		t.Errorf("nothing set: %v", got)
	}
}
