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

func TestQueueScenarios(t *testing.T) {
	scenarios := []struct {
		name string
		run  func(*testing.T)
	}{
		{
			name: "idle worker blocks without callback",
			run: func(t *testing.T) {
				r := &queueScenarioRecorder{}
				_, _, stop := queueScenarioStart(r.handoff, 0)
				defer stop()
				synctest.Wait()
				queueScenarioAdvance(time.Hour)
				queueScenarioAssertCalls(t, r)
			},
		},
		{
			name: "season episode burst coalesces and resets debounce",
			run: func(t *testing.T) {
				r := &queueScenarioRecorder{}
				q, _, stop := queueScenarioStart(r.handoff, 0)
				defer stop()
				const season = "/media/TV/Show/Season 01"
				queueScenarioSend(q, season)
				for range 4 {
					queueScenarioAdvance(time.Second)
					queueScenarioSend(q, season)
				}
				queueScenarioAdvance(queueDebounceDelay - time.Nanosecond)
				queueScenarioAssertCalls(t, r)
				queueScenarioAdvance(time.Nanosecond)
				queueScenarioAssertCalls(t, r, season)
				queueScenarioAdvance(2 * queueDebounceDelay)
				queueScenarioAssertCalls(t, r, season)
			},
		},
		{
			name: "separate folders have independent deadlines",
			run: func(t *testing.T) {
				r := &queueScenarioRecorder{}
				q, _, stop := queueScenarioStart(r.handoff, 0)
				defer stop()
				queueScenarioSend(q, "/media/Movies/First Movie (2024)")
				queueScenarioAdvance(2 * time.Second)
				queueScenarioSend(q, "/media/TV/Show/Season 02")
				queueScenarioAdvance(queueDebounceDelay - 2*time.Second)
				queueScenarioAssertCalls(t, r, "/media/Movies/First Movie (2024)")
				queueScenarioAdvance(2 * time.Second)
				queueScenarioAssertCalls(t, r, "/media/Movies/First Movie (2024)", "/media/TV/Show/Season 02")
			},
		},
		{
			name: "event accepted at deadline before dispatch resets debounce",
			run: func(t *testing.T) {
				r := &queueScenarioRecorder{}
				q := queueScenarioBare(r.handoff, 0)
				q.add("/media/TV/Show/Season 01")
				queueScenarioAdvance(queueDebounceDelay)
				// Control ordering without racing an input and timer in the worker's select.
				q.add("/media/TV/Show/Season 01")
				q.process()
				queueScenarioAssertCalls(t, r)
				queueScenarioAdvance(queueDebounceDelay)
				q.process()
				queueScenarioAssertCalls(t, r, "/media/TV/Show/Season 01")
			},
		},
		{
			name: "event after deadline handoff starts a new batch",
			run: func(t *testing.T) {
				r := &queueScenarioRecorder{}
				q, _, stop := queueScenarioStart(r.handoff, 0)
				defer stop()
				queueScenarioSend(q, "/media/TV/Show/Season 01")
				queueScenarioAdvance(queueDebounceDelay)
				queueScenarioAssertCalls(t, r, "/media/TV/Show/Season 01")
				queueScenarioSend(q, "/media/TV/Show/Season 01")
				queueScenarioAdvance(queueDebounceDelay - time.Nanosecond)
				queueScenarioAssertCalls(t, r, "/media/TV/Show/Season 01")
				queueScenarioAdvance(time.Nanosecond)
				queueScenarioAssertCalls(t, r, "/media/TV/Show/Season 01", "/media/TV/Show/Season 01")
			},
		},
		{
			name: "handoff preserves full folder path priority and dispatch time",
			run: func(t *testing.T) {
				r := &queueScenarioRecorder{}
				q, _, stop := queueScenarioStart(r.handoff, 7)
				defer stop()
				started := time.Now()
				queueScenarioSend(q, "/media/TV/Show/Season 01")
				queueScenarioAdvance(queueDebounceDelay)
				queueScenarioAssertCalls(t, r, "/media/TV/Show/Season 01")
				scan := r.snapshot()[0]
				if scan.Priority != 7 || !scan.Time.Equal(started.Add(queueDebounceDelay)) {
					t.Fatalf("unexpected scan metadata: %+v", scan)
				}
			},
		},
		{
			name: "multiple failures retain scan until recovery",
			run: func(t *testing.T) {
				r := &queueScenarioRecorder{failures: map[string]int{"/media/TV/Show/Season 01": 3}}
				q, _, stop := queueScenarioStart(r.handoff, 0)
				defer stop()
				started := time.Now()
				queueScenarioSend(q, "/media/TV/Show/Season 01")
				queueScenarioAdvance(queueDebounceDelay)
				for expected := 2; expected <= 4; expected++ {
					queueScenarioAdvance(queueRetryDelay - time.Nanosecond)
					if calls := len(r.snapshot()); calls != expected-1 {
						t.Fatalf("retry ran before its deadline: got %d calls, want %d", calls, expected-1)
					}
					queueScenarioAdvance(time.Nanosecond)
					if calls := len(r.snapshot()); calls != expected {
						t.Fatalf("retry did not run: got %d calls, want %d", calls, expected)
					}
				}
				for i, scan := range r.snapshot() {
					want := started.Add(queueDebounceDelay + time.Duration(i)*queueRetryDelay)
					if scan.Folder != "/media/TV/Show/Season 01" || !scan.Time.Equal(want) {
						t.Fatalf("retry %d has unexpected scan: %+v, want time %v", i, scan, want)
					}
				}
				queueScenarioAdvance(time.Minute)
				queueScenarioAssertCalls(t, r, "/media/TV/Show/Season 01", "/media/TV/Show/Season 01", "/media/TV/Show/Season 01", "/media/TV/Show/Season 01")
			},
		},
		{
			name: "failure in one folder does not suppress another",
			run: func(t *testing.T) {
				r := &queueScenarioRecorder{failures: map[string]int{"/media/TV/Failing Show/Season 01": 1}}
				q, _, stop := queueScenarioStart(r.handoff, 0)
				defer stop()
				queueScenarioSend(q, "/media/TV/Failing Show/Season 01")
				queueScenarioSend(q, "/media/Movies/Successful Movie (2024)")
				queueScenarioAdvance(queueDebounceDelay)
				queueScenarioAssertCounts(t, r, map[string]int{"/media/TV/Failing Show/Season 01": 1, "/media/Movies/Successful Movie (2024)": 1})
				queueScenarioAdvance(queueRetryDelay)
				queueScenarioAssertCounts(t, r, map[string]int{"/media/TV/Failing Show/Season 01": 2, "/media/Movies/Successful Movie (2024)": 1})
			},
		},
		{
			name: "new folder event is accepted while retry is pending",
			run: func(t *testing.T) {
				r := &queueScenarioRecorder{failures: map[string]int{"/media/TV/Failing Show/Season 01": 1}}
				q, _, stop := queueScenarioStart(r.handoff, 0)
				defer stop()
				queueScenarioSend(q, "/media/TV/Failing Show/Season 01")
				queueScenarioAdvance(queueDebounceDelay)
				queueScenarioAdvance(2 * time.Second)
				queueScenarioSend(q, "/media/TV/New Show/Season 01")
				queueScenarioAdvance(queueRetryDelay - 2*time.Second)
				queueScenarioAssertCalls(t, r, "/media/TV/Failing Show/Season 01", "/media/TV/Failing Show/Season 01")
				queueScenarioAdvance(queueDebounceDelay - queueRetryDelay + 2*time.Second)
				queueScenarioAssertCalls(t, r, "/media/TV/Failing Show/Season 01", "/media/TV/Failing Show/Season 01", "/media/TV/New Show/Season 01")
			},
		},
		{
			name: "same folder event during retry starts a fresh debounce",
			run: func(t *testing.T) {
				r := &queueScenarioRecorder{failures: map[string]int{"/media/TV/Show/Season 01": 1}}
				q, _, stop := queueScenarioStart(r.handoff, 0)
				defer stop()
				queueScenarioSend(q, "/media/TV/Show/Season 01")
				queueScenarioAdvance(queueDebounceDelay)
				queueScenarioAdvance(2 * time.Second)
				queueScenarioSend(q, "/media/TV/Show/Season 01")
				// New season activity deliberately replaces the previous retry deadline.
				queueScenarioAdvance(queueRetryDelay - 2*time.Second)
				queueScenarioAssertCalls(t, r, "/media/TV/Show/Season 01")
				queueScenarioAdvance(queueDebounceDelay - queueRetryDelay + 2*time.Second)
				queueScenarioAssertCalls(t, r, "/media/TV/Show/Season 01", "/media/TV/Show/Season 01")
			},
		},
		{
			name: "closing an idle input stops the worker",
			run: func(t *testing.T) {
				r := &queueScenarioRecorder{}
				_, done, stop := queueScenarioStart(r.handoff, 0)
				defer stop()
				stop()
				synctest.Wait()
				queueScenarioAssertStopped(t, done)
				queueScenarioAdvance(time.Hour)
				queueScenarioAssertCalls(t, r)
			},
		},
		{
			name: "closing input cancels pending debounce",
			run: func(t *testing.T) {
				r := &queueScenarioRecorder{}
				q, done, stop := queueScenarioStart(r.handoff, 0)
				defer stop()
				queueScenarioSend(q, "/media/TV/Show/Season 01")
				stop()
				synctest.Wait()
				queueScenarioAssertStopped(t, done)
				queueScenarioAdvance(2 * queueDebounceDelay)
				queueScenarioAssertCalls(t, r)
			},
		},
		{
			name: "closing input cancels pending retry",
			run: func(t *testing.T) {
				r := &queueScenarioRecorder{failures: map[string]int{"/media/TV/Show/Season 01": 1}}
				q, done, stop := queueScenarioStart(r.handoff, 0)
				defer stop()
				queueScenarioSend(q, "/media/TV/Show/Season 01")
				queueScenarioAdvance(queueDebounceDelay)
				stop()
				synctest.Wait()
				queueScenarioAssertStopped(t, done)
				queueScenarioAdvance(2 * queueRetryDelay)
				queueScenarioAssertCalls(t, r, "/media/TV/Show/Season 01")
			},
		},
		{
			name: "slow handoff backpressure preserves the next event",
			run: func(t *testing.T) {
				r := &queueScenarioRecorder{}
				entered := make(chan struct{})
				release := make(chan struct{})
				var releaseOnce sync.Once
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				q, _, stop := queueScenarioStart(func(scans ...autoscan.Scan) error {
					if err := r.handoff(scans...); err != nil {
						return err
					}
					if scans[0].Folder == "/media/TV/Slow Show/Season 01" {
						close(entered)
						<-release
					}
					return nil
				}, 0)
				defer stop()
				defer unblock()
				queueScenarioSend(q, "/media/TV/Slow Show/Season 01")
				queueScenarioAdvance(queueDebounceDelay)
				<-entered
				sent := make(chan struct{})
				go func() {
					q.inputs <- "/media/TV/Next Show/Season 01"
					close(sent)
				}()
				synctest.Wait()
				queueScenarioAdvance(time.Minute)
				queueScenarioAssertCalls(t, r, "/media/TV/Slow Show/Season 01")
				unblock()
				synctest.Wait()
				select {
				case <-sent:
				default:
					t.Fatal("worker did not accept the waiting event after callback recovery")
				}
				queueScenarioAdvance(queueDebounceDelay)
				queueScenarioAssertCalls(t, r, "/media/TV/Slow Show/Season 01", "/media/TV/Next Show/Season 01")
			},
		},
		{
			name: "successful handoff is not duplicated later",
			run: func(t *testing.T) {
				r := &queueScenarioRecorder{}
				q, _, stop := queueScenarioStart(r.handoff, 0)
				defer stop()
				queueScenarioSend(q, "/media/TV/Show/Season 01")
				queueScenarioAdvance(queueDebounceDelay)
				queueScenarioAssertCalls(t, r, "/media/TV/Show/Season 01")
				queueScenarioAdvance(time.Hour)
				queueScenarioAssertCalls(t, r, "/media/TV/Show/Season 01")
			},
		},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			synctest.Test(t, scenario.run)
		})
	}
}

