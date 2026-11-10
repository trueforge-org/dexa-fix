package shared

import (
	"os/exec"
	"strings"
	"testing"
)

// pkg/shared is what the frontend and the backend have in common, so it can depend on neither of
// them, nor on the store. Test files do not count.
func TestSharedDoesNotDependOnEitherSide(t *testing.T) {
	const module = "github.com/trueforge-org/dexafix/"
	forbidden := []string{module + "pkg/frontend", module + "pkg/backend", module + "pkg/app", module + "pkg/store", module + "pkg/workers", "github.com/valkey-io"}
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", "./...").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	deps := strings.Fields(string(out))
	if len(deps) < 10 {
		t.Fatalf("suspiciously few dependencies: %v", deps)
	}
	for _, d := range deps {
		for _, f := range forbidden {
			if d == f || strings.HasPrefix(d, f+"/") {
				t.Errorf("pkg/shared depends on %s", d)
			}
		}
	}
}
