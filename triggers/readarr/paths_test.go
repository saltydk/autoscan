package readarr_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/saltydk/autoscan"
	"github.com/saltydk/autoscan/triggers/readarr"
)

func TestDownloadRejectsBlankBatchEntry(t *testing.T) {
	trigger, err := readarr.New(readarr.Config{})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	handler := trigger(func(scans ...autoscan.Scan) error {
		calls++
		return nil
	})
	body, err := json.Marshal(map[string]any{
		"eventType": "Download",
		"bookFiles": []map[string]string{
			{"path": "/media/Books/Artist/Album/01 - Track.epub"},
			{"path": ""},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body))))
	if response.Code != http.StatusBadRequest {
		t.Errorf("download with a blank batch entry = HTTP %d, want 400", response.Code)
	}
	if calls != 0 {
		t.Errorf("malformed batch reached the processor %d times, want none", calls)
	}
}

func TestDownloadRejectsInvalidBatchPaths(t *testing.T) {
	for _, tc := range []struct {
		name    string
		file    map[string]any
		rewrite []autoscan.Rewrite
	}{
		{name: "omitted path", file: map[string]any{}},
		{name: "null path", file: map[string]any{"path": nil}},
		{name: "whitespace path", file: map[string]any{"path": " \t\n"}},
		{name: "relative path", file: map[string]any{"path": "Books/Artist/Album/track.epub"}},
		{name: "windows path", file: map[string]any{"path": `C:\Books\Artist\Album\track.epub`}},
		{name: "NUL path", file: map[string]any{"path": "/events/Books/Bad/\x00.epub"}},
		{
			name:    "relative source would rewrite to absolute",
			file:    map[string]any{"path": "Books/Artist/Album/track.epub"},
			rewrite: []autoscan.Rewrite{{From: "^Books/", To: "/media/Books/"}},
		},
		{
			name:    "empty rewritten path",
			file:    map[string]any{"path": "/events/Books/Bad/track.epub"},
			rewrite: []autoscan.Rewrite{{From: "^/events/Books/Bad/.*$", To: ""}},
		},
		{
			name:    "whitespace rewritten path",
			file:    map[string]any{"path": "/events/Books/Bad/track.epub"},
			rewrite: []autoscan.Rewrite{{From: "^/events/Books/Bad/.*$", To: " \t"}},
		},
		{
			name:    "relative rewritten path",
			file:    map[string]any{"path": "/events/Books/Bad/track.epub"},
			rewrite: []autoscan.Rewrite{{From: "^/events/Books/Bad/", To: "Books/Bad/"}},
		},
		{
			name:    "NUL rewritten path",
			file:    map[string]any{"path": "/events/Books/Bad/track.epub"},
			rewrite: []autoscan.Rewrite{{From: "^/events/Books/Bad/", To: "/media/Books/\x00/"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := serveEvent(t, readarr.Config{Rewrite: tc.rewrite}, "Download", []map[string]any{
				{"path": "/events/Books/Good/01 - First.epub"},
				{"path": "/events/Books/Good/02 - Second.epub"},
				tc.file,
			})
			if got.status != http.StatusBadRequest {
				t.Errorf("invalid batch = HTTP %d, want 400", got.status)
			}
			if got.callbacks != 0 {
				t.Errorf("invalid batch reached the processor %d times with %v", got.callbacks, got.scans)
			}
		})
	}
}

func TestDownloadPreservesRewritingAndFolderDeduplication(t *testing.T) {
	got := serveEvent(t, readarr.Config{
		Priority: 7,
		Rewrite: []autoscan.Rewrite{
			{From: "^/events/", To: "/media/"},
			{From: "^/events/", To: "/wrong/"},
		},
	}, "dOwNlOaD", []map[string]any{
		{"path": "/events/Books/José/Novél One (2026)/01 - First.epub"},
		{"path": "/events/Books/José/Novél One (2026)/02 - Second.epub"},
		{"path": "/events/Books/José/Novél Two (2026)/01 - Other.pdf"},
	})
	if got.status != http.StatusOK || got.callbacks != 1 {
		t.Fatalf("valid batch = HTTP %d, processor calls %d", got.status, got.callbacks)
	}
	want := []string{"/media/Books/José/Novél One (2026)", "/media/Books/José/Novél Two (2026)"}
	if len(got.scans) != len(want) {
		t.Fatalf("deduplicated scans = %v, want folders %v", got.scans, want)
	}
	for i, scan := range got.scans {
		if scan.Folder != want[i] || scan.Priority != 7 || scan.Time.IsZero() {
			t.Errorf("scan[%d] = %+v, want folder %q and priority 7", i, scan, want[i])
		}
	}
}

func TestEventStatusesWithoutScans(t *testing.T) {
	for _, tc := range []struct {
		event string
		files []map[string]any
		want  int
	}{
		{"Test", []map[string]any{{"path": ""}}, http.StatusOK},
		{"BookFileDelete", []map[string]any{{"path": "/media/Books/Artist/Album/track.epub"}}, http.StatusBadRequest},
		{"Download", nil, http.StatusBadRequest},
		{"", []map[string]any{{"path": "/media/Books/Artist/Album/track.epub"}}, http.StatusBadRequest},
	} {
		name := tc.event
		if name == "" {
			name = "missing event type"
		}
		t.Run(name, func(t *testing.T) {
			got := serveEvent(t, readarr.Config{}, tc.event, tc.files)
			if got.status != tc.want || got.callbacks != 0 {
				t.Errorf("event %q = HTTP %d, processor calls %d; want HTTP %d and none", tc.event, got.status, got.callbacks, tc.want)
			}
		})
	}
}

type webhookResult struct {
	status, callbacks int
	scans             []autoscan.Scan
}

func serveEvent(t *testing.T, config readarr.Config, event string, files []map[string]any) webhookResult {
	t.Helper()
	trigger, err := readarr.New(config)
	if err != nil {
		t.Fatal(err)
	}
	var result webhookResult
	handler := trigger(func(scans ...autoscan.Scan) error {
		result.callbacks++
		result.scans = append(result.scans, scans...)
		return nil
	})
	body, err := json.Marshal(map[string]any{"eventType": event, "bookFiles": files})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body))))
	result.status = response.Code
	return result
}
