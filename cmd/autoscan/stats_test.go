package main

import (
	"context"
	"database/sql"
	"testing"
	"testing/synctest"
	"time"

	"github.com/saltydk/autoscan/migrate"
	"github.com/saltydk/autoscan/processor"
)

// A transient database error used to stop scan statistics for the rest of the
// process lifetime.
func TestScanStatsContinueAfterDatabaseError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		mg, err := migrate.New(db, "migrations")
		if err != nil {
			t.Fatal(err)
		}
		proc, err := processor.New(processor.Config{Db: db, Mg: mg})
		if err != nil {
			t.Fatal(err)
		}
		if err := proc.ConfigureTargets([]string{"plex:a"}); err != nil {
			t.Fatal(err)
		}
		db.Close()

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			scanStats(ctx, proc, time.Minute)
			close(done)
		}()
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("scan stats stopped after a database error")
		default:
		}
		cancel()
		<-done
	})
}
