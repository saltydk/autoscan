package triggers_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/saltydk/autoscan"
	"github.com/saltydk/autoscan/migrate"
	"github.com/saltydk/autoscan/processor"
	"github.com/saltydk/autoscan/triggers/a_train"
	"github.com/saltydk/autoscan/triggers/lidarr"
	"github.com/saltydk/autoscan/triggers/manual"
	"github.com/saltydk/autoscan/triggers/radarr"
	"github.com/saltydk/autoscan/triggers/readarr"
	"github.com/saltydk/autoscan/triggers/sonarr"
	_ "modernc.org/sqlite"
)

type compatibilityWebhook struct {
	name       string
	fixture    string
	folder     string
	wantScans  int
	newTrigger func([]autoscan.Rewrite, int) (autoscan.HTTPTrigger, error)
}

var compatibilityWebhooks = []compatibilityWebhook{
	{"manual", "", "Movies/Dr.Strangelove.1964", 1,
		func(rules []autoscan.Rewrite, priority int) (autoscan.HTTPTrigger, error) {
			return manual.New(manual.Config{Rewrite: rules, Priority: priority})
		}},
	{"radarr", "radarr/testdata/interstellar.json", "Movies/Dr.Strangelove.1964", 1,
		func(rules []autoscan.Rewrite, priority int) (autoscan.HTTPTrigger, error) {
			return radarr.New(radarr.Config{Rewrite: rules, Priority: priority})
		}},
	{"sonarr", "sonarr/testdata/westworld.json", "TV/Mr.Robot/Season.01", 1,
		func(rules []autoscan.Rewrite, priority int) (autoscan.HTTPTrigger, error) {
			return sonarr.New(sonarr.Config{Rewrite: rules, Priority: priority})
		}},
	{"lidarr", "lidarr/testdata/marshmello.json", "Music/A.C.D.C/Back.in.Black.1980", 1,
		func(rules []autoscan.Rewrite, priority int) (autoscan.HTTPTrigger, error) {
			return lidarr.New(lidarr.Config{Rewrite: rules, Priority: priority})
		}},
	{"readarr", "readarr/testdata/sanderson.json", "Books/J.R.R.Tolkien/The.Lord.of.the.Rings.1954", 1,
		func(rules []autoscan.Rewrite, priority int) (autoscan.HTTPTrigger, error) {
			return readarr.New(readarr.Config{Rewrite: rules, Priority: priority})
		}},
	{"a-train", "a_train/testdata/modified.json", "TV/Mr.Robot/Season.01", 2,
		func(rules []autoscan.Rewrite, priority int) (autoscan.HTTPTrigger, error) {
			return a_train.New(a_train.Config{Rewrite: rules, Priority: priority})
		}},
}

func TestWebhookMediaPathsAndConfiguredRegexCompatibility(t *testing.T) {
	for _, webhook := range compatibilityWebhooks {
		for _, rewrite := range []struct {
			name    string
			source  string
			dest    string
			pattern string
			to      string
		}{
			{"unchanged_dotted_folder", "/media2/", "/media2/", "", ""},
			{"loose_regex_matches_sibling", "/media2/", "/mnt/unionfs/Media2/", "^/media", "/mnt/unionfs/Media"},
			{"bounded_regex_preserves_sibling", "/media2/", "/media2/", "^/media/", "/mnt/unionfs/Media/"},
			{"bounded_regex_rewrites_library", "/media/", "/mnt/unionfs/Media/", "^/media/", "/mnt/unionfs/Media/"},
			{"capture_regex_preserves_media_folder", "/media/", "/mnt/unionfs/Media/", "^/media/(.*)$", "/mnt/unionfs/Media/$1"},
		} {
			for _, independent := range []bool{false, true} {
				queueName := "legacy_queue"
				if independent {
					queueName = "independent_target_queues"
				}
				t.Run(webhook.name+"/"+rewrite.name+"/"+queueName, func(t *testing.T) {
					proc := compatibilityProcessor(t, independent)
					var received []autoscan.Scan
					callback := func(scans ...autoscan.Scan) error {
						received = append(received, scans...)
						return proc.Add(scans...)
					}
					var rules []autoscan.Rewrite
					if rewrite.pattern != "" {
						rules = []autoscan.Rewrite{{From: rewrite.pattern, To: rewrite.to}}
					}
					const priority = 7
					trigger, err := webhook.newTrigger(rules, priority)
					if err != nil {
						t.Fatal(err)
					}
					request := compatibilityRequest(t, webhook, rewrite.source+webhook.folder, "media2")
					response := httptest.NewRecorder()
					compatibilityHandler(webhook.name, trigger(callback)).ServeHTTP(response, request)
					if response.Code != http.StatusOK {
						t.Fatalf("webhook status = %d, want 200", response.Code)
					}
					wantFolder := rewrite.dest + webhook.folder
					if len(received) != webhook.wantScans {
						t.Fatalf("callback scans = %d, want %d", len(received), webhook.wantScans)
					}
					compatibilityAssertScans(t, received, wantFolder, priority)
					if independent {
						for _, targetID := range []string{"plex", "jellyfin"} {
							target := &compatibilityTarget{}
							if err := proc.ProcessTarget(targetID, target); err != nil {
								t.Fatal(err)
							}
							if len(target.scans) != 1 {
								t.Fatalf("target %s scans = %d, want 1", targetID, len(target.scans))
							}
							compatibilityAssertScans(t, target.scans, wantFolder, priority)
						}
					} else {
						target := &compatibilityTarget{}
						if err := proc.Process([]autoscan.Target{target}); err != nil {
							t.Fatal(err)
						}
						if len(target.scans) != 1 {
							t.Fatalf("legacy target scans = %d, want 1", len(target.scans))
						}
						compatibilityAssertScans(t, target.scans, wantFolder, priority)
					}
					if remaining, err := proc.ScansRemaining(); err != nil || remaining != 0 {
						t.Errorf("remaining scans = %d, error = %v, want empty queue", remaining, err)
					}
				})
			}
		}
	}
}

