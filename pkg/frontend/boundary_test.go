package frontend

import (
	"os/exec"
	"strings"
	"testing"
)

// The frontend has no database, no keys and no security decisions of its own: it asks the main
// backend through the internal API, whose contract is pkg/shared/apiclient. This test makes the
// rule fail the build instead of relying on review: no package the frontend is built from (test
// files do not count) may be a backend, the store, a worker, or a library for databases, OIDC or
// WebAuthn.

const module = "github.com/trueforge-org/dexa-fix/"

var forbidden = []string{
	module + "pkg/backend", // every backend, including pkg/backend/main
	module + "pkg/app",
	module + "pkg/workers",
	module + "pkg/store", // incl. sqlstore
	module + "pkg/mailer",
	"github.com/uptrace/bun",
	"modernc.org/sqlite",
	"github.com/jackc/pgx",
	"github.com/pressly/goose",
	"github.com/coreos/go-oidc",
	"github.com/go-webauthn",
}

func TestFrontendDoesNotDependOnTheBackendOrTheStore(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", ".").Output()
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
				t.Errorf("the frontend depends on %s (forbidden: %s)", d, f)
			}
		}
	}
}
