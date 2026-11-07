package main

import (
	"fmt"
	"os"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexa-fix/cmd"
)

// Version is set at build time through ldflags.
var Version = "dev"

func main() {
	configureLogging()
	fmt.Printf("---\ndexa-fix version: %s\n---\n", Version)
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