func TestATrainSelectsDriveRewriteByExactID(t *testing.T) {
	for _, drive := range []struct{ id, destination string }{
		{"media", "/first/"}, {"media2", "/second/"}, {"media3", "/global/"},
	} {
		t.Run(drive.id, func(t *testing.T) {
			trigger, err := a_train.New(a_train.Config{
				Priority: 9,
				Rewrite:  []autoscan.Rewrite{{From: "^/media/", To: "/global/"}},
				Drives: []a_train.Drive{
					{ID: "media", Rewrite: []autoscan.Rewrite{{From: "^/media/", To: "/first/"}}},
					{ID: "media2", Rewrite: []autoscan.Rewrite{{From: "^/media/", To: "/second/"}}},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			var received []autoscan.Scan
			handler := compatibilityHandler("a-train", trigger(func(scans ...autoscan.Scan) error {
				received = append(received, scans...)
				return nil
			}))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, compatibilityRequest(t, compatibilityWebhooks[5], "/media/TV/Mr.Robot/Season.01", drive.id))
			if response.Code != http.StatusOK || len(received) != 2 {
				t.Fatalf("status = %d, scans = %d", response.Code, len(received))
			}
			compatibilityAssertScans(t, received, drive.destination+"TV/Mr.Robot/Season.01", 9)
		})
	}
}

func compatibilityHandler(name string, handler http.Handler) http.Handler {
	if name != "a-train" {
		return handler
	}
	router := chi.NewRouter()
	router.Post("/triggers/a-train/{drive}", handler.ServeHTTP)
	return router
}

func compatibilityRequest(t *testing.T, webhook compatibilityWebhook, folder, drive string) *http.Request {
	t.Helper()
	if webhook.name == "manual" {
		return httptest.NewRequest(http.MethodPost, "/triggers/manual?"+url.Values{"dir": []string{folder}}.Encode(), nil)
	}
	data, err := os.ReadFile(webhook.fixture)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	switch webhook.name {
	case "radarr":
		payload["movie"].(map[string]any)["folderPath"] = folder
	case "sonarr":
		payload["series"].(map[string]any)["path"] = path.Dir(folder)
		payload["episodeFile"].(map[string]any)["relativePath"] = path.Base(folder) + "/Mr.Robot.S01E01.mkv"
	case "lidarr", "readarr":
		key := "trackFiles"
		if webhook.name == "readarr" {
			key = "bookFiles"
		}
		for _, item := range payload[key].([]any) {
			file := item.(map[string]any)
			file["path"] = path.Join(folder, path.Base(file["path"].(string)))
		}
	case "a-train":
		payload["created"] = []string{folder}
		payload["deleted"] = []string{folder}
	}
	data, err = json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	requestPath := "/triggers/" + webhook.name
	if webhook.name == "a-train" {
		requestPath += "/" + drive
	}
	request := httptest.NewRequest(http.MethodPost, requestPath, bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	return request
}

func compatibilityProcessor(t *testing.T, independent bool) *processor.Processor {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	mg, err := migrate.New(db, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	proc, err := processor.New(processor.Config{Db: db, Mg: mg})
	if err != nil {
		t.Fatal(err)
	}
	if independent {
		if err := proc.ConfigureTargets([]string{"plex", "jellyfin"}); err != nil {
			t.Fatal(err)
		}
	}
	return proc
}

func compatibilityAssertScans(t *testing.T, scans []autoscan.Scan, folder string, priority int) {
	t.Helper()
	for _, scan := range scans {
		if scan.Folder != folder || scan.Priority != priority || scan.Time.IsZero() {
			t.Errorf("scan = %+v, want folder %q and priority %d with timestamp", scan, folder, priority)
		}
		if !strings.HasPrefix(scan.Folder, "/") {
			t.Errorf("scan has non-absolute media path %q", scan.Folder)
		}
	}
}

type compatibilityTarget struct{ scans []autoscan.Scan }

func (target *compatibilityTarget) Available() error { return nil }
func (target *compatibilityTarget) Scan(scan autoscan.Scan) error {
	target.scans = append(target.scans, scan)
	return nil
}
