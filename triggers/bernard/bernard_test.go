package bernard

import (
	"database/sql"
	_ "embed"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	lowe "github.com/l3uddz/bernard"
	ds "github.com/l3uddz/bernard/datastore"
	"github.com/l3uddz/bernard/datastore/sqlite"
	"github.com/robfig/cron/v3"
	"github.com/rs/zerolog"

	"github.com/saltydk/autoscan"
)

// Bernard v0.5.1's ordered schema fixture isolates the handoff regression from
// its upstream migrator's random migration-map order. It does not fix bootstrap.
//
//go:embed testdata/bernard-v0.5.1-schema.sql
var partialSyncFixtureSchema string

func TestPartialSyncRetriesHandoffBeforeAdvancingCursor(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(partialSyncFixtureSchema); err != nil {
		t.Fatal(err)
	}
	store := &sqlite.Datastore{DB: db}

	const driveID = "test-drive"
	if err := store.FullSync(ds.Drive{ID: driveID, Name: "test", PageToken: "old"},
		[]ds.Folder{{ID: "movies", Name: "Movies", Parent: driveID}}, nil); err != nil {
		t.Fatal(err)
	}

	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.Path != "/drive/v3/changes" || r.URL.Query().Get("pageToken") != "old" {
			t.Fatalf("unexpected Drive request: %s", r.URL)
		}
		body := `{"newStartPageToken":"new","changes":[{"fileId":"movie","file":{"id":"movie","driveId":"test-drive","name":"movie.mkv","mimeType":"video/x-matroska","parents":["movies"],"size":"42","md5Checksum":"test-md5"}}]}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	})}

	callbackAttempts := 0
	persistenceErr := errors.New("temporary persistence failure")
	scanTime := time.Now()
	d := daemon{
		bernard:  lowe.New(authStub{}, store, lowe.WithClient(client)),
		store:    &bds{store},
		log:      zerolog.Nop(),
		priority: 3,
		callback: func(scans ...autoscan.Scan) error {
			callbackAttempts++
			token, err := store.PageToken(driveID)
			if err != nil {
				t.Fatal(err)
			}
			if token != "old" {
				t.Fatalf("cursor during processor handoff = %q, want old", token)
			}
			if len(scans) != 1 || scans[0] != (autoscan.Scan{Folder: "/Movies", Priority: 3, Time: scanTime}) {
				t.Fatalf("callback received unexpected scans: %+v", scans)
			}
			if callbackAttempts == 1 {
				return persistenceErr
			}
			return nil
		},
	}
	drive := &drive{
		ID:       driveID,
		Rewriter: func(p string) string { return p },
		Allowed:  func(string) bool { return true },
		ScanTime: func() time.Time { return scanTime },
	}
	c := cron.New()
	job := newSyncJob(c, zerolog.Nop(), func() error { return d.partialSync(drive) })
	id, err := c.AddJob("@every 1m", job)
	if err != nil {
		t.Fatal(err)
	}
	job.jobID = id

	job.Run()
	if job.attempts != 1 || len(job.errors) != 1 || !errors.Is(job.errors[0], persistenceErr) {
		t.Fatalf("handoff failure was not preserved for retry: attempts=%d errors=%v", job.attempts, job.errors)
	}
	if errors.Is(job.errors[0], autoscan.ErrFatal) || c.Entry(id).ID != id {
		t.Fatal("temporary handoff failure stopped the sync job")
	}
	if token, err := store.PageToken(driveID); err != nil || token != "old" {
		t.Fatalf("failed handoff advanced cursor: token=%q err=%v", token, err)
	}
	if _, err := d.store.GetFile(driveID, "movie"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("failed handoff persisted Drive changes: %v", err)
	}

	job.Run()
	if callbackAttempts != 2 || requests != 2 {
		t.Fatalf("change was not replayed: callbacks=%d requests=%d", callbackAttempts, requests)
	}
	if token, err := store.PageToken(driveID); err != nil || token != "new" {
		t.Fatalf("successful handoff did not advance cursor: token=%q err=%v", token, err)
	}
	if _, err := d.store.GetFile(driveID, "movie"); err != nil {
		t.Fatalf("successful handoff did not persist Drive changes: %v", err)
	}
	if job.attempts != 0 || len(job.errors) != 0 || c.Entry(id).ID != id {
		t.Fatalf("successful retry did not reset job: attempts=%d errors=%v", job.attempts, job.errors)
	}
}

type authStub struct{}

func (authStub) AccessToken() (string, int64, error) { return "test-token", 0, nil }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
