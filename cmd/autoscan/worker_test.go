package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/saltydk/autoscan"
	"github.com/saltydk/autoscan/migrate"
	"github.com/saltydk/autoscan/processor"
	"github.com/saltydk/autoscan/targets/plex"
)

type workerTarget struct {
	available func() error
	scan      func(autoscan.Scan) error
}

func (t workerTarget) Available() error {
	if t.available != nil {
		return t.available()
	}
	return nil
}

func (t workerTarget) Scan(scan autoscan.Scan) error { return t.scan(scan) }

func workerProcessor(t *testing.T, ids ...string) *processor.Processor {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	mg, err := migrate.New(db, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	proc, err := processor.New(processor.Config{Db: db, Mg: mg})
	if err != nil {
		t.Fatal(err)
	}
	if err := proc.ConfigureTargets(ids); err != nil {
		t.Fatal(err)
	}
	if err := proc.Add(autoscan.Scan{Folder: "/media/TV/Show/Season 1", Time: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}
	return proc
}

func TestTargetWorkersRemainIndependentDuringStalledDelivery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		proc := workerProcessor(t, "healthy", "stalled")
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		release := make(chan struct{})
		started := make(chan struct{}, 1)
		healthyScans := make(chan autoscan.Scan, 1)
		targets := []configuredTarget{
			{id: "healthy", target: workerTarget{scan: func(scan autoscan.Scan) error {
				healthyScans <- scan
				return nil
			}}},
			{id: "stalled", target: workerTarget{scan: func(autoscan.Scan) error {
				started <- struct{}{}
				<-release
				return nil
			}}},
		}
		var workers sync.WaitGroup
		for _, target := range targets {
			workers.Go(func() {
				err := runTarget(ctx, proc, target, time.Second)
				if !errors.Is(err, context.Canceled) {
					t.Errorf("runTarget(%s) = %v", target.id, err)
				}
			})
		}
		synctest.Wait()
		select {
		case <-started:
		default:
			t.Error("stalled target did not begin delivery")
		}
		select {
		case scan := <-healthyScans:
			if scan.Folder != "/media/TV/Show/Season 1" {
				t.Errorf("healthy scan folder = %q", scan.Folder)
			}
		default:
			t.Error("healthy target blocked behind stalled target")
		}
		if remaining, err := proc.ScansRemaining(); err != nil || remaining != 1 {
			t.Errorf("pending stalled deliveries = %d, %v", remaining, err)
		}
		cancel()
		close(release)
		workers.Wait()
	})
}

func TestTargetWorkerHonorsRetryAfterAndRecovers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		proc := workerProcessor(t, "limited")
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var attempts atomic.Int64
		target := configuredTarget{id: "limited", target: workerTarget{scan: func(autoscan.Scan) error {
			if attempts.Add(1) == 1 {
				return autoscan.HTTPStatusError(&http.Response{
					StatusCode: http.StatusTooManyRequests,
					Status:     "429 Too Many Requests",
					Header:     http.Header{"Retry-After": []string{"60"}},
				})
			}
			return nil
		}}}
		var workers sync.WaitGroup
		workers.Go(func() {
			if err := runTarget(ctx, proc, target, time.Second); !errors.Is(err, context.Canceled) {
				t.Errorf("runTarget = %v", err)
			}
		})
		synctest.Wait()
		time.Sleep(59 * time.Second)
		synctest.Wait()
		if got := attempts.Load(); got != 1 {
			t.Errorf("attempts before Retry-After elapsed = %d", got)
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if got := attempts.Load(); got != 2 {
			t.Errorf("attempts after Retry-After elapsed = %d", got)
		}
		if remaining, err := proc.ScansRemaining(); err != nil || remaining != 0 {
			t.Errorf("queue after target recovery = %d, %v", remaining, err)
		}
		if got := proc.Metrics().Retried; got != 1 {
			t.Errorf("retry count after recovery = %d", got)
		}
		cancel()
		workers.Wait()
	})
}

