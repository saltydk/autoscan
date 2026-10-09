package sonarr

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/saltydk/autoscan"
)

func TestSonarrNoWorkDoesNotCallProcessor(t *testing.T) {
	for _, payload := range []string{
		`{"eventType":"Test"}`,
		`{"eventType":"Grab","series":{"path":"/events/TV/Unused Show (2026)"}}`,
		`{}`,
		`{"eventType":"Rename","series":{"path":"/events/TV/Empty Rename Show (2026)"},"renamedEpisodeFiles":[]}`,
	} {
		t.Run(payload, func(t *testing.T) {
			checkSonarrEmptyEvent(t, Config{}, payload, http.StatusOK, nil)
		})
	}
}

func TestSonarrRejectsInvalidPathsBeforeNormalization(t *testing.T) {
	for _, series := range []string{"", " ", ".", "relative/TV/Show", "/events/TV/Bad\x00Show"} {
		t.Run("series="+series, func(t *testing.T) {
			checkSonarrEmptyEvent(t, Config{}, sonarrEmptyDownload(series, "Season 01/episode.mkv"), http.StatusBadRequest, nil)
		})
	}
	for _, relative := range []string{"", " ", ".", "..", "Season 01/..", "Season 01/", "../episode.mkv", "/elsewhere/episode.mkv", "Season 01/episode\x00.mkv"} {
		t.Run("file="+relative, func(t *testing.T) {
			checkSonarrEmptyEvent(t, Config{}, sonarrEmptyDownload("/events/TV/Valid Show (2026)", relative), http.StatusBadRequest, nil)
		})
	}
}

func TestSonarrInvalidRenameEntryDoesNotSubmitPartialBatch(t *testing.T) {
	for _, invalid := range []map[string]string{
		{"previousPath": "", "relativePath": "Season 02/episode.mkv"},
		{"previousPath": ".", "relativePath": "Season 02/episode.mkv"},
		{"previousPath": "relative/old.mkv", "relativePath": "Season 02/episode.mkv"},
		{"previousPath": "/events/TV/Old Show (2026)/Season 01/old.mkv", "relativePath": ""},
		{"previousPath": "/events/TV/Old Show (2026)/Season 01/old.mkv", "relativePath": "."},
	} {
		payload := map[string]any{
			"eventType": "Rename",
			"series":    map[string]string{"path": "/events/TV/New Show (2026)"},
			"renamedEpisodeFiles": []map[string]string{
				{"previousPath": "/events/TV/Old Show (2026)/Season 01/good.mkv", "relativePath": "Season 02/good.mkv"},
				invalid,
			},
		}
		checkSonarrEmptyEvent(t, Config{}, sonarrEmptyJSON(t, payload), http.StatusBadRequest, nil)
	}
}

func TestSonarrRejectsInvalidRewrittenFolder(t *testing.T) {
	for _, destination := range []string{"", ".", "relative/folder", "/media/Bad\x00Folder"} {
		c := Config{Rewrite: []autoscan.Rewrite{{From: `^.*$`, To: destination}}}
		checkSonarrEmptyEvent(t, c, sonarrEmptyDownload("/events/TV/Valid Show (2026)", "Season 01/episode.mkv"), http.StatusBadRequest, nil)
	}
	c := Config{Rewrite: []autoscan.Rewrite{
		{From: `^/events/TV/Old Show \(2026\)/`, To: "/media/TV/Old Show (2026)/"},
		{From: `^/events/TV/New Show \(2026\)/.*$`, To: ""},
	}}
	payload := `{"eventType":"Rename","series":{"path":"/events/TV/New Show (2026)"},"renamedEpisodeFiles":[{"previousPath":"/events/TV/Old Show (2026)/Season 01/episode.mkv","relativePath":"Season 02/episode.mkv"}]}`
	checkSonarrEmptyEvent(t, c, payload, http.StatusBadRequest, nil)
}

func TestSonarrValidUnmountedFoldersPreserveDedupeAndPriority(t *testing.T) {
	c := Config{Priority: 7, Rewrite: []autoscan.Rewrite{{From: `^/events/`, To: "/media/"}}}
	payload := `{"eventType":"Rename","series":{"path":"/events/TV/New.Show.2026"},"renamedEpisodeFiles":[{"previousPath":"/events/TV/Old.Show.2026/Season 01/episode-01.mkv","relativePath":"Season 02/episode-01.mkv"},{"previousPath":"/events/TV/Old.Show.2026/Season 01/episode-02.mkv","relativePath":"Season 02/episode-02.mkv"}]}`
	checkSonarrEmptyEvent(t, c, payload, http.StatusOK, []string{"/media/TV/Old.Show.2026/Season 01", "/media/TV/New.Show.2026/Season 02"})
}

func sonarrEmptyDownload(series, relative string) string {
	data, _ := json.Marshal(map[string]any{"eventType": "Download", "series": map[string]string{"path": series}, "episodeFile": map[string]string{"relativePath": relative}})
	return string(data)
}

func sonarrEmptyJSON(t *testing.T, payload any) string {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func checkSonarrEmptyEvent(t *testing.T, c Config, payload string, status int, folders []string) {
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
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/triggers/sonarr", strings.NewReader(payload)))
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
