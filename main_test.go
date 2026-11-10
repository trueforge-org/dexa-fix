package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
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

// runWith runs the program as `dexafix <args>` with the given environment and returns its exit code
// and what it wrote to standard output and standard error.
func runWith(t *testing.T, env map[string]string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	for k, v := range env {
		t.Setenv(k, v)
	}
	oldArgs, oldOut, oldErr, oldLogger, oldLevel := os.Args, os.Stdout, os.Stderr, log.Logger, zerolog.GlobalLevel()
	outFile, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	errFile, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	os.Args, os.Stdout, os.Stderr = append([]string{"dexafix"}, args...), outFile, errFile
	code = run()
	os.Args, os.Stdout, os.Stderr, log.Logger = oldArgs, oldOut, oldErr, oldLogger
	zerolog.SetGlobalLevel(oldLevel)
	read := func(f *os.File) string {
		f.Close()
		b, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	return code, read(outFile), read(errFile)
}

func TestACommandThatWorksExitsWithZeroAndShowsTheBanner(t *testing.T) {
	code, _, stderr := runWith(t, map[string]string{"LOGFORMAT": "console"}, "info")
	if code != 0 || !strings.Contains(stderr, "DexaFix") {
		t.Fatalf("exit code %d, stderr %q; want 0 and the banner", code, stderr)
	}
}

func TestACommandThatFailsExitsWithOneAndSaysWhy(t *testing.T) {
	code, _, stderr := runWith(t, map[string]string{"LOGFORMAT": "console"}, "no-such-command")
	if code != 1 || !strings.Contains(stderr, "no-such-command") {
		t.Fatalf("exit code %d, stderr %q; want 1 and the unknown command named", code, stderr)
	}
}

func TestAServiceThatFailsToStartLogsItAsJSONForACollector(t *testing.T) {
	// `serve web` without a backend URL stops at once; its start and its failure are what is logged.
	code, stdout, stderr := runWith(t, map[string]string{"LOGFORMAT": "json"}, "serve", "web")
	if code != 1 {
		t.Fatalf("exit code %d, want 1 for a web process without a backend; stderr %q", code, stderr)
	}
	var starting, failed bool
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("a log line that is not JSON: %q", line)
		}
		starting = starting || entry["message"] == "starting" && entry["service"] == "web"
		failed = failed || entry["message"] == "the command failed"
	}
	if !starting || !failed {
		t.Fatalf("starting=%v failed=%v in %q; want both logged", starting, failed, stdout)
	}
}

func TestMainEndsTheProcessWithTheExitCodeOfTheCommand(t *testing.T) {
	got := -1
	oldExit, oldArgs, oldErr := exit, os.Args, os.Stderr
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { exit, os.Args, os.Stderr = oldExit, oldArgs, oldErr; devnull.Close() }()
	exit = func(code int) { got = code }
	os.Args, os.Stderr = []string{"dexafix", "no-such-command"}, devnull
	t.Setenv("LOGFORMAT", "console")
	main()
	if got != 1 {
		t.Fatalf("main exited with %d, want 1", got)
	}
}

func TestTheLogLevelComesFromTheEnvironment(t *testing.T) {
	for level, want := range map[string]zerolog.Level{
		"trace": zerolog.TraceLevel, "debug": zerolog.DebugLevel, "info": zerolog.InfoLevel,
		"warn": zerolog.WarnLevel, "error": zerolog.ErrorLevel, "fatal": zerolog.FatalLevel,
		"panic": zerolog.PanicLevel, "": zerolog.InfoLevel, "nonsense": zerolog.InfoLevel,
	} {
		if got := parseLogLevel(level); got != want {
			t.Errorf("LOGLEVEL=%q gives %v, want %v", level, got, want)
		}
	}
}

func TestOnlyACharacterDeviceIsATerminal(t *testing.T) {
	null, err := os.Open(os.DevNull) // a character device, like a terminal
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	file, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
	closed, err := os.CreateTemp(t.TempDir(), "closed")
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	if !isTerminal(null) {
		t.Error("a character device is not seen as a terminal")
	}
	if isTerminal(file) {
		t.Error("a plain file is seen as a terminal")
	}
	if isTerminal(closed) {
		t.Error("a file that cannot be examined is seen as a terminal")
	}
}
