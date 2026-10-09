package processor

import (
	"testing"
	"time"

	"github.com/saltydk/autoscan"
)

func TestAddEmptyBatchDoesNotAccessDatabase(t *testing.T) {
	for _, independent := range []bool{false, true} {
		name := "shared"
		if independent {
			name = "targets"
		}
		t.Run(name, func(t *testing.T) {
			proc, db := queueProcessor(t, ":memory:", 0)
			if independent {
				if err := proc.ConfigureTargets([]string{"plex"}); err != nil {
					t.Fatal(err)
				}
			}
			db.Close()
			if err := proc.Add(); err != nil {
				t.Fatalf("empty event accessed the unavailable database: %v", err)
			}
			if proc.Metrics().Received != 0 {
				t.Fatal("empty event changed received count")
			}
		})
	}
}

func TestAddFiltersInvalidFoldersAndPreservesValidEvents(t *testing.T) {
	for _, independent := range []bool{false, true} {
		name := "shared"
		if independent {
			name = "targets"
		}
		t.Run(name, func(t *testing.T) {
			proc, db := queueProcessor(t, ":memory:", 0)
			if independent {
				if err := proc.ConfigureTargets([]string{"plex", "emby"}); err != nil {
					t.Fatal(err)
				}
			}
			folder := "/unmounted/Movies/æøå Film.2026"
			at := now().Add(-time.Hour)
			scans := []autoscan.Scan{
				{Folder: "", Time: at}, {Folder: ".", Time: at},
				{Folder: "relative/movie", Time: at}, {Folder: "/media/Bad\x00Path", Time: at},
				{Folder: folder, Priority: 3, Time: at},
				{Folder: folder, Priority: 5, Time: at.Add(time.Second)},
			}
			if err := proc.Add(scans...); err != nil {
				t.Fatal(err)
			}
			if scans[0].Folder != "" || scans[4].Priority != 3 {
				t.Fatal("validation mutated the caller's input slice")
			}
			table, expected := "scan", 1
			if independent {
				table, expected = "target_scan", 2
			}
			var total, valid int
			if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&total); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE folder = ? AND priority = 5 AND time = ?", folder, at.Add(time.Second)).Scan(&valid); err != nil {
				t.Fatal(err)
			}
			if total != expected || valid != expected {
				t.Fatalf("stored total/valid = %d/%d, want %d/%d", total, valid, expected, expected)
			}
			if proc.Metrics().Received != 2 {
				t.Fatalf("received count = %d, want only 2 valid events", proc.Metrics().Received)
			}
		})
	}
}

func TestAddInvalidOnlyBatchDoesNotAccessDatabase(t *testing.T) {
	proc, db := queueProcessor(t, ":memory:", 0)
	db.Close()
	if err := proc.Add(autoscan.Scan{Folder: ""}, autoscan.Scan{Folder: "."}); err != nil {
		t.Fatalf("invalid-only event accessed database: %v", err)
	}
}
