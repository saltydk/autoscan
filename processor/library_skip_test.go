package processor

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/saltydk/autoscan"
)

func TestLibrarySkipAcknowledgesOnlyItsTargetDelivery(t *testing.T) {
	proc, _ := queueProcessor(t, ":memory:", 0)
	if err := proc.ConfigureTargets([]string{"skip", "healthy", "offline", "rejected"}); err != nil {
		t.Fatal(err)
	}
	if err := proc.Add(autoscan.Scan{Folder: "/media/Movies/Example.Movie.2026", Time: now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	for _, outcome := range []struct {
		id  string
		err error
	}{
		{"skip", fmt.Errorf("show-only library: %w", autoscan.ErrLibraryNotMatched)},
		{"healthy", nil},
		{"offline", autoscan.ErrTargetUnavailable},
		{"rejected", autoscan.ErrScanRejected},
	} {
		target := &recordingTarget{scan: func(autoscan.Scan) error { return outcome.err }}
		err := proc.ProcessTarget(outcome.id, target)
		if outcome.id == "offline" {
			if !errors.Is(err, autoscan.ErrTargetUnavailable) {
				t.Fatalf("offline delivery = %v", err)
			}
		} else if err != nil {
			t.Fatalf("%s delivery = %v", outcome.id, err)
		}
	}
	if remaining, err := proc.ScansRemaining(); err != nil || remaining != 1 {
		t.Fatalf("remaining deliveries = %d, %v; want only offline", remaining, err)
	}
	if processed := proc.ScansProcessed(); processed != 1 {
		t.Fatalf("processed deliveries = %d, want one accepted target", processed)
	}
	if err := proc.ProcessTarget("skip", &recordingTarget{}); !errors.Is(err, autoscan.ErrNoScans) {
		t.Fatalf("skipped delivery remained queued: %v", err)
	}
}

func TestLegacyLibrarySkipDoesNotCountAsProcessed(t *testing.T) {
	proc, _ := queueProcessor(t, ":memory:", 0)
	if err := proc.Add(autoscan.Scan{Folder: "/media/Movies/Example.Movie.2026", Time: now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	target := &recordingTarget{scan: func(autoscan.Scan) error { return autoscan.ErrLibraryNotMatched }}
	if err := proc.Process([]autoscan.Target{target}); err != nil {
		t.Fatal(err)
	}
	if processed := proc.ScansProcessed(); processed != 0 {
		t.Fatalf("legacy skipped folder counted as processed: %d", processed)
	}
	if remaining, err := proc.ScansRemaining(); err != nil || remaining != 0 {
		t.Fatalf("legacy skip queue = %d, %v", remaining, err)
	}
}

func TestLegacyLibrarySkipDoesNotCompleteFailedAcknowledgment(t *testing.T) {
	proc, db := queueProcessor(t, ":memory:", 0)
	if err := proc.Add(autoscan.Scan{Folder: "/media/Movies/Example.Movie.2026", Time: now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER fail_scan_delete BEFORE DELETE ON scan BEGIN SELECT RAISE(ABORT, 'ack failed'); END`); err != nil {
		t.Fatal(err)
	}
	target := &recordingTarget{scan: func(autoscan.Scan) error { return autoscan.ErrLibraryNotMatched }}
	if err := proc.Process([]autoscan.Target{target}); !errors.Is(err, autoscan.ErrFatal) {
		t.Fatalf("failed legacy acknowledgment = %v", err)
	}
	if processed := proc.ScansProcessed(); processed != 0 {
		t.Fatalf("failed acknowledgment counted as processed: %d", processed)
	}
}
