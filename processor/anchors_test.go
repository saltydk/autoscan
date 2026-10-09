package processor

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/saltydk/autoscan"
)

type anchorLog struct {
	Level   string   `json:"level"`
	Missing []string `json:"missing_anchors"`
}

func captureAnchorLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var output bytes.Buffer
	previous := log.Logger
	log.Logger = zerolog.New(&output)
	t.Cleanup(func() { log.Logger = previous })
	return &output
}

func readAnchorLogs(t *testing.T, output *bytes.Buffer) []anchorLog {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	var entries []anchorLog
	for {
		var entry anchorLog
		if err := decoder.Decode(&entry); err != nil {
			if errors.Is(err, io.EOF) {
				return entries
			}
			t.Fatalf("decode anchor log: %v", err)
		}
		entries = append(entries, entry)
	}
}

func TestAnchorTransitionsLogChangesOnce(t *testing.T) {
	output := captureAnchorLogs(t)
	first := filepath.Join(t.TempDir(), "first.anchor")
	second := filepath.Join(t.TempDir(), "second.anchor")
	p := &Processor{anchors: []string{first, second}}
	for range 10 {
		if err := p.checkAnchors(); !errors.Is(err, autoscan.ErrAnchorUnavailable) {
			t.Fatalf("initial missing anchors = %v", err)
		}
	}
	entries := readAnchorLogs(t, output)
	if len(entries) != 1 || !slices.Equal(entries[0].Missing, []string{first, second}) {
		t.Fatalf("repeated outage logs = %+v, want one complete missing set", entries)
	}
	if err := os.WriteFile(first, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if err := p.checkAnchors(); !errors.Is(err, autoscan.ErrAnchorUnavailable) {
			t.Fatalf("partially recovered anchors = %v", err)
		}
	}
	entries = readAnchorLogs(t, output)
	if len(entries) != 2 || !slices.Equal(entries[1].Missing, []string{second}) {
		t.Fatalf("changed outage logs = %+v, want the remaining missing file once", entries)
	}
	if err := os.WriteFile(second, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if err := p.checkAnchors(); err != nil {
			t.Fatalf("recovered anchors = %v", err)
		}
	}
	entries = readAnchorLogs(t, output)
	if len(entries) != 3 || entries[2].Level != "info" || len(entries[2].Missing) != 0 {
		t.Fatalf("recovery logs = %+v, want one recovery notification", entries)
	}
}

func TestConcurrentAnchorChecksShareTransitionState(t *testing.T) {
	output := captureAnchorLogs(t)
	anchor := filepath.Join(t.TempDir(), "media.anchor")
	p := &Processor{anchors: []string{anchor}}
	check := func(want error) {
		var workers sync.WaitGroup
		for range 32 {
			workers.Go(func() {
				if err := p.checkAnchors(); !errors.Is(err, want) {
					t.Errorf("concurrent anchor check = %v, want %v", err, want)
				}
			})
		}
		workers.Wait()
	}
	check(autoscan.ErrAnchorUnavailable)
	if entries := readAnchorLogs(t, output); len(entries) != 1 {
		t.Fatalf("concurrent outage produced %d logs, want one", len(entries))
	}
	if err := os.WriteFile(anchor, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	check(nil)
	if entries := readAnchorLogs(t, output); len(entries) != 2 || entries[1].Level != "info" {
		t.Fatalf("concurrent recovery logs = %+v, want one additional recovery", entries)
	}
}

func TestAnchorDirectoryDoesNotReplaceSentinelFile(t *testing.T) {
	output := captureAnchorLogs(t)
	anchor := filepath.Join(t.TempDir(), "media.anchor")
	if err := os.Mkdir(anchor, 0o700); err != nil {
		t.Fatal(err)
	}
	proc, _ := queueProcessor(t, ":memory:", 0)
	proc.anchors = []string{anchor}
	if err := proc.ConfigureTargets([]string{"plex"}); err != nil {
		t.Fatal(err)
	}
	if err := proc.Add(autoscan.Scan{Folder: "/media/Movies/Example Movie (2026)", Time: now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	target := &recordingTarget{}
	for range 5 {
		if err := proc.ProcessTarget("plex", target); !errors.Is(err, autoscan.ErrAnchorUnavailable) {
			t.Fatalf("directory anchor delivery = %v", err)
		}
	}
	if len(target.scans) != 0 {
		t.Fatal("directory anchor allowed target delivery")
	}
	if entries := readAnchorLogs(t, output); len(entries) != 1 {
		t.Fatalf("unchanged directory anchor produced %d outage logs", len(entries))
	}
	if err := os.Remove(anchor); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(anchor, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := proc.ProcessTarget("plex", target); err != nil {
		t.Fatalf("sentinel file recovery = %v", err)
	}
	if len(target.scans) != 1 {
		t.Fatalf("recovered target received %d scans, want one", len(target.scans))
	}
}
