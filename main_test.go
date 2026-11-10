package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestServiceFromArgs(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{nil, ""},
		{[]string{"db", "migrate"}, ""},
		{[]string{"user", "list"}, ""},
		{[]string{"secrets", "init", "--host-dir=/x"}, ""},
		{[]string{"serve"}, "dexafix"},
		{[]string{"serve", "--listen=:8080"}, "dexafix"},
		{[]string{"serve", "web"}, "web"},
		{[]string{"serve", "api", "--listen="}, "api"},
		{[]string{"serve", "--health-listen", "127.0.0.1:8083", "body"}, "body"},
		{[]string{"serve", "worker", "--providers", "withings"}, "worker"},
		{[]string{"serve", "nonsense"}, "dexafix"},
	} {
		if got := serviceFromArgs(c.args); got != c.want {
			t.Errorf("serviceFromArgs(%v) = %q, want %q", c.args, got, c.want)
		}
	}
}

func TestLogFormatAndBanner(t *testing.T) {
	for in, want := range map[string]string{"": "console", "console": "console", "text": "console", "json": "json", " JSON ": "json"} {
		if got := logFormat(in); got != want {
			t.Errorf("logFormat(%q) = %q, want %q", in, got, want)
		}
	}
	if got := banner("1.2.3", "api"); got != "DexaFix 1.2.3 · api" {
		t.Errorf("banner: %q", got)
	}
	if got := banner("1.2.3", ""); got != "DexaFix 1.2.3" {
		t.Errorf("banner without a service: %q", got)
	}
	if got := banner("1.2.3", "dexafix"); got != "DexaFix 1.2.3" {
		t.Errorf("banner of the combined server: %q", got)
	}
}

func TestConsoleLoggerIsPlainOutsideATerminal(t *testing.T) {
	var buf bytes.Buffer
	l := newLogger(&buf, formatConsole, false, false, "api")
	l.Info().Str("public", ":8080").Msg("serving the backend")
	out := buf.String()
	if strings.Contains(out, "\x1b[") {
		t.Errorf("colour codes in a log that is not a terminal: %q", out)
	}
	for _, want := range []string{"INF", "api │ serving the backend", "public=:8080", "T"} { // T: a full RFC 3339 time
		if !strings.Contains(out, want) {
			t.Errorf("%q is missing from %q", want, out)
		}
	}
}

func TestConsoleLoggerOnATerminalHasColourAndTheTimeOfDay(t *testing.T) {
	var buf bytes.Buffer
	l := newLogger(&buf, formatConsole, true, true, "web")
	l.Warn().Msg("careful")
	out := buf.String()
	if !strings.Contains(out, "\x1b[") {
		t.Errorf("no colour on a terminal: %q", out)
	}
	if strings.Contains(out, "T") && strings.Contains(out, "Z ") {
		t.Errorf("a terminal wants the time of day, not a full timestamp: %q", out)
	}
	if !strings.Contains(out, "web │ careful") {
		t.Errorf("service missing: %q", out)
	}
}

func TestCombinedServerAndOtherCommandsHaveNoServicePrefix(t *testing.T) {
	for _, service := range []string{"", "dexafix"} {
		var buf bytes.Buffer
		l := newLogger(&buf, formatConsole, false, false, service)
		l.Info().Msg("serving")
		if strings.Contains(buf.String(), "│") {
			t.Errorf("service %q: %q", service, buf.String())
		}
	}
}

func TestJSONLoggerWritesOneObjectPerLineWithTheService(t *testing.T) {
	var buf bytes.Buffer
	l := newLogger(&buf, formatJSON, false, false, "worker")
	l.Info().Str("id", "w1").Msg("worker registered")
	l.Error().Msg("")
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %q", buf.String())
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &m); err != nil {
		t.Fatalf("not JSON: %v: %q", err, lines[0])
	}
	if m["service"] != "worker" || m["message"] != "worker registered" || m["level"] != "info" || m["id"] != "w1" || m["time"] == nil {
		t.Errorf("fields: %v", m)
	}
	// Without a service (db, user, ...) there is no service field.
	buf.Reset()
	plain := newLogger(&buf, formatJSON, false, false, "")
	plain.Info().Msg("x")
	if strings.Contains(buf.String(), "service") {
		t.Errorf("service field without a service: %q", buf.String())
	}
}
