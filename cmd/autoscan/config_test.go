package main

import (
	"strings"
	"testing"
	"time"
)

func TestDecodeConfigDefaults(t *testing.T) {
	c, err := decodeConfig(strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 3030 || len(c.Host) != 1 || c.Host[0] != "" {
		t.Fatalf("unexpected listen defaults: %v:%d", c.Host, c.Port)
	}
	if c.MinimumAge != 10*time.Minute || c.ScanDelay != 5*time.Second || c.ScanStats != time.Hour {
		t.Fatalf("unexpected timing defaults: %s, %s, %s", c.MinimumAge, c.ScanDelay, c.ScanStats)
	}
}

func TestDecodeConfigCompatibility(t *testing.T) {
	const input = `
host: [127.0.0.1, "::1"]
port: 4040
minimum-age: 15m
scan-delay: 2s
scan-stats: 0s
anchors: [/mnt/media/.anchor]
authentication:
  username: autoscan
  password: "on"
triggers:
  sonarr:
    - name: series
      priority: 2
      rewrite:
        - from: ^/downloads/(.*)$
          to: /mnt/media/$1
targets:
  jellyfin:
    - url: http://jellyfin:8096
      token: "12345"
`
	c, err := decodeConfig(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Host) != 2 || c.Host[1] != "::1" || c.Port != 4040 {
		t.Fatalf("unexpected listen configuration: %v:%d", c.Host, c.Port)
	}
	if c.MinimumAge != 15*time.Minute || c.ScanDelay != 2*time.Second || c.ScanStats != 0 {
		t.Fatalf("incorrect duration decoding: %s, %s, %s", c.MinimumAge, c.ScanDelay, c.ScanStats)
	}
	if len(c.Anchors) != 1 || c.Anchors[0] != "/mnt/media/.anchor" {
		t.Fatalf("incorrect anchors: %v", c.Anchors)
	}
	if c.Auth.Username != "autoscan" || c.Auth.Password != "on" {
		t.Fatal("authentication values changed during decoding")
	}
	if len(c.Triggers.Sonarr) != 1 || c.Triggers.Sonarr[0].Name != "series" || c.Triggers.Sonarr[0].Priority != 2 {
		t.Fatalf("incorrect Sonarr configuration: %+v", c.Triggers.Sonarr)
	}
	rewrites := c.Triggers.Sonarr[0].Rewrite
	if len(rewrites) != 1 || rewrites[0].From != "^/downloads/(.*)$" || rewrites[0].To != "/mnt/media/$1" {
		t.Fatalf("rewrite expressions changed: %+v", rewrites)
	}
	if len(c.Targets.Jellyfin) != 1 || c.Targets.Jellyfin[0].URL != "http://jellyfin:8096" || c.Targets.Jellyfin[0].Token != "12345" {
		t.Fatalf("incorrect Jellyfin configuration: %+v", c.Targets.Jellyfin)
	}
}

func TestDecodeConfigRejectsInvalidConfiguration(t *testing.T) {
	cases := map[string]string{
		"unknown field":          "minimum-ages: 1m\n",
		"unknown nested field":   "triggers:\n  manual:\n    prioritty: 1\n",
		"duplicate field":        "port: 3030\nport: 4040\n",
		"duplicate nested field": "authentication:\n  username: a\n  username: b\n",
		"invalid duration":       "scan-delay: tomorrow\n",
		"malformed YAML":         "host: [\n",
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeConfig(strings.NewReader(input)); err == nil {
				t.Fatal("expected invalid configuration to be rejected")
			}
		})
	}
}
