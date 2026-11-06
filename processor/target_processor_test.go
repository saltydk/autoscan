package processor

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/saltydk/autoscan"
	"github.com/saltydk/autoscan/migrate"
)

type recordingTarget struct {
	scans []autoscan.Scan
	scan  func(autoscan.Scan) error
}

func (*recordingTarget) Available() error { return nil }

func (target *recordingTarget) Scan(scan autoscan.Scan) error {
	target.scans = append(target.scans, scan)
	if target.scan != nil {
		return target.scan(scan)
	}
	return nil
}

func queueProcessor(t *testing.T, path string, minimumAge time.Duration) (*Processor, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	mg, err := migrate.New(db, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	proc, err := New(Config{Db: db, Mg: mg, MinimumAge: minimumAge})
	if err != nil {
		t.Fatal(err)
	}
	return proc, db
}

func TestTargetQueuesIsolateFailureAndRejectWithoutBlocking(t *testing.T) {
	proc, _ := queueProcessor(t, ":memory:", 0)
	if err := proc.ConfigureTargets([]string{"healthy", "offline", "rejected"}); err != nil {
		t.Fatal(err)
	}
	scan := autoscan.Scan{Folder: "/media/TV/Show/Season 1", Time: now().Add(-time.Hour)}
	if err := proc.Add(scan); err != nil {
		t.Fatal(err)
	}
	offline := &recordingTarget{scan: func(autoscan.Scan) error { return autoscan.ErrTargetUnavailable }}
	if err := proc.ProcessTarget("offline", offline); !errors.Is(err, autoscan.ErrTargetUnavailable) {
		t.Fatalf("offline delivery = %v", err)
	}
	healthy := &recordingTarget{}
	if err := proc.ProcessTarget("healthy", healthy); err != nil {
		t.Fatal(err)
	}
	rejected := &recordingTarget{scan: func(autoscan.Scan) error {
		return fmt.Errorf("library root scans are disabled: %w", autoscan.ErrScanRejected)
	}}
	if err := proc.ProcessTarget("rejected", rejected); err != nil {
		t.Fatal(err)
	}
	if err := proc.ProcessTarget("healthy", healthy); !errors.Is(err, autoscan.ErrNoScans) {
		t.Fatalf("healthy queue was rescanned after success: %v", err)
	}
	if remaining, err := proc.ScansRemaining(); err != nil || remaining != 1 {
		t.Fatalf("remaining = %d, %v; want offline delivery only", remaining, err)
	}
	offline.scan = nil
	if err := proc.ProcessTarget("offline", offline); err != nil {
		t.Fatal(err)
	}
	if remaining, err := proc.ScansRemaining(); err != nil || remaining != 0 {
		t.Fatalf("remaining after recovery = %d, %v", remaining, err)
	}
	if len(healthy.scans) != 1 || len(offline.scans) != 2 || len(rejected.scans) != 1 {
		t.Fatalf("delivery counts healthy/offline/rejected = %d/%d/%d", len(healthy.scans), len(offline.scans), len(rejected.scans))
	}
}

func TestTargetQueuesCoalesceAndWaitForMinimumAge(t *testing.T) {
	proc, _ := queueProcessor(t, ":memory:", 10*time.Minute)
	if err := proc.ConfigureTargets([]string{"plex"}); err != nil {
		t.Fatal(err)
	}
	clock := now()
	oldNow := now
	now = func() time.Time { return clock }
	t.Cleanup(func() { now = oldNow })
	folder := "/media/TV/Show/Season 1"
	for _, scan := range []autoscan.Scan{
		{Folder: folder, Priority: 5, Time: clock.Add(-20 * time.Minute)},
		{Folder: folder, Priority: 2, Time: clock.Add(-time.Minute)},
	} {
		if err := proc.Add(scan); err != nil {
			t.Fatal(err)
		}
	}
	target := &recordingTarget{}
	if err := proc.ProcessTarget("plex", target); !errors.Is(err, autoscan.ErrNoScans) {
		t.Fatalf("young folder delivery = %v", err)
	}
	if remaining, err := proc.ScansRemaining(); err != nil || remaining != 1 {
		t.Fatalf("coalesced queue size = %d, %v", remaining, err)
	}
	clock = clock.Add(10 * time.Minute)
	if err := proc.ProcessTarget("plex", target); err != nil {
		t.Fatal(err)
	}
	if len(target.scans) != 1 || target.scans[0].Priority != 5 {
		t.Fatalf("coalesced delivery = %+v", target.scans)
	}
}

func TestTargetQueueAcknowledgesOnlyDeliveredGeneration(t *testing.T) {
	proc, _ := queueProcessor(t, ":memory:", 0)
	if err := proc.ConfigureTargets([]string{"plex"}); err != nil {
		t.Fatal(err)
	}
	scan := autoscan.Scan{Folder: "/media/TV/Show/Season 1", Time: now().Add(-time.Hour)}
	if err := proc.Add(scan); err != nil {
		t.Fatal(err)
	}
	target := &recordingTarget{scan: func(scan autoscan.Scan) error {
		// Identical timestamps must still identify a new event during delivery.
		scan.Priority++
		return proc.Add(scan)
	}}
	if err := proc.ProcessTarget("plex", target); err != nil {
		t.Fatal(err)
	}
	if remaining, err := proc.ScansRemaining(); err != nil || remaining != 1 {
		t.Fatalf("event during delivery was lost: remaining = %d, %v", remaining, err)
	}
	target.scan = nil
	if err := proc.ProcessTarget("plex", target); err != nil {
		t.Fatal(err)
	}
	if len(target.scans) != 2 || target.scans[1].Priority != 1 {
		t.Fatalf("replacement event = %+v", target.scans)
	}
}

func TestTargetQueuesMigrateLegacyAndRecoverAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "autoscan.db")
	proc, db := queueProcessor(t, path, 0)
	if err := proc.Add(autoscan.Scan{Folder: "/media/TV/Show/Season 1", Time: now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := proc.ConfigureTargets([]string{"healthy", "offline"}); err != nil {
		t.Fatal(err)
	}
	if err := proc.ProcessTarget("healthy", &recordingTarget{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	proc, _ = queueProcessor(t, path, 0)
	if err := proc.ConfigureTargets([]string{"offline", "healthy"}); err != nil {
		t.Fatal(err)
	}
	if remaining, err := proc.ScansRemaining(); err != nil || remaining != 1 {
		t.Fatalf("queue after restart = %d, %v", remaining, err)
	}
	if err := proc.ProcessTarget("healthy", &recordingTarget{}); !errors.Is(err, autoscan.ErrNoScans) {
		t.Fatalf("completed delivery reappeared after restart: %v", err)
	}
	if err := proc.ProcessTarget("offline", &recordingTarget{}); err != nil {
		t.Fatal(err)
	}
}

func TestTargetQueueWritesAreAtomic(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			proc, db := queueProcessor(t, ":memory:", 0)
			scan := autoscan.Scan{Folder: "/media/TV/Show/Season 1", Time: now().Add(-time.Hour)}
			if legacy {
				if err := proc.Add(scan); err != nil {
					t.Fatal(err)
				}
			} else if err := proc.ConfigureTargets([]string{"healthy", "offline"}); err != nil {
				t.Fatal(err)
			}
			_, err := db.Exec(`CREATE TRIGGER fail_target BEFORE INSERT ON target_scan
WHEN NEW.target_id = 'offline' BEGIN SELECT RAISE(ABORT, 'simulated write failure'); END`)
			if err != nil {
				t.Fatal(err)
			}
			if legacy {
				err = proc.ConfigureTargets([]string{"healthy", "offline"})
			} else {
				err = proc.Add(scan)
			}
			if err == nil {
				t.Fatal("expected failed queue write")
			}
			var count int
			if err := db.QueryRow(`SELECT COUNT(*) FROM target_scan`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("partially written queues = %d, %v", count, err)
			}
			if legacy {
				if remaining, err := proc.ScansRemaining(); err != nil || remaining != 1 {
					t.Fatalf("legacy event lost on failed migration = %d, %v", remaining, err)
				}
			}
		})
	}
}

