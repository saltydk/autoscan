package bernard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/rs/zerolog"
	"golang.org/x/sync/semaphore"
)

func TestSyncWarningDoesNotReleaseActiveJob(t *testing.T) {
	for _, source := range []string{"full", "partial"} {
		t.Run(source, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				logs := &syncWarningRecorder{}
				d := daemon{
					log: zerolog.New(logs).With().Str("trigger", "bernard").Logger(),
					limiter: &rateLimiter{
						ctx: context.Background(),
						sem: semaphore.NewWeighted(1),
					},
				}
				entered := make(chan struct{})
				release := make(chan struct{})
				var releaseOnce sync.Once
				finish := func() { releaseOnce.Do(func() { close(release) }) }
				defer finish()
				done := make(chan struct{})
				var calls atomic.Int32
				job := newSyncJob(cron.New(), zerolog.Nop(), func() error {
					if err := d.limiter.Acquire(1); err != nil {
						return err
					}
					defer d.limiter.Release(1)
					return d.runSyncWithWarning("test-drive", source, func() error {
						calls.Add(1)
						close(entered)
						<-release
						return nil
					})
				})
				guarded := cron.NewChain(cron.SkipIfStillRunning(cron.DiscardLogger)).Then(job)
				go func() {
					guarded.Run()
					close(done)
				}()
				<-entered
				time.Sleep(syncWarningAfter - time.Nanosecond)
				synctest.Wait()
				if entries := logs.entries(t); len(entries) != 0 {
					t.Fatalf("warning logged before threshold: %v", entries)
				}
				time.Sleep(time.Nanosecond)
				synctest.Wait()
				entries := logs.entries(t)
				if len(entries) != 1 {
					t.Fatalf("got %d warning entries, want 1", len(entries))
				}
				entry := entries[0]
				if entry["level"] != "warn" || entry["trigger"] != "bernard" || entry["drive_id"] != "test-drive" || entry["sync_source"] != source {
					t.Fatalf("warning lost sync context: %v", entry)
				}
				if entry["elapsed"] != float64(syncWarningAfter)/float64(zerolog.DurationFieldUnit) {
					t.Fatalf("unexpected warning elapsed time: %v", entry)
				}
				select {
				case <-done:
					t.Fatal("warning returned before the sync finished")
				default:
				}
				if d.limiter.sem.TryAcquire(1) {
					d.limiter.Release(1)
					t.Fatal("warning released the active sync's semaphore")
				}
				guarded.Run()
				if calls.Load() != 1 {
					t.Fatal("slow-sync warning permitted an overlapping cron job")
				}
				time.Sleep(syncWarningAfter)
				synctest.Wait()
				if entries := logs.entries(t); len(entries) != 1 {
					t.Fatalf("one slow job produced repeated warnings: %v", entries)
				}
				finish()
				<-done
				if !d.limiter.sem.TryAcquire(1) {
					t.Fatal("finished sync retained the semaphore")
				}
				d.limiter.Release(1)
			})
		})
	}
}

func TestSyncWarningFastCompletionStopsTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logs := &syncWarningRecorder{}
		d := daemon{log: zerolog.New(logs)}
		want := errors.New("sync failure")
		err := d.runSyncWithWarning("test-drive", "partial", func() error {
			time.Sleep(time.Minute)
			return want
		})
		if !errors.Is(err, want) {
			t.Fatalf("warning wrapper changed sync error: %v", err)
		}
		time.Sleep(2 * syncWarningAfter)
		synctest.Wait()
		if entries := logs.entries(t); len(entries) != 0 {
			t.Fatalf("completed sync produced a warning: %v", entries)
		}
	})
}

type syncWarningRecorder struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (r *syncWarningRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buffer.Write(p)
}

func (r *syncWarningRecorder) entries(t *testing.T) []map[string]any {
	t.Helper()
	r.mu.Lock()
	data := append([]byte(nil), r.buffer.Bytes()...)
	r.mu.Unlock()
	var entries []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("decode warning log: %v", err)
		}
		entries = append(entries, entry)
	}
	return entries
}
