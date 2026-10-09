package a_train

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/saltydk/autoscan"
)

func TestATrainEmptyBatchDoesNotCallProcessor(t *testing.T) {
	for _, body := range []string{`{}`, `{"created":[],"deleted":[]}`, `{"created":null,"deleted":null}`} {
		t.Run(body, func(t *testing.T) {
			trigger, err := New(Config{})
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			handler := trigger(func(...autoscan.Scan) error { calls++; return nil })
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/triggers/a-train/drive", strings.NewReader(body)))
			if response.Code != http.StatusOK || calls != 0 {
				t.Fatalf("status=%d calls=%d, want 200 and no callback", response.Code, calls)
			}
		})
	}
}

func TestATrainRejectsInvalidPathBatchBeforeCallback(t *testing.T) {
	for _, test := range []struct {
		name             string
		created, deleted []string
		rewrite          []autoscan.Rewrite
	}{
		{name: "blank_created", created: []string{""}},
		{name: "blank_deleted", deleted: []string{""}},
		{name: "relative_created", created: []string{"Movies/Example (2026)"}},
		{name: "relative_deleted", deleted: []string{"TV/Example/Season 01"}},
		{name: "relative_must_not_be_salvaged_by_rewrite", created: []string{"Movies/Example (2026)"}, rewrite: []autoscan.Rewrite{{From: "^Movies/", To: "/server/Movies/"}}},
		{name: "nul", created: []string{"/media/Movies/Example\x00"}},
		{name: "mixed_valid_and_invalid", created: []string{"/media/Movies/Example (2026)"}, deleted: []string{""}},
		{name: "rewrite_to_empty", created: []string{"/incoming/Example"}, rewrite: []autoscan.Rewrite{{From: "^/incoming/Example$", To: ""}}},
		{name: "rewrite_to_relative", deleted: []string{"/incoming/Example"}, rewrite: []autoscan.Rewrite{{From: "^/incoming/", To: "relative/"}}},
		{name: "rewrite_to_nul", created: []string{"/incoming/Example"}, rewrite: []autoscan.Rewrite{{From: "^/incoming/", To: "/server/\x00"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			trigger, err := New(Config{Rewrite: test.rewrite})
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			handler := trigger(func(...autoscan.Scan) error { calls++; return nil })
			body, err := json.Marshal(map[string]any{"created": test.created, "deleted": test.deleted})
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/triggers/a-train/drive", strings.NewReader(string(body))))
			if response.Code != http.StatusBadRequest || calls != 0 {
				t.Fatalf("status=%d calls=%d, want 400 and no callback", response.Code, calls)
			}
		})
	}
}

func TestATrainValidRemotePathsPreserveDriveRewriteAndDuplicates(t *testing.T) {
	trigger, err := New(Config{Priority: 7, Rewrite: []autoscan.Rewrite{{From: "^/unmounted/", To: "/ignored/"}}, Drives: []Drive{{ID: "drive", Rewrite: []autoscan.Rewrite{{From: "^/unmounted/", To: "/server/"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	var received []autoscan.Scan
	calls := 0
	router := chi.NewRouter()
	router.Post("/triggers/a-train/{drive}", trigger(func(scans ...autoscan.Scan) error { calls++; received = append(received, scans...); return nil }).ServeHTTP)
	body := `{"created":["/unmounted/Movies/映画 (2026)"],"deleted":["/unmounted/Movies/映画 (2026)","/unmounted/TV/Example Show/Season 01"]}`
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/triggers/a-train/drive", strings.NewReader(body)))
	if response.Code != http.StatusOK || calls != 1 || len(received) != 3 {
		t.Fatalf("status=%d calls=%d scans=%+v", response.Code, calls, received)
	}
	for i, want := range []string{"/server/Movies/映画 (2026)", "/server/Movies/映画 (2026)", "/server/TV/Example Show/Season 01"} {
		if received[i].Folder != want || received[i].Priority != 7 || received[i].Time.IsZero() {
			t.Errorf("scan=%+v, want %q priority 7 and timestamp", received[i], want)
		}
	}
}
