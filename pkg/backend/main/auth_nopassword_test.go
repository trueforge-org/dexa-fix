package mainbackend

import (
	"context"
	"net/http"
	"testing"

	"github.com/trueforge-org/dexafix/pkg/models"
)

func TestAnAccountWithoutAPasswordAnswersLikeAnUnknownOne(t *testing.T) {
	g := newRig(t)
	uid, _ := g.signUp("alice")
	if err := g.st.DeleteIdentity(context.Background(), uid, models.ProviderLocal); err != nil {
		t.Fatal(err)
	}
	var known, unknown struct {
		Error  string
		Status int
	}
	g.signIn("alice").decode(t, &known)
	g.signIn("nobody").decode(t, &unknown)
	if known != unknown || known.Status != http.StatusUnauthorized {
		t.Fatalf("an account without a password answers %+v, an unknown one %+v", known, unknown)
	}
}
