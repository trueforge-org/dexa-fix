package mainbackend

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
)

func TestAResetLinkIsBuiltFromTheBaseURLNeverFromAnEmailedHostHeader(t *testing.T) {
	g := newRig(t)
	req := httptest.NewRequest("GET", "http://attacker.example/", nil)

	if link, fromRequest := g.b.resetURL(req, "tok", true); link != "" || !fromRequest {
		t.Fatalf("an e-mailed link without a base URL: %q, fromRequest %v", link, fromRequest)
	}
	if link, fromRequest := g.b.resetURL(req, "tok", false); !strings.HasPrefix(link, "http://attacker.example") || !fromRequest {
		t.Fatalf("a link shown to the administrator: %q, fromRequest %v", link, fromRequest)
	}
	base, _ := url.Parse("https://dexa.example.com")
	l := *g.b.cfg()
	l.baseURL = base
	g.b.swapSettings(&l)
	for _, emailed := range []bool{true, false} {
		link, fromRequest := g.b.resetURL(req, "a b", emailed)
		if link != "https://dexa.example.com/password/reset?t=a+b" || fromRequest {
			t.Fatalf("emailed %v: %q, fromRequest %v", emailed, link, fromRequest)
		}
	}
}

// failingMailer is a mail server that refuses everything.
type failingMailer struct{}

func (failingMailer) Send(context.Context, string, string, string) error {
	return errors.New("the mail server said no")
}

func TestAResetMailThatCannotBeSentIsReportedAndLogged(t *testing.T) {
	g := newRig(t)
	uid, _ := g.signUp("alice")
	l := *g.b.cfg()
	l.mailer = failingMailer{}
	g.b.swapSettings(&l)
	u, err := g.st.GetUser(context.Background(), uid)
	if err != nil {
		t.Fatal(err)
	}
	logs := captureSignInLog(t)
	if g.b.sendResetMail(u, "https://dexa.example.com/password/reset?t=x", time.Hour) {
		t.Fatal("a mail that failed was reported as sent")
	}
	if !strings.Contains(logs.String(), "could not send the password reset e-mail") {
		t.Fatalf("log %s", logs)
	}
	if g.b.sendResetMail(models.User{ID: uid, Username: "no-address"}, "x", time.Hour) {
		t.Fatal("a mail was sent to an account without an address")
	}
}

func TestResetLinksThatCannotBeRemovedAreLogged(t *testing.T) {
	g := newRig(t)
	uid, _ := g.signUp("alice")
	breakTable(t, g.st, "password_resets")
	logs := captureSignInLog(t)
	g.b.forgetResets(context.Background(), uid)
	if !strings.Contains(logs.String(), "could not remove password reset links") {
		t.Fatalf("log %s", logs)
	}
}
