package processor

import (
	"errors"
	"testing"
	"time"

	"github.com/saltydk/autoscan"
)

func TestReceivedMetricsCountPersistedEvents(t *testing.T) {
	for _, independent := range []bool{false, true} {
		name := "legacy"
		if independent {
			name = "target_queues"
		}
		t.Run(name, func(t *testing.T) {
			proc, db := queueProcessor(t, ":memory:", 0)
			if independent {
				if err := proc.ConfigureTargets([]string{"plex", "jellyfin"}); err != nil {
					t.Fatal(err)
				}
			}
			scan := autoscan.Scan{Folder: "/media/TV/Example Show/Season 01", Time: now().Add(-time.Hour)}
			if err := proc.Add(scan, scan); err != nil {
				t.Fatal(err)
			}
			if metrics := proc.Metrics(); metrics != (MetricsSnapshot{Received: 2}) {
				t.Fatalf("metrics after coalesced input events = %+v", metrics)
			}
			wantPending := 1
			if independent {
				wantPending = 2
			}
			if pending, err := proc.ScansRemaining(); err != nil || pending != wantPending {
				t.Fatalf("pending deliveries = %d, %v; want %d", pending, err, wantPending)
			}

			if independent {
				if _, err := db.Exec(`CREATE TRIGGER reject_input BEFORE INSERT ON target_scan
WHEN NEW.target_id = 'jellyfin' BEGIN SELECT RAISE(ABORT, 'simulated persistence failure'); END`); err != nil {
					t.Fatal(err)
				}
			} else if _, err := db.Exec(`DROP TABLE scan`); err != nil {
				t.Fatal(err)
			}
			if err := proc.Add(scan); err == nil {
				t.Fatal("expected failed persistence")
			}
			if metrics := proc.Metrics(); metrics != (MetricsSnapshot{Received: 2}) {
				t.Fatalf("failed persistence changed input counters: %+v", metrics)
			}
		})
	}
}

func TestTargetMetricsDistinguishCompletedOutcomes(t *testing.T) {
	proc, _ := queueProcessor(t, ":memory:", 0)
	if err := proc.ConfigureTargets([]string{"processed", "skipped", "rejected", "unavailable"}); err != nil {
		t.Fatal(err)
	}
	if err := proc.Add(autoscan.Scan{Folder: "/media/TV/Example Show/Season 01", Time: now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	for _, delivery := range []struct {
		id     string
		result error
	}{
		{"processed", nil},
		{"skipped", autoscan.ErrLibraryNotMatched},
		{"rejected", autoscan.ErrScanRejected},
		{"unavailable", autoscan.ErrTargetUnavailable},
	} {
		target := &recordingTarget{scan: func(autoscan.Scan) error { return delivery.result }}
		err := proc.ProcessTarget(delivery.id, target)
		if delivery.id == "unavailable" {
			if !errors.Is(err, autoscan.ErrTargetUnavailable) {
				t.Fatalf("unavailable delivery = %v", err)
			}
			proc.RecordRetry()
		} else if err != nil {
			t.Fatalf("%s delivery = %v", delivery.id, err)
		}
	}
	want := MetricsSnapshot{Received: 1, Processed: 1, Retried: 1, Skipped: 1, Rejected: 1}
	if metrics := proc.Metrics(); metrics != want {
		t.Fatalf("completed delivery metrics = %+v, want %+v", metrics, want)
	}
	if count := proc.ScansProcessed(); count != 1 {
		t.Fatalf("ScansProcessed() = %d, want accepted deliveries only", count)
	}
	if pending, err := proc.ScansRemaining(); err != nil || pending != 1 {
		t.Fatalf("remaining unavailable delivery = %d, %v", pending, err)
	}
}

func TestTargetMetricsDoNotCompleteFailedAcknowledgment(t *testing.T) {
	proc, db := queueProcessor(t, ":memory:", 0)
	if err := proc.ConfigureTargets([]string{"plex"}); err != nil {
		t.Fatal(err)
	}
	if err := proc.Add(autoscan.Scan{Folder: "/media/Movies/Example Movie (2026)", Time: now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER reject_ack BEFORE DELETE ON target_scan
BEGIN SELECT RAISE(ABORT, 'simulated acknowledgment failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := proc.ProcessTarget("plex", &recordingTarget{}); !errors.Is(err, autoscan.ErrFatal) {
		t.Fatalf("failed acknowledgment = %v, want ErrFatal", err)
	}
	if metrics := proc.Metrics(); metrics != (MetricsSnapshot{Received: 1}) {
		t.Fatalf("failed acknowledgment counted a completed delivery: %+v", metrics)
	}
	if pending, err := proc.ScansRemaining(); err != nil || pending != 1 {
		t.Fatalf("failed acknowledgment discarded pending work: %d, %v", pending, err)
	}
}
