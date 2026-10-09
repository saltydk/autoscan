package manual

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/saltydk/autoscan"
)

func TestManualRejectsInvalidDirectoryBatchBeforeCallback(t *testing.T) {
	for _, test := range []struct {
		name    string
		dirs    []string
		rewrite []autoscan.Rewrite
	}{
		{name: "blank", dirs: []string{""}},
		{name: "whitespace", dirs: []string{"   "}},
		{name: "relative", dirs: []string{"Movies/Example (2026)"}},
		{name: "relative_must_not_be_salvaged_by_rewrite", dirs: []string{"Movies/Example (2026)"}, rewrite: []autoscan.Rewrite{{From: "^Movies/", To: "/server/Movies/"}}},
		{name: "nul", dirs: []string{"/media/Movies/Example\x00"}},
		{name: "mixed_valid_and_invalid", dirs: []string{"/media/Movies/Example (2026)", ""}},
		{name: "rewrite_to_empty", dirs: []string{"/incoming/Example"}, rewrite: []autoscan.Rewrite{{From: "^/incoming/Example$", To: ""}}},
		{name: "rewrite_to_relative", dirs: []string{"/incoming/Example"}, rewrite: []autoscan.Rewrite{{From: "^/incoming/", To: "relative/"}}},
		{name: "rewrite_to_nul", dirs: []string{"/incoming/Example"}, rewrite: []autoscan.Rewrite{{From: "^/incoming/", To: "/server/\x00"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			trigger, err := New(Config{Rewrite: test.rewrite})
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			handler := trigger(func(...autoscan.Scan) error { calls++; return nil })
			request := httptest.NewRequest(http.MethodPost, "/triggers/manual?"+url.Values{"dir": test.dirs}.Encode(), nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || calls != 0 {
				t.Fatalf("status=%d callback calls=%d, want 400 and no callback", response.Code, calls)
			}
		})
	}
}

func TestManualValidMediaDirectoriesAndReadMethods(t *testing.T) {
	trigger, err := New(Config{Priority: 7, Rewrite: []autoscan.Rewrite{{From: "^/unmounted/", To: "/server/"}, {From: "^/unmounted/", To: "/ignored/"}}})
	if err != nil {
		t.Fatal(err)
	}
	var received []autoscan.Scan
	calls := 0
	handler := trigger(func(scans ...autoscan.Scan) error { calls++; received = append(received, scans...); return nil })
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(method, "/triggers/manual?dir=", nil))
		if response.Code != http.StatusOK || calls != 0 {
			t.Fatalf("%s status=%d callback calls=%d", method, response.Code, calls)
		}
	}
	dirs := []string{"/unmounted/Movies/映画 (2026)", "/unmounted/TV/Example Show/Season 01/"}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/triggers/manual?"+url.Values{"dir": dirs}.Encode(), nil))
	if response.Code != http.StatusOK || calls != 1 || len(received) != 2 {
		t.Fatalf("status=%d calls=%d scans=%+v", response.Code, calls, received)
	}
	for i, want := range []string{"/server/Movies/映画 (2026)", "/server/TV/Example Show/Season 01"} {
		if received[i].Folder != want || received[i].Priority != 7 || received[i].Time.IsZero() {
			t.Errorf("scan=%+v, want %q priority 7 and timestamp", received[i], want)
		}
	}
}
