package inotify

import (
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/rs/zerolog"

	"github.com/saltydk/autoscan"
)

func TestInotifyDirectoryHistorySuppressesDuplicateRemoval(t *testing.T) {
	const movie = "/media/Movies/Example.Movie.2026.1080p"
	d := daemon{
		paths:              []path{inotifyScenarioRoot("/media/Movies")},
		queue:              &queue{inputs: make(chan string, 2)},
		retiredDirectories: map[string]time.Time{movie: time.Now().Add(directoryHistoryTTL)},
	}
	for _, op := range []fsnotify.Op{fsnotify.Rename, fsnotify.Remove, fsnotify.Rename} {
		if err := d.handleEvent(fsnotify.Event{Name: movie, Op: op}); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case folder := <-d.queue.inputs:
		t.Fatalf("duplicate directory event widened its scan to %q", folder)
	default:
	}
}

func TestInotifyDirectoryHistoryExpiresAndStaysBounded(t *testing.T) {
	const keep = "/media/TV/Example Show/Season 01"
	now := time.Now()
	d := daemon{retiredDirectories: make(map[string]time.Time)}
	for i := range directoryHistoryLimit + 2 {
		name := fmt.Sprintf("/media/Movies/Example.Movie.%04d", i)
		d.retiredDirectories[name] = now.Add(10*time.Second + time.Duration(i))
	}
	d.retiredDirectories[keep] = now.Add(directoryHistoryTTL)
	const expired = "/media/Movies/Expired Movie (2020)"
	d.retiredDirectories[expired] = now.Add(-time.Nanosecond)
	d.trimDirectoryHistory(keep)
	if len(d.retiredDirectories) != directoryHistoryLimit {
		t.Fatalf("history retained %d entries, want %d", len(d.retiredDirectories), directoryHistoryLimit)
	}
	if _, exists := d.retiredDirectories[keep]; !exists {
		t.Fatal("history cap discarded the directory currently being processed")
	}
	if _, exists := d.retiredDirectories[expired]; exists {
		t.Fatal("history retained an expired directory")
	}
	for i := range 3 {
		if _, exists := d.retiredDirectories[fmt.Sprintf("/media/Movies/Example.Movie.%04d", i)]; exists {
			t.Fatalf("history retained older entry %d instead of a newer entry", i)
		}
	}
}

func TestInotifyOwnedQueueStopsWithoutClosingBorrowedInputs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newQueue(func(...autoscan.Scan) error {
			t.Error("queue submitted a pending scan during shutdown")
			return nil
		}, zerolog.Nop(), 0)
		q.inputs <- "/media/TV/Example Show/Season 01"
		synctest.Wait()
		q.stop()
		q.stop()
		select {
		case <-q.done:
		default:
			t.Fatal("owned queue worker did not stop")
		}
		time.Sleep(queueDebounceDelay)
	})

	borrowed := &queue{inputs: make(chan string, 1)}
	borrowed.stop()
	borrowed.inputs <- "/media/Movies/Example Movie (2026)"
	if folder := <-borrowed.inputs; folder != "/media/Movies/Example Movie (2026)" {
		t.Fatalf("shutdown changed a borrowed queue's input: %q", folder)
	}
}
