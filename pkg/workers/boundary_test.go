package workers

import (
	"os/exec"
	"strings"
	"testing"
)

// Workers talk to outside services for the backend and have no database: they know the backend only
// through pkg/shared/workerapi. So they must not depend on the backend, the frontend, the store or
// any database library. Test files do not count.
func TestWorkersDoNotDependOnTheDatabaseOrEitherSide(t *testing.T) {
	const module = "github.com/trueforge-org/dexa-fix/"
	forbidden := []string{
		module + "pkg/frontend", module + "pkg/backend", module + "pkg/app", module + "pkg/store",
		"database/sql", "github.com/uptrace/bun", "github.com/jackc/pgx", "modernc.org/sqlite", "github.com/pressly/goose",
	}
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
				t.Errorf("the workers depend on %s", d)
			}
		}
	}
}