type queueScenarioRecorder struct {
	mu       sync.Mutex
	calls    []autoscan.Scan
	failures map[string]int
}

func (r *queueScenarioRecorder) handoff(scans ...autoscan.Scan) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, scans...)
	for _, scan := range scans {
		if r.failures[scan.Folder] > 0 {
			r.failures[scan.Folder]--
			return errors.New("temporary handoff failure")
		}
	}
	return nil
}

func (r *queueScenarioRecorder) snapshot() []autoscan.Scan {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]autoscan.Scan(nil), r.calls...)
}

func queueScenarioBare(callback autoscan.ProcessorFunc, priority int) *queue {
	return &queue{
		callback: callback,
		log:      zerolog.Nop(),
		priority: priority,
		inputs:   make(chan string),
		scans:    make(map[string]time.Time),
		lock:     &sync.Mutex{},
	}
}

func queueScenarioStart(callback autoscan.ProcessorFunc, priority int) (*queue, <-chan struct{}, func()) {
	q := queueScenarioBare(callback, priority)
	done := make(chan struct{})
	go func() {
		defer close(done)
		q.worker()
	}()
	var once sync.Once
	stop := func() { once.Do(func() { close(q.inputs) }) }
	return q, done, stop
}

func queueScenarioAdvance(d time.Duration) {
	time.Sleep(d)
	synctest.Wait()
}

func queueScenarioSend(q *queue, folder string) {
	q.inputs <- folder
	synctest.Wait()
}

func queueScenarioAssertCalls(t *testing.T, r *queueScenarioRecorder, folders ...string) {
	t.Helper()
	got := r.snapshot()
	if len(got) != len(folders) {
		t.Fatalf("got %d handoffs, want %d; scans=%+v", len(got), len(folders), got)
	}
	for i, scan := range got {
		if scan.Folder != folders[i] {
			t.Fatalf("handoff %d folder=%q, want %q", i, scan.Folder, folders[i])
		}
	}
}

func queueScenarioAssertCounts(t *testing.T, r *queueScenarioRecorder, want map[string]int) {
	t.Helper()
	got := make(map[string]int)
	for _, scan := range r.snapshot() {
		got[scan.Folder]++
	}
	if len(got) != len(want) {
		t.Fatalf("unexpected handoff counts: got %v, want %v", got, want)
	}
	for folder, count := range want {
		if got[folder] != count {
			t.Fatalf("unexpected handoff counts: got %v, want %v", got, want)
		}
	}
}

func queueScenarioAssertStopped(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	default:
		t.Fatal("queue worker did not stop after input closure")
	}
}
