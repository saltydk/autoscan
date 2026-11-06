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

// Any non-transient availability error (here a 403, e.g. from an SSO proxy in
// front of Plex) previously stopped the worker for good, stranding the queue
// until restart. The original report was a 421, now classified as transient.
func TestTargetWorkerRecoversAfterDaysOfNonTransientFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		proc := workerProcessor(t, "proxied")
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var healthy atomic.Bool
		var checks atomic.Int64
		scans := make(chan autoscan.Scan, 1)
		target := configuredTarget{id: "proxied", target: workerTarget{
			available: func() error {
				checks.Add(1)
				if healthy.Load() {
					return nil
				}
				return autoscan.HTTPStatusError(&http.Response{
					StatusCode: http.StatusForbidden,
					Status:     "403 Forbidden",
				})
			},
			scan: func(scan autoscan.Scan) error {
				if !healthy.Load() {
					t.Error("scan delivered before target became available")
				}
				scans <- scan
				return nil
			},
		}}
		done := make(chan error, 1)
		go func() { done <- runTarget(ctx, proc, target, time.Second) }()

		time.Sleep(3 * 24 * time.Hour)
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("worker stopped during outage: %v", err)
		default:
		}
		// Capped backoff: at most one check per retryMaxDelay once ramped up.
		if got, limit := checks.Load(), int64(3*24*time.Hour/retryMaxDelay)+10; got > limit {
			t.Errorf("availability checks during outage = %d, want <= %d", got, limit)
		}

		healthy.Store(true)
		time.Sleep(retryMaxDelay)
		synctest.Wait()
		select {
		case scan := <-scans:
			if scan.Folder != "/media/TV/Show/Season 1" {
				t.Errorf("scan folder = %q", scan.Folder)
			}
		default:
			t.Fatal("queued scan not delivered after target recovered")
		}
		if remaining, err := proc.ScansRemaining(); err != nil || remaining != 0 {
			t.Errorf("queue after recovery = %d, %v", remaining, err)
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("runTarget = %v", err)
		}
	})
}

func TestTargetWorkerBacksOffExponentially(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		proc := workerProcessor(t, "flaky")
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		start := time.Now()
		var attempts []time.Duration
		var mu sync.Mutex
		target := configuredTarget{id: "flaky", target: workerTarget{
			available: func() error {
				mu.Lock()
				defer mu.Unlock()
				attempts = append(attempts, time.Since(start))
				return autoscan.ErrFatal
			},
			scan: func(autoscan.Scan) error { return nil },
		}}
		go func() { _ = runTarget(ctx, proc, target, time.Second) }()
		time.Sleep(20 * time.Minute)
		synctest.Wait()
		cancel()
		synctest.Wait()

		want := []time.Duration{0, 15, 45, 105, 225, 465, 765, 1065}
		mu.Lock()
		defer mu.Unlock()
		if len(attempts) < len(want) {
			t.Fatalf("attempts = %v", attempts)
		}
		for i, w := range want {
			if attempts[i] != w*time.Second {
				t.Errorf("attempt %d at %v, want %v (all: %v)", i, attempts[i], w*time.Second, attempts)
			}
		}
	})
}

func TestTargetWorkerResetsBackoffAfterRecovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		proc := workerProcessor(t, "blip")
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var mu sync.Mutex
		var attempts []time.Time
		failNext := 3
		target := configuredTarget{id: "blip", target: workerTarget{scan: func(autoscan.Scan) error {
			mu.Lock()
			defer mu.Unlock()
			attempts = append(attempts, time.Now())
			if failNext > 0 {
				failNext--
				return autoscan.ErrFatal
			}
			return nil
		}}}
		go func() { _ = runTarget(ctx, proc, target, time.Second) }()
		time.Sleep(10 * time.Minute) // three failures, then delivery and idle polling
		synctest.Wait()
		mu.Lock()
		failNext = 1
		before := len(attempts)
		mu.Unlock()
		if err := proc.Add(autoscan.Scan{Folder: "/media/TV/Show/Season 2", Time: time.Unix(1, 0)}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		cancel()
		synctest.Wait()

		mu.Lock()
		defer mu.Unlock()
		if len(attempts) != before+2 {
			t.Fatalf("attempts after new scan = %d, want %d", len(attempts)-before, 2)
		}
		if gap := attempts[before+1].Sub(attempts[before]); gap != retryBaseDelay {
			t.Errorf("first retry after recovery waited %v, want %v", gap, retryBaseDelay)
		}
		if remaining, err := proc.ScansRemaining(); err != nil || remaining != 0 {
			t.Errorf("queue = %d, %v", remaining, err)
		}
	})
}

func TestRetryDelay(t *testing.T) {
	for _, tt := range []struct {
		failures   int
		retryAfter time.Duration
		want       time.Duration
	}{
		{1, 0, 15 * time.Second},
		{2, 0, 30 * time.Second},
		{5, 0, 240 * time.Second},
		{6, 0, retryMaxDelay},
		{1000, 0, retryMaxDelay},
		{1, 2 * time.Minute, 2 * time.Minute},
		{1, 24 * time.Hour, retryAfterMaxDelay},
		{1, time.Duration(1<<63 - 1), retryAfterMaxDelay},
	} {
		if got := retryDelay(tt.failures, tt.retryAfter); got != tt.want {
			t.Errorf("retryDelay(%d, %v) = %v, want %v", tt.failures, tt.retryAfter, got, tt.want)
		}
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
