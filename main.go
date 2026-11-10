// Command dexafix is the entry point of the binary. It sets up logging and runs the command line
// implemented in package cmd.
package main

import (
	"fmt"
	"os"
	"time"
	_ "time/tzdata" // time zones must work in the minimal container image, which has no zoneinfo

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	_ "golang.org/x/crypto/x509roots/fallback" // CA roots for HTTPS when the image (scratch) has none

	"github.com/trueforge-org/dexafix/cmd"
)

// Version is set at build time through ldflags.
var Version = "dev"

func main() {
	configureLogging()
	cmd.Version = Version
	fmt.Fprintf(os.Stderr, "---\ndexafix version: %s\n---\n", Version) // stderr: some commands print data on stdout
	if err := cmd.Execute(); err != nil {
		log.Fatal().Err(err).Msg("Failed to execute command")
	}
}

func configureLogging() {
	zerolog.DurationFieldUnit = time.Second
	zerolog.SetGlobalLevel(parseLogLevel(os.Getenv("LOGLEVEL")))
	log.Logger = log.Output(zerolog.ConsoleWriter{
		Out:        os.Stdout,
		TimeFormat: time.RFC3339,
		NoColor:    os.Getenv("DEBUG") != "",
	})
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
