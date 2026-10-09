package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func TestFileLoggingPreservesTextAndAllowsJSON(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			var stderr, file bytes.Buffer
			logger, err := buildLogger(&stderr, &file, format, 0, "")
			if err != nil {
				t.Fatal(err)
			}
			logger.Info().Int("remaining", 3).Msg("Scan stats")
			if !strings.Contains(stderr.String(), "Scan stats") || json.Valid(stderr.Bytes()) {
				t.Fatalf("console output = %q", stderr.String())
			}
			if format == "text" {
				if !strings.Contains(file.String(), "Scan stats") || json.Valid(file.Bytes()) || strings.Contains(file.String(), "\x1b[") {
					t.Fatalf("default file output = %q", file.String())
				}
				return
			}
			var record map[string]any
			if err := json.Unmarshal(file.Bytes(), &record); err != nil {
				t.Fatalf("JSON file output = %q: %v", file.String(), err)
			}
			if record["message"] != "Scan stats" || record["level"] != "info" || record["remaining"] != float64(3) || record["time"] == nil {
				t.Fatalf("JSON record = %+v", record)
			}
		})
	}
}

func TestLoggingVerbosityAndExplicitLevel(t *testing.T) {
	for _, tc := range []struct {
		name      string
		verbosity int
		level     string
		want      zerolog.Level
	}{
		{"default", 0, "", zerolog.InfoLevel},
		{"verbose", 1, "", zerolog.DebugLevel},
		{"trace", 2, "", zerolog.TraceLevel},
		{"explicit override", 2, "warn", zerolog.WarnLevel},
		{"explicit trace", 0, "trace", zerolog.TraceLevel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stderr, file bytes.Buffer
			logger, err := buildLogger(&stderr, &file, "text", tc.verbosity, tc.level)
			if err != nil || logger.GetLevel() != tc.want {
				t.Fatalf("level = %v, error = %v; want %v", logger.GetLevel(), err, tc.want)
			}
		})
	}
	for _, tc := range []struct{ format, level string }{{"invalid", ""}, {"text", "invalid"}} {
		if _, err := buildLogger(&bytes.Buffer{}, &bytes.Buffer{}, tc.format, 0, tc.level); err == nil {
			t.Errorf("invalid logging options accepted: %+v", tc)
		}
	}
}
