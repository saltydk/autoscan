package main

import (
	"fmt"
	"io"
	"time"

	"github.com/rs/zerolog"
)

func buildLogger(stderr, file io.Writer, format string, verbosity int, level string) (zerolog.Logger, error) {
	var fileOutput io.Writer
	switch format {
	case "text":
		fileOutput = zerolog.ConsoleWriter{Out: file, TimeFormat: time.Stamp, NoColor: true}
	case "json":
		fileOutput = file
	default:
		return zerolog.Logger{}, fmt.Errorf("unknown log format %q; use text or json", format)
	}
	logLevel := zerolog.InfoLevel
	switch {
	case verbosity > 1:
		logLevel = zerolog.TraceLevel
	case verbosity == 1:
		logLevel = zerolog.DebugLevel
	}
	if level != "" {
		var err error
		logLevel, err = zerolog.ParseLevel(level)
		if err != nil {
			return zerolog.Logger{}, fmt.Errorf("invalid log level %q: %w", level, err)
		}
	}
	output := io.MultiWriter(
		zerolog.ConsoleWriter{Out: stderr, TimeFormat: time.Stamp}, fileOutput,
	)
	return zerolog.New(output).With().Timestamp().Logger().Level(logLevel), nil
}
