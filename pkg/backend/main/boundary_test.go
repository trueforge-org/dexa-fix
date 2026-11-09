package mainbackend

import (
	"os/exec"
	"strings"
	"testing"
)

// The backend never renders pages and does not import the frontend (its tests may, to drive the
// HTML flows; test files do not count here). It does not import the body service either: it reaches
// the scans, readings and estimates only through the contract in pkg/shared/bodyapi, so the two can
// run as separate processes.
func TestBackendDoesNotImportTheFrontend(t *testing.T) {
	const module = "github.com/trueforge-org/dexa-fix/"
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	for _, d := range strings.Fields(string(out)) {
		if d == module+"pkg/frontend" || strings.HasPrefix(d, module+"pkg/frontend/") || d == module+"pkg/app" ||
			d == module+"pkg/workers" || strings.HasPrefix(d, module+"pkg/workers/") ||
			d == module+"pkg/backend/body" {
			t.Errorf("the main backend depends on %s", d)
		}
	}
}
