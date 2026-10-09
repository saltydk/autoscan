package radarr

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/saltydk/autoscan"
)

func TestRadarrUnsupportedEventsDoNotCallProcessor(t *testing.T) {
	for _, payload := range []string{`{"eventType":"Test"}`, `{}`, `{"eventType":"MovieAdded","movie":{"folderPath":"/events/Movies/Unused Movie (2026)"}}`} {
		checkRadarrEmptyEvent(t, Config{}, payload, http.StatusOK, nil)
	}
}

func TestRadarrRejectsInvalidPathsBeforeNormalization(t *testing.T) {
	for _, folder := range []string{"", " ", ".", "relative/Movies/Movie", "/events/Movies/Bad\x00Movie"} {
		checkRadarrEmptyEvent(t, Config{}, radarrEmptyPayload("Download", folder, "video.mkv"), http.StatusBadRequest, nil)
		checkRadarrEmptyEvent(t, Config{}, radarrEmptyPayload("MovieDelete", folder, ""), http.StatusBadRequest, nil)
	}
	for _, relative := range []string{"", " ", ".", "..", "Extras/..", "Extras/", "../video.mkv", "/elsewhere/video.mkv", "video\x00.mkv"} {
		checkRadarrEmptyEvent(t, Config{}, radarrEmptyPayload("Download", "/events/Movies/Valid Movie (2026)", relative), http.StatusBadRequest, nil)
	}
}

func TestRadarrRejectsInvalidRewrittenFolder(t *testing.T) {
	for _, destination := range []string{"", ".", "relative/folder", "/media/Bad\x00Folder"} {
		c := Config{Rewrite: []autoscan.Rewrite{{From: `^.*$`, To: destination}}}
		checkRadarrEmptyEvent(t, c, radarrEmptyPayload("Download", "/events/Movies/Valid Movie (2026)", "video.mkv"), http.StatusBadRequest, nil)
	}
}

func TestRadarrValidUnmountedFoldersPreservePriority(t *testing.T) {
	c := Config{Priority: 8, Rewrite: []autoscan.Rewrite{{From: `^/events/`, To: "/media/"}}}
	for _, eventType := range []string{"Download", "MovieFileDelete", "MovieDelete", "Rename"} {
		checkRadarrEmptyEvent(t, c, radarrEmptyPayload(eventType, "/events/Movies/Valid.Movie.2026", "video.mkv"), http.StatusOK, []string{"/media/Movies/Valid.Movie.2026"})
	}
}

func radarrEmptyPayload(eventType, folder, relative string) string {
	data, _ := json.Marshal(map[string]any{"eventType": eventType, "movie": map[string]string{"folderPath": folder}, "movieFile": map[string]string{"relativePath": relative}})
	return string(data)
}

func checkRadarrEmptyEvent(t *testing.T, c Config, payload string, status int, folders []string) {
	t.Helper()
	trigger, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	var received []string
	h := trigger(func(scans ...autoscan.Scan) error {
		calls++
		for _, scan := range scans {
			received = append(received, scan.Folder)
			if scan.Priority != c.Priority || scan.Time.IsZero() {
				t.Errorf("scan metadata changed: %+v", scan)
			}
		}
		return nil
	})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/triggers/radarr", strings.NewReader(payload)))
	if w.Code != status {
		t.Errorf("status=%d, want %d", w.Code, status)
	}
	if !slices.Equal(received, folders) {
		t.Errorf("folders=%q, want %q", received, folders)
	}
	wantCalls := 0
	if len(folders) > 0 {
		wantCalls = 1
	}
	if calls != wantCalls {
		t.Errorf("processor calls=%d, want %d", calls, wantCalls)
	}
}
