// Command dexafix is the entry point of the binary. It sets up logging and runs the command line
// implemented in package cmd.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	_ "time/tzdata" // time zones must work in the minimal container image, which has no zoneinfo

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	_ "golang.org/x/crypto/x509roots/fallback" // CA roots for HTTPS when the image (scratch) has none

	"github.com/trueforge-org/dexafix/cmd"
)

// Version is set at build time through ldflags.
var Version = "dev"

// exit ends the process. It is a variable so that a test can run main without ending the test.
var exit = os.Exit

func main() { exit(run()) }

// run sets up logging, runs the command line and returns the exit code of the process.
func run() int {
	format := logFormat(os.Getenv("LOGFORMAT"))
	service := serviceFromArgs(os.Args[1:])
	configureLogging(format, service)
	cmd.Version = Version
	switch {
	case format == formatJSON && service != "":
		log.Info().Str("version", Version).Msg("starting")
	case format == formatConsole:
		fmt.Fprintln(os.Stderr, banner(Version, service)) // stderr: some commands print data on stdout
	}
	if err := cmd.Execute(); err != nil {
		// Execute has already told the person what went wrong, in plain words and on stderr; saying
		// it again as a log line only makes the same error show twice. A log collector still wants it.
		if format == formatJSON {
			log.Error().Err(err).Msg("the command failed")
		}
		return 1
	}
	return 0
}

const (
	formatConsole = "console"
	formatJSON    = "json"
)

// logFormat reads LOGFORMAT: "json" for one JSON object per line (for a log collector), anything else
// for the readable console output.
func logFormat(v string) string {
	if strings.EqualFold(strings.TrimSpace(v), formatJSON) {
		return formatJSON
	}
	return formatConsole
}

// serviceFromArgs names the process a command line starts, for the log lines and the banner:
// "web", "api", "body" or "worker" for `serve <name>`, "dexafix" for the combined `serve`, and ""
// for every other command (db, user, secrets, ...), whose output is not a log.
func serviceFromArgs(args []string) string {
	var words []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			words = append(words, a)
		}
	}
	if len(words) == 0 || words[0] != "serve" {
		return ""
	}
	for _, w := range words[1:] { // the first one: a flag's value is a word too, but never a service name
		switch w {
		case "web", "api", "body", "worker":
			return w
		}
	}
	return "dexafix"
}

func banner(version, service string) string {
	if service == "" || service == "dexafix" {
		return "DexaFix " + version
	}
	return "DexaFix " + version + " · " + service
}

func configureLogging(format, service string) {
	zerolog.DurationFieldUnit = time.Second
	zerolog.SetGlobalLevel(parseLogLevel(os.Getenv("LOGLEVEL")))
	tty := isTerminal(os.Stdout)
	color := tty && os.Getenv("NO_COLOR") == "" && os.Getenv("DEBUG") == ""
	log.Logger = newLogger(os.Stdout, format, color, tty, service)
}

// isTerminal reports whether f is a terminal, where colour and short times help; in a file or a pipe
// (docker logs, a log collector) they get in the way.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// newLogger builds the logger: one JSON object per line, or readable lines such as
//
//	2026-11-10T11:26:21Z INF api │ serving the backend public=:8080 internal=:8081
//
// with the service in front of the message, colour only on a terminal, and the time of day alone
// there (a terminal is read live; a collected log keeps the full time).
func newLogger(w io.Writer, format string, color, tty bool, service string) zerolog.Logger {
	if format == formatJSON {
		c := zerolog.New(w).With().Timestamp()
		if service != "" {
			c = c.Str("service", service)
		}
		return c.Logger()
	}
	timeFormat := time.RFC3339
	if tty {
		timeFormat = "15:04:05"
	}
	cw := zerolog.ConsoleWriter{Out: w, TimeFormat: timeFormat, NoColor: !color}
	if service != "" && service != "dexafix" {
		cw.FormatMessage = func(i any) string {
			if i == nil {
				return service + " │"
			}
			return service + " │ " + fmt.Sprint(i)
		}
	}
	return zerolog.New(cw).With().Timestamp().Logger()
}

func parseLogLevel(level string) zerolog.Level {
	switch level {
	case "trace":
		return zerolog.TraceLevel
	case "debug":
		return zerolog.DebugLevel
	case "warn":
		return zerolog.WarnLevel
	case "error":
		return zerolog.ErrorLevel
	case "fatal":
		return zerolog.FatalLevel
	case "panic":
		return zerolog.PanicLevel
	default:
		return zerolog.InfoLevel
	}
}
