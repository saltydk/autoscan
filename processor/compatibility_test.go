package processor

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/saltydk/autoscan"
	"github.com/saltydk/autoscan/migrate"
)

// This fixture was created by autoscan at ddc2514 using modernc.org/sqlite
// v1.18.2, through processor.New and Processor.Add. Keep it unchanged so driver
// updates are tested against the original on-disk timestamp representation.
func TestLegacyDatabase(t *testing.T) {
	data, err := os.ReadFile("testdata/legacy-v1.18.2.db")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "autoscan.db")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}

	want := autoscan.Scan{
		Folder:   "/media/Movies/Existing",
		Priority: 5,
		Time:     time.Date(2026, 9, 1, 12, 34, 56, 123456789, time.UTC),
	}
	for _, phase := range []string{"upgrade", "reopen"} {
		t.Run(phase, func(t *testing.T) {
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			mg, err := migrate.New(db, "migrations")
			if err != nil {
				t.Fatal(err)
			}
			store, err := newDatastore(db, mg)
			if err != nil {
				t.Fatal(err)
			}
			scans, err := store.GetAll()
			if err != nil {
				t.Fatal(err)
			}
			if len(scans) != 1 || scans[0].Folder != want.Folder || scans[0].Priority != want.Priority || !scans[0].Time.Equal(want.Time) {
				t.Fatalf("queued scans = %+v, want %+v", scans, want)
			}
			if phase == "upgrade" {
				want.Priority = 7
				want.Time = want.Time.Add(time.Minute)
				if err := store.Upsert([]autoscan.Scan{want}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