func TestTargetWorkerRetainsQueueOnPermanentFailure(t *testing.T) {
	proc := workerProcessor(t, "invalid")
	target := configuredTarget{id: "invalid", target: workerTarget{
		available: func() error { return autoscan.ErrFatal },
		scan:      func(autoscan.Scan) error { t.Fatal("unavailable target received a scan"); return nil },
	}}
	if err := runTarget(t.Context(), proc, target, time.Second); !errors.Is(err, autoscan.ErrFatal) {
		t.Fatalf("runTarget = %v", err)
	}
	if remaining, err := proc.ScansRemaining(); err != nil || remaining != 1 {
		t.Fatalf("permanent failure discarded queued work = %d, %v", remaining, err)
	}
}

func TestTargetQueueIdentityAndOfflineConfiguration(t *testing.T) {
	base := targetQueueID("plex", "", "http://plex:32400", nil)
	if got := targetQueueID("plex", "", "http://plex:32400/", nil); got != base {
		t.Fatalf("trailing slash changes identity: %s != %s", got, base)
	}
	if got := targetQueueID("plex", "", "http://plex:32400", []autoscan.Rewrite{}); got != base {
		t.Fatalf("empty rewrite rules change identity: %s != %s", got, base)
	}
	if got := targetQueueID("plex", "named", "http://changed:32400", []autoscan.Rewrite{{From: "old", To: "new"}}); got != "plex:named" {
		t.Fatalf("named identity = %q", got)
	}
	if got := targetQueueID("emby", "", "http://plex:32400", nil); got == base {
		t.Fatal("different target types share identity")
	}
	if got := targetQueueID("plex", "", "http://plex:32400", []autoscan.Rewrite{{From: "old", To: "new"}}); got == base {
		t.Fatal("distinct rewrite rules share implicit identity")
	}
	proc := workerProcessor(t)
	var c config
	c.Targets.Plex = []plex.Config{{URL: "http://127.0.0.1:1", Token: "first"}}
	targets, err := configureTargets(c, proc)
	if err != nil {
		t.Fatalf("offline target blocked configuration: %v", err)
	}
	c.Targets.Plex[0].Token = "changed"
	next, err := configureTargets(c, proc)
	if err != nil || len(next) != 1 || targets[0].id != next[0].id {
		t.Fatalf("credential rotation changed queue ownership: %+v, %v", next, err)
	}
	c.Targets.Plex = append(c.Targets.Plex, c.Targets.Plex[0])
	duplicates, err := configureTargets(c, proc)
	if err != nil || len(duplicates) != 2 {
		t.Fatalf("existing unnamed duplicate targets were rejected: %+v, %v", duplicates, err)
	}
	if duplicates[0].id != targets[0].id || duplicates[1].id != targets[0].id+"#2" {
		t.Fatalf("duplicate identities = %+v", duplicates)
	}
	c.Targets.Plex[1].Name = "second"
	if _, err := configureTargets(c, proc); err != nil {
		t.Fatalf("distinct named target rejected: %v", err)
	}
	c.Targets.Plex[0].Name = "second"
	if _, err := configureTargets(c, proc); err == nil {
		t.Fatal("duplicate explicit target names were accepted")
	}
}

func TestExistingDuplicateTargetYAMLRetainsStableQueues(t *testing.T) {
	c, err := decodeConfig(strings.NewReader(`targets:
  plex:
    - url: http://127.0.0.1:1
      token: first
    - url: http://127.0.0.1:1/
      token: second
`))
	if err != nil {
		t.Fatal(err)
	}
	proc := workerProcessor(t)
	first, err := configureTargets(c, proc)
	if err != nil {
		t.Fatal(err)
	}
	c.Targets.Plex[0].Token = "rotated-first"
	c.Targets.Plex[1].Token = "rotated-second"
	second, err := configureTargets(c, proc)
	if err != nil {
		t.Fatal(err)
	}
	for i := range first {
		if first[i].id != second[i].id {
			t.Fatalf("queue %d changed after credential rotation", i)
		}
	}
	if first[0].id == first[1].id {
		t.Fatal("duplicate targets share a queue")
	}
}
