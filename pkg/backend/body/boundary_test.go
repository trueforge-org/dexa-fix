package bodybackend

import (
	"os/exec"
	"strings"
	"testing"
)

// The body service knows nothing about people: no sign-in, no sessions, no e-mail, no other
// backend, no frontend and no workers. A token from the main backend is all it needs. (Its tests
// may use the store to build a service; test files do not count here.)
func TestBodyServiceStaysSmall(t *testing.T) {
	const module = "github.com/trueforge-org/dexa-fix/"
	forbidden := []string{
		module + "pkg/backend/main", module + "pkg/frontend", module + "pkg/workers", module + "pkg/app",
		module + "pkg/mailer", module + "pkg/shared/apiclient", module + "pkg/shared/workerapi",
		"github.com/coreos/go-oidc", "github.com/go-webauthn", "golang.org/x/crypto/bcrypt", "net/smtp",
	}
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
				t.Errorf("the body service depends on %s", d)
			}
		}
	}
}