func TestTargetQueuesRetainDormantTargets(t *testing.T) {
	proc, _ := queueProcessor(t, ":memory:", 0)
	if err := proc.ConfigureTargets([]string{"disabled"}); err != nil {
		t.Fatal(err)
	}
	if err := proc.Add(autoscan.Scan{Folder: "/media/TV/Show/Season 1", Time: now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := proc.ConfigureTargets([]string{"active"}); err != nil {
		t.Fatal(err)
	}
	if remaining, err := proc.ScansRemaining(); err != nil || remaining != 0 {
		t.Fatalf("dormant queue included in active stats = %d, %v", remaining, err)
	}
	if err := proc.ConfigureTargets([]string{"disabled"}); err != nil {
		t.Fatal(err)
	}
	if err := proc.ProcessTarget("disabled", &recordingTarget{}); err != nil {
		t.Fatalf("dormant work was not restored: %v", err)
	}
}

func queuedFolders(t *testing.T, db *sql.DB, targetID string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT folder FROM target_scan WHERE target_id = ? ORDER BY folder`, targetID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var folders []string
	for rows.Next() {
		var folder string
		if err := rows.Scan(&folder); err != nil {
			t.Fatal(err)
		}
		folders = append(folders, folder)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return folders
}

// Changing an unnamed target's URL changes its queue identity; v1.4.6 left the
// old backlog dormant forever, which silently dropped it.
func TestTargetQueueFollowsIdentityChange(t *testing.T) {
	proc, db := queueProcessor(t, ":memory:", 0)
	if err := proc.ConfigureTargets([]string{"plex:old-url", "emby:server"}); err != nil {
		t.Fatal(err)
	}
	for _, folder := range []string{"/media/TV/A", "/media/TV/B"} {
		if err := proc.Add(autoscan.Scan{Folder: folder, Time: now().Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := proc.ConfigureTargets([]string{"plex:new-url", "emby:server"}); err != nil {
		t.Fatal(err)
	}
	if got := queuedFolders(t, db, "plex:new-url"); len(got) != 2 {
		t.Fatalf("adopted queue = %v, want both scans", got)
	}
	if got := queuedFolders(t, db, "plex:old-url"); len(got) != 0 {
		t.Fatalf("old queue still holds %v", got)
	}
	if got := queuedFolders(t, db, "emby:server"); len(got) != 2 {
		t.Fatalf("other target queue changed: %v", got)
	}
	if remaining, err := proc.ScansRemaining(); err != nil || remaining != 4 {
		t.Fatalf("remaining = %d, %v", remaining, err)
	}
}

// Upgrading from v1.4.6 has no identity history, so autoscan cannot tell a
// renamed target from a removed one; a drained queue must not take another
// target's backlog.
func TestTargetQueueNoAdoptionWithoutIdentityHistory(t *testing.T) {
	proc, db := queueProcessor(t, ":memory:", 0)
	if _, err := db.Exec(`INSERT INTO target_scan (target_id, folder, priority, time) VALUES ('plex:removed', '/media/TV/A', 0, ?)`, now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := proc.ConfigureTargets([]string{"plex:drained"}); err != nil {
		t.Fatal(err)
	}
	if got := queuedFolders(t, db, "plex:drained"); len(got) != 0 {
		t.Fatalf("drained target adopted a removed target's queue: %v", got)
	}
	if got := queuedFolders(t, db, "plex:removed"); len(got) != 1 {
		t.Fatalf("removed target queue = %v, want retained", got)
	}
}

func addScan(t *testing.T, proc *Processor, folder string) {
	t.Helper()
	if err := proc.Add(autoscan.Scan{Folder: folder, Time: now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
}

func configure(t *testing.T, proc *Processor, ids ...string) {
	t.Helper()
	if err := proc.ConfigureTargets(ids); err != nil {
		t.Fatal(err)
	}
}

// A target removed in an earlier run keeps its queue for re-adding; a target
// added later is not a rename of it.
func TestTargetQueueRemovedThenOtherAddedKeepsBacklog(t *testing.T) {
	proc, db := queueProcessor(t, ":memory:", 0)
	configure(t, proc, "plex:a", "plex:b")
	addScan(t, proc, "/media/TV/X")
	if err := proc.ProcessTarget("plex:a", &recordingTarget{}); err != nil {
		t.Fatal(err)
	}
	configure(t, proc, "plex:a")
	configure(t, proc, "plex:a", "plex:c")
	if got := queuedFolders(t, db, "plex:c"); len(got) != 0 {
		t.Fatalf("unrelated new target took removed target's backlog: %v", got)
	}
	configure(t, proc, "plex:a", "plex:b", "plex:c")
	if got := queuedFolders(t, db, "plex:b"); len(got) != 1 {
		t.Fatalf("re-added target backlog = %v, want resumed", got)
	}
}

// Reverting a mistaken URL edit must bring the backlog back to the original.
func TestTargetQueueFollowsRevertedIdentityChange(t *testing.T) {
	proc, db := queueProcessor(t, ":memory:", 0)
	configure(t, proc, "plex:a")
	addScan(t, proc, "/media/TV/X")
	configure(t, proc, "plex:typo")
	addScan(t, proc, "/media/TV/Y")
	configure(t, proc, "plex:a")
	if got := queuedFolders(t, db, "plex:a"); len(got) != 2 {
		t.Fatalf("reverted target queue = %v, want both scans", got)
	}
	if got := queuedFolders(t, db, "plex:typo"); len(got) != 0 {
		t.Fatalf("typo queue still holds %v", got)
	}
}

func TestTargetQueueReportsDormantScans(t *testing.T) {
	proc, _ := queueProcessor(t, ":memory:", 0)
	configure(t, proc, "plex:a", "emby:b")
	addScan(t, proc, "/media/TV/X")
	configure(t, proc, "plex:a")
	if got, err := proc.DormantScans(); err != nil || got != 1 {
		t.Fatalf("dormant scans = %d, %v", got, err)
	}
}

func TestTargetQueueAdoptionIsConservative(t *testing.T) {
	t.Run("removed target stays dormant for re-adding", func(t *testing.T) {
		proc, db := queueProcessor(t, ":memory:", 0)
		if err := proc.ConfigureTargets([]string{"plex:a", "plex:b"}); err != nil {
			t.Fatal(err)
		}
		if err := proc.Add(autoscan.Scan{Folder: "/media/TV/A", Time: now().Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
		if err := proc.ConfigureTargets([]string{"plex:a"}); err != nil {
			t.Fatal(err)
		}
		if got := queuedFolders(t, db, "plex:b"); len(got) != 1 {
			t.Fatalf("removed target queue = %v, want retained", got)
		}
	})
	t.Run("ambiguous changes stay dormant", func(t *testing.T) {
		proc, db := queueProcessor(t, ":memory:", 0)
		if err := proc.ConfigureTargets([]string{"plex:a", "plex:b"}); err != nil {
			t.Fatal(err)
		}
		if err := proc.Add(autoscan.Scan{Folder: "/media/TV/A", Time: now().Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
		if err := proc.ConfigureTargets([]string{"plex:c", "plex:d"}); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"plex:a", "plex:b"} {
			if got := queuedFolders(t, db, id); len(got) != 1 {
				t.Errorf("%s queue = %v, want retained", id, got)
			}
		}
		for _, id := range []string{"plex:c", "plex:d"} {
			if got := queuedFolders(t, db, id); len(got) != 0 {
				t.Errorf("%s adopted ambiguous work: %v", id, got)
			}
		}
	})
	t.Run("different kind is never adopted", func(t *testing.T) {
		proc, db := queueProcessor(t, ":memory:", 0)
		if err := proc.ConfigureTargets([]string{"plex:a"}); err != nil {
			t.Fatal(err)
		}
		if err := proc.Add(autoscan.Scan{Folder: "/media/TV/A", Time: now().Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
		if err := proc.ConfigureTargets([]string{"jellyfin:a"}); err != nil {
			t.Fatal(err)
		}
		if got := queuedFolders(t, db, "plex:a"); len(got) != 1 {
			t.Fatalf("plex queue = %v, want retained", got)
		}
	})
	t.Run("known empty target does not claim another's queue", func(t *testing.T) {
		proc, db := queueProcessor(t, ":memory:", 0)
		if err := proc.ConfigureTargets([]string{"plex:a", "plex:b"}); err != nil {
			t.Fatal(err)
		}
		if err := proc.Add(autoscan.Scan{Folder: "/media/TV/A", Time: now().Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
		if err := proc.ProcessTarget("plex:a", &recordingTarget{}); err != nil {
			t.Fatal(err)
		}
		// plex:a is known and drained; plex:b is removed. plex:a must not take b's work.
		if err := proc.ConfigureTargets([]string{"plex:a"}); err != nil {
			t.Fatal(err)
		}
		if got := queuedFolders(t, db, "plex:a"); len(got) != 0 {
			t.Fatalf("known target adopted removed target's work: %v", got)
		}
	})
}
