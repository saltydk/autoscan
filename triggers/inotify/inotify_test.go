package inotify

import (
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/saltydk/autoscan"
)

func TestQueueRetainsFailedScan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const folder = "/media/movies"
		attempts := 0
		q := &queue{
			callback: func(scans ...autoscan.Scan) error {
				attempts++
				if len(scans) != 1 || scans[0].Folder != folder || scans[0].Priority != 3 {
					t.Fatalf("callback received unexpected scans: %+v", scans)
				}
				if attempts == 1 {
					return errors.New("temporary persistence failure")
				}
				return nil
			},
			log:      zerolog.Nop(),
			priority: 3,
			scans:    map[string]time.Time{folder: time.Now()},
			lock:     &sync.Mutex{},
		}

		q.process()
		retryAt, ok := q.scans[folder]
		if !ok {
			t.Fatal("failed processor handoff discarded the scan")
		}
		if !retryAt.After(time.Now()) || retryAt.After(time.Now().Add(time.Minute)) {
			t.Fatalf("retry deadline %v is not a bounded delay", retryAt)
		}

		q.process()
		if attempts != 1 {
			t.Fatalf("retried before the deadline: got %d attempts", attempts)
		}

		time.Sleep(time.Until(retryAt))
		q.process()
		if attempts != 2 {
			t.Fatalf("got %d attempts, want 2", attempts)
		}
		if len(q.scans) != 0 {
			t.Fatalf("successful handoff left queued scans: %v", q.scans)
		}
	})
}

func TestQueueWorkerRetriesAndAcceptsNewEvents(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var attemptsMu sync.Mutex
		attempts := make(map[string]int)
		q := newQueue(func(scans ...autoscan.Scan) error {
			attemptsMu.Lock()
			defer attemptsMu.Unlock()
			folder := scans[0].Folder
			attempts[folder]++
			if folder == "/media/first" && attempts[folder] == 1 {
				return errors.New("temporary persistence failure")
			}
			return nil
		}, zerolog.Nop(), 0)
		defer close(q.inputs)
		assertAttempts := func(first, second int) {
			t.Helper()
			attemptsMu.Lock()
			defer attemptsMu.Unlock()
			if attempts["/media/first"] != first || attempts["/media/second"] != second {
				t.Fatalf("unexpected handoff attempts: got %v, want first=%d second=%d", attempts, first, second)
			}
		}

		q.inputs <- "/media/first"
		synctest.Wait()
		time.Sleep(queueDebounceDelay - time.Nanosecond)
		synctest.Wait()
		assertAttempts(0, 0)

		time.Sleep(time.Nanosecond)
		synctest.Wait()
		assertAttempts(1, 0)
		q.inputs <- "/media/second"
		synctest.Wait()

		time.Sleep(queueRetryDelay - time.Nanosecond)
		synctest.Wait()
		assertAttempts(1, 0)
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		assertAttempts(2, 0)

		time.Sleep(queueDebounceDelay - queueRetryDelay)
		synctest.Wait()
		assertAttempts(2, 1)
		q.lock.Lock()
		remaining := len(q.scans)
		q.lock.Unlock()
		if remaining != 0 {
			t.Fatalf("successful handoffs left %d scans queued", remaining)
		}
	})
}
