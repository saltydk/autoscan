//go:build linux

package inotify

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/rs/zerolog"

	"github.com/saltydk/autoscan"
)

func TestInotifyLifecycleScenarioWatcherCloseStopsWorker(t *testing.T) {
	t.Parallel()
	if !runInotifyScenario(t) {
		return
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { watcher.Close() })
	d := &daemon{
		watcher: watcher,
		queue:   &queue{inputs: make(chan string, 32)},
		log:     zerolog.Nop(),
	}
	done := make(chan struct{})
	go func() {
		d.worker()
		close(done)
	}()
	if err := watcher.Close(); err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		t.Fatal("daemon worker did not exit after the watcher closed both event channels")
	}
}

func TestInotifyLifecycleScenarioMissingRootClosesWatcher(t *testing.T) {
	t.Parallel()
	if !runInotifyScenario(t) {
		return
	}
	d := &daemon{
		paths: []path{inotifyScenarioRoot(filepath.Join(t.TempDir(), "missing"))},
		queue: &queue{inputs: make(chan string, 32)},
		log:   zerolog.Nop(),
	}
	if err := d.startMonitoring(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing root initialization = %v, want wrapped os.ErrNotExist", err)
	}
	if d.watcher == nil {
		t.Fatal("startup did not create the watcher needed to exercise failure cleanup")
	}
	select {
	case _, open := <-d.watcher.Events:
		if open {
			t.Error("watcher event channel remained open after failed initialization")
		}
	default:
		t.Error("watcher event channel was not closed after failed initialization")
	}
	select {
	case _, open := <-d.watcher.Errors:
		if open {
			t.Error("watcher error channel remained open after failed initialization")
		}
	default:
		t.Error("watcher error channel was not closed after failed initialization")
	}
}

func TestInotifyLifecycleScenarioFailedTriggerStartupStopsQueue(t *testing.T) {
	t.Parallel()
	if !runInotifyScenario(t) {
		return
	}
	baseline := inotifyScenarioQueueWorkers()
	c := Config{Verbosity: "disabled", Paths: []configScenarioPath{{Path: filepath.Join(t.TempDir(), "missing")}}}
	trigger, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		trigger(func(...autoscan.Scan) error {
			t.Error("failed startup unexpectedly submitted a scan")
			return nil
		})
	}
	// Every started goroutine appears in runtime.Stack even before it first runs.
	// Allow a correctly closed queue worker time to observe its closed input channel.
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for {
		runtime.Gosched()
		remaining := inotifyScenarioQueueWorkers() - baseline
		if remaining <= 0 {
			return
		}
		select {
		case <-poll.C:
		case <-deadline.C:
			t.Fatalf("failed trigger startup retained %d queue worker goroutines", remaining)
		}
	}
}

func inotifyScenarioQueueWorkers() int {
	for size := 64 * 1024; ; size *= 2 {
		buffer := make([]byte, size)
		n := runtime.Stack(buffer, true)
		if n < len(buffer) {
			return strings.Count(string(buffer[:n]), "github.com/saltydk/autoscan/triggers/inotify.(*queue).worker(")
		}
	}
}

func TestInotifyLifecycleScenarioExistingDirectoriesAreWatched(t *testing.T) {
	t.Parallel()
	if !runInotifyScenario(t) {
		return
	}
	root := filepath.Join(t.TempDir(), "root")
	show := filepath.Join(root, "Show")
	season := filepath.Join(show, "Season 01")
	if err := os.MkdirAll(season, 0o755); err != nil {
		t.Fatal(err)
	}
	d := newInotifyScenarioDaemon(t, inotifyScenarioRoot(root))
	want := []string{root, show, season}
	slices.Sort(want)
	got := d.watcher.WatchList()
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("startup watch list = %v, want %v", got, want)
	}
}

func TestInotifyLifecycleScenarioRootRecreationLimitation(t *testing.T) {
	t.Parallel()
	if !runInotifyScenario(t) {
		return
	}
	root := filepath.Join(t.TempDir(), "root")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	d := newInotifyScenarioDaemon(t, inotifyScenarioRoot(root))
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	awaitInotifyScenario(t, "deleted root watch removal", func() bool {
		return len(d.watcher.WatchList()) == 0
	})
	// Drain the old root's removal notification before exercising its replacement.
	collectInotifyScenarioFolders(d)
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "new.mkv"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := collectInotifyScenarioFolders(d); len(got) != 0 {
		t.Fatalf("root recreation characterization changed: folders = %v", got)
	}
	if got := d.watcher.WatchList(); len(got) != 0 {
		t.Fatalf("root recreation characterization changed: watches = %v", got)
	}
	t.Log("current limitation: recreated configured roots require trigger restart to regain watches")
}

func TestInotifyLifecycleScenarioSymlinkRootLimitation(t *testing.T) {
	t.Parallel()
	if !runInotifyScenario(t) {
		return
	}
	directory := t.TempDir()
	realRoot := filepath.Join(directory, "real")
	linkRoot := filepath.Join(directory, "linked")
	if err := os.Mkdir(realRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Fatal(err)
	}
	d := newInotifyScenarioDaemon(t, inotifyScenarioRoot(linkRoot))
	if got := d.watcher.WatchList(); len(got) != 0 {
		t.Fatalf("symlink-root characterization changed: watches = %v", got)
	}
	if err := os.WriteFile(filepath.Join(realRoot, "new.mkv"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := collectInotifyScenarioFolders(d); len(got) != 0 {
		t.Fatalf("symlink-root characterization changed: folders = %v", got)
	}
	t.Log("current limitation: configured symlink roots succeed without installing any watches")
}

func TestInotifyLifecycleScenarioWatcherErrorsDoNotStopEvents(t *testing.T) {
	for _, scenario := range []struct {
		name string
		err  error
	}{
		{"ordinary_error", errors.New("scenario watcher error")},
		{"overflow_error", fsnotify.ErrEventOverflow},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			if !runInotifyScenario(t) {
				return
			}
			root := t.TempDir()
			d := newInotifyScenarioDaemon(t, inotifyScenarioRoot(root))
			timer := time.NewTimer(time.Second)
			defer timer.Stop()
			select {
			case d.watcher.Errors <- scenario.err:
			case <-timer.C:
				t.Fatal("daemon did not receive a watcher error")
			}
			if err := os.WriteFile(filepath.Join(root, "after-error.mkv"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			if got := collectInotifyScenarioFolders(d); !slices.Equal(got, []string{root}) {
				t.Fatalf("folders after watcher error = %v, want [%s]", got, root)
			}
		})
	}
}
