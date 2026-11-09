package mainbackend

import (
	"strings"
	"testing"
)

func TestDocsPublic(t *testing.T) {
	ts, c := newTestServer(t)
	code, page := body(t, c, ts.URL+"/docs")
	if code != 200 {
		t.Fatalf("GET /docs signed out: %d", code)
	}
	for _, want := range []string{`id="toc"`, `href="#getting-started"`, `id="server-config"`, `href="/docs"`} {
		if !strings.Contains(page, want) {
			t.Errorf("signed-out docs page lacks %s", want)
		}
	}
	if strings.Contains(page, `class="user-menu"`) {
		t.Error("signed-out docs page shows the user menu")
	}

	signup(t, ts, c, "alice")
	code, page = body(t, c, ts.URL+"/docs")
	if code != 200 {
		t.Fatalf("GET /docs signed in: %d", code)
	}
	for _, want := range []string{`id="toc"`, `class="user-menu"`, "alice", `href="/docs"`} {
		if !strings.Contains(page, want) {
			t.Errorf("signed-in docs page lacks %s", want)
		}
	}
}

func TestDocsLinkOnSignInPage(t *testing.T) {
	ts, c := newTestServer(t)
	_, page := body(t, c, ts.URL+"/")
	if !strings.Contains(page, `href="/docs"`) {
		t.Error("sign-in page has no Docs link")
	}
}
