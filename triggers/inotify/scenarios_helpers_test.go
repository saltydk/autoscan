package inotify

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/saltydk/autoscan"
)

// Native watcher workers have no stop API. Separate test processes contain their
// descriptors and goroutines, including the worker's closed-channel failure.
func runInotifyScenario(t *testing.T) bool {
	t.Helper()
	if os.Getenv("AUTOSCAN_INOTIFY_TEST_CHILD") == "1" {
		return true
	}
	parts := strings.Split(t.Name(), "/")
	for i, part := range parts {
		parts[i] = "^" + regexp.QuoteMeta(part) + "$"
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0],
		"-test.run="+strings.Join(parts, "/"), "-test.v", "-test.count=1", "-test.timeout=40s")
	cmd.Env = append(os.Environ(), "AUTOSCAN_INOTIFY_TEST_CHILD=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Errorf("isolated scenario failed: %v\n%s", err, output)
	} else {
		t.Logf("isolated scenario passed:\n%s", output)
	}
	return false
}

func inotifyScenarioRoot(root string) path {
	return path{Path: root, Rewriter: func(value string) string { return value }, Allowed: func(string) bool { return true }}
}

func newInotifyScenarioDaemon(t *testing.T, paths ...path) *daemon {
	t.Helper()
	d := &daemon{
		paths: paths,
		log:   zerolog.Nop(),
		queue: &queue{inputs: make(chan string, 1024)},
	}
	if err := d.startMonitoring(); err != nil {
		t.Fatalf("start real filesystem monitoring: %v", err)
	}
	// The subprocess owns cleanup because worker cannot yet stop on watcher.Close.
	return d
}

// Event translation tests inspect queue submissions before debounce. Queue
// timing and callback delivery are exercised separately and through public New.
func collectInotifyScenarioFolders(d *daemon) []string {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	var folders []string
	for {
		select {
		case folder := <-d.queue.inputs:
			folders = append(folders, folder)
		case <-timer.C:
			return uniqueInotifyScenarioFolders(folders)
		}
	}
}

func uniqueInotifyScenarioFolders(folders []string) []string {
	folders = slices.Clone(folders)
	slices.Sort(folders)
	return slices.Compact(folders)
}

func assertInotifyScenarioFolders(t *testing.T, got, want []string) {
	t.Helper()
	got = uniqueInotifyScenarioFolders(got)
	want = uniqueInotifyScenarioFolders(want)
	if !slices.Equal(got, want) {
		t.Errorf("queued folders = %q, want %q", got, want)
	}
}

func awaitInotifyScenario(t *testing.T, description string, ready func() bool) {
	t.Helper()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !ready() {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s", description)
		}
	}
}

type inotifyScenarioRecorder struct {
	mu    sync.Mutex
	scans []autoscan.Scan
}

func (r *inotifyScenarioRecorder) callback(scans ...autoscan.Scan) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scans = append(r.scans, scans...)
	return nil
}

func (r *inotifyScenarioRecorder) snapshot() []autoscan.Scan {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.scans)
}

func (r *inotifyScenarioRecorder) folders() []string {
	var folders []string
	for _, scan := range r.snapshot() {
		folders = append(folders, scan.Folder)
	}
	return uniqueInotifyScenarioFolders(folders)
}

func startPublicInotifyScenario(t *testing.T, config Config) *inotifyScenarioRecorder {
	t.Helper()
	config.Verbosity = "disabled"
	trigger, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	recorder := &inotifyScenarioRecorder{}
	trigger(recorder.callback)
	return recorder
}

func inotifyScenarioMakeDir(t *testing.T, directory string) {
	t.Helper()
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
}

func inotifyScenarioWrite(t *testing.T, file string) {
	t.Helper()
	if err := os.WriteFile(file, []byte("media fixture"), 0600); err != nil {
		t.Fatal(err)
	}
}

func inotifyScenarioRename(t *testing.T, old, next string) {
	t.Helper()
	if err := os.Rename(old, next); err != nil {
		t.Fatal(err)
	}
}

func inotifyScenarioRemove(t *testing.T, file string) {
	t.Helper()
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
}

func inotifyScenarioWaitForWatch(t *testing.T, d *daemon, directory string) {
	t.Helper()
	awaitInotifyScenario(t, fmt.Sprintf("recursive watch on %s", directory), func() bool {
		return slices.Contains(d.watcher.WatchList(), directory)
	})
}
