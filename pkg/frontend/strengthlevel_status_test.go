package frontend

import (
	"net/http"
	"strings"
	"testing"
)

func TestTheStrengthLevelProgressBlockPollsWhileASyncRuns(t *testing.T) {
	fs := newFaultStack(t)
	fs.fail(answering(http.StatusOK, `{"Running":true,"Text":"Sending day 3 of 10","Done":3,"Total":10}`), "GET", "/users/1/strengthlevel/status")
	res := fs.get("/u/1/strengthlevel/status")
	if res.code != http.StatusOK || !strings.HasPrefix(res.header.Get("Content-Type"), "text/html") ||
		!strings.Contains(res.body, "Sending day 3 of 10") || !strings.Contains(res.body, `hx-trigger="every 2s"`) {
		t.Fatalf("%d %v %q", res.code, res.header, res.body)
	}
}

func TestTheStrengthLevelProgressBlockStopsPollingWhenThereIsNoSync(t *testing.T) {
	fs := newFaultStack(t)
	fs.fail(answering(http.StatusOK, `null`), "GET", "/users/1/strengthlevel/status")
	res := fs.get("/u/1/strengthlevel/status")
	if res.code != http.StatusOK || strings.Contains(res.body, "hx-trigger") {
		t.Fatalf("%d %q", res.code, res.body)
	}
}

func TestTheAdminPageDoesNotOpenWhenTheStepUpCheckFails(t *testing.T) {
	fs := newFaultStack(t)
	fs.fail(serverError, "GET", "/admin/step-up")
	if res := fs.get("/admin"); res.code != http.StatusServiceUnavailable {
		t.Fatalf("%d %q", res.code, res.body)
	}
}
