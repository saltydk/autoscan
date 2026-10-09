package inotify

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"
	"github.com/saltydk/autoscan"
)

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
