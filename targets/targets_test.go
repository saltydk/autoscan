package targets_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/saltydk/autoscan"
	autoscantarget "github.com/saltydk/autoscan/targets/autoscan"
	"github.com/saltydk/autoscan/targets/emby"
	"github.com/saltydk/autoscan/targets/jellyfin"
	"github.com/saltydk/autoscan/targets/plex"
)

type mediaTarget struct {
	name      string
	new       func(string) (autoscan.Target, error)
	libraries string
	scanPath  string
	payload   string
}

var mediaTargets = []mediaTarget{
	{
		name:      "plex",
		new:       func(url string) (autoscan.Target, error) { return plex.New(plex.Config{URL: url}) },
		libraries: "/library/sections",
		scanPath:  "/library/sections/1/refresh",
		payload:   `{"MediaContainer":{"Directory":[{"key":"1","title":"Series","Location":[{"path":"/media/series"}]},{"key":"2","title":"Nested","Location":[{"path":"/media/series/Special"}]}]}}`,
	},
	{
		name:      "emby",
		new:       func(url string) (autoscan.Target, error) { return emby.New(emby.Config{URL: url}) },
		libraries: "/emby/Library/SelectableMediaFolders",
		scanPath:  "/Library/Media/Updated",
		payload:   `[{"Name":"Series","SubFolders":[{"Path":"/media/series"}]},{"Name":"Nested","SubFolders":[{"Path":"/media/series/Special"}]}]`,
	},
	{
		name:      "jellyfin",
		new:       func(url string) (autoscan.Target, error) { return jellyfin.New(jellyfin.Config{URL: url}) },
		libraries: "/Library/VirtualFolders",
		scanPath:  "/Library/Media/Updated",
		payload:   `[{"Name":"Series","Locations":["/media/series"]},{"Name":"Nested","Locations":["/media/series/Special"]}]`,
	},
}

func TestMediaTargetsRetryLibraryDiscovery(t *testing.T) {
	for _, targetType := range mediaTargets {
		t.Run(targetType.name, func(t *testing.T) {
			var libraryRequests atomic.Int32
			var scans atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/":
					_, _ = io.WriteString(w, `{"MediaContainer":{"version":"1.40.0"}}`)
				case targetType.libraries:
					if libraryRequests.Add(1) == 1 {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					_, _ = io.WriteString(w, targetType.payload)
				default:
					scans.Add(1)
					w.WriteHeader(http.StatusNoContent)
				}
			}))
			defer server.Close()
			target, err := targetType.new(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			scan := autoscan.Scan{Folder: "/media/series/Show/Season 01"}
			if err := target.Scan(scan); !errors.Is(err, autoscan.ErrTargetUnavailable) {
				t.Fatalf("Scan() during discovery failure = %v", err)
			}
			if got := scans.Load(); got != 0 {
				t.Errorf("failed discovery sent %d scans", got)
			}
			if err := target.Scan(scan); err != nil {
				t.Fatalf("Scan() after recovery = %v", err)
			}
			if got := scans.Load(); got != 1 {
				t.Errorf("recovered discovery sent %d scans, want 1", got)
			}
		})
	}
}

func TestPlexRejectsUnsupportedVersionBeforeScanning(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			_, _ = io.WriteString(w, `{"MediaContainer":{"version":"1.19.0"}}`)
			return
		}
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	target, err := plex.New(plex.Config{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := target.Scan(autoscan.Scan{Folder: "/media/series/Show"}); !errors.Is(err, autoscan.ErrFatal) {
		t.Fatalf("Scan() with unsupported Plex = %v", err)
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("unsupported Plex received %d library/scan requests", got)
	}
}

func TestTargetsStartOfflineAndRetryAvailability(t *testing.T) {
	constructors := append([]mediaTarget(nil), mediaTargets...)
	constructors = append(constructors, mediaTarget{
		name: "autoscan",
		new:  func(url string) (autoscan.Target, error) { return autoscantarget.New(autoscantarget.Config{URL: url}) },
	})
	for _, targetType := range constructors {
		t.Run(targetType.name, func(t *testing.T) {
			var requests atomic.Int32
			var status atomic.Int32
			status.Store(http.StatusTooManyRequests)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if code := status.Load(); code != http.StatusOK {
					w.Header().Set("Retry-After", "90")
					w.WriteHeader(int(code))
					return
				}
				switch r.URL.Path {
				case targetType.libraries:
					_, _ = io.WriteString(w, targetType.payload)
				case "/":
					_, _ = io.WriteString(w, `{"MediaContainer":{"version":"1.40.0"}}`)
				default:
					w.WriteHeader(http.StatusNoContent)
				}
			}))
			defer server.Close()
			target, err := targetType.new(server.URL)
			if err != nil {
				t.Fatalf("New() while offline: %v", err)
			}
			if got := requests.Load(); got != 0 {
				t.Fatalf("New() made %d network calls", got)
			}
			for _, code := range []int{http.StatusTooManyRequests, http.StatusRequestTimeout} {
				status.Store(int32(code))
				err = target.Available()
				if !errors.Is(err, autoscan.ErrTargetUnavailable) || errors.Is(err, autoscan.ErrFatal) {
					t.Fatalf("Available() with status %d = %v", code, err)
				}
				if got := autoscan.RetryDelay(err); got != 90*time.Second {
					t.Errorf("RetryDelay() = %v, want 90s", got)
				}
			}
			status.Store(http.StatusOK)
			if err := target.Available(); err != nil {
				t.Fatalf("Available() after recovery: %v", err)
			}
		})
	}
}

func TestMediaTargetsRejectLibraryRoots(t *testing.T) {
	for _, targetType := range mediaTargets {
		t.Run(targetType.name, func(t *testing.T) {
			var requests atomic.Int32
			var libraryRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/" {
					_, _ = io.WriteString(w, `{"MediaContainer":{"version":"1.40.0"}}`)
					return
				}
				if r.URL.Path == targetType.libraries {
					libraryRequests.Add(1)
					_, _ = io.WriteString(w, targetType.payload)
					return
				}
				requests.Add(1)
				if r.URL.Path != targetType.scanPath {
					t.Errorf("scan URL = %q, want %q", r.URL.Path, targetType.scanPath)
				}
				const wantFolder = "/media/series/Show/Season 01"
				if targetType.name == "plex" {
					if got := r.URL.Query().Get("path"); got != wantFolder {
						t.Errorf("scan path = %q, want %q", got, wantFolder)
					}
				} else {
					var payload struct {
						Updates []struct{ Path string }
					}
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Errorf("decode scan: %v", err)
					} else if len(payload.Updates) != 1 || payload.Updates[0].Path != wantFolder {
						t.Errorf("scan payload = %+v", payload)
					}
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			target, err := targetType.new(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			if got := libraryRequests.Load(); got != 0 {
				t.Fatalf("constructor made %d network calls", got)
			}
			for _, folder := range []string{
				"/media/series", "/media/series/", "/media//series/.",
				"/media/series/Show/..", "/media/series/Special/",
			} {
				err := target.Scan(autoscan.Scan{Folder: folder})
				if !errors.Is(err, autoscan.ErrScanRejected) || errors.Is(err, autoscan.ErrFatal) {
					t.Errorf("Scan(%q) = %v, want only ErrScanRejected", folder, err)
				}
			}
			for _, folder := range []string{"/other/Show", "/media/series-old/Show", "/media/series/../outside"} {
				if err := target.Scan(autoscan.Scan{Folder: folder}); err != nil {
					t.Errorf("out-of-library Scan(%q) = %v, want only ErrLibraryNotMatched", folder, err)
				}
			}
			if got := requests.Load(); got != 0 {
				t.Fatalf("rejected or unrelated scans made %d requests", got)
			}
			if err := target.Scan(autoscan.Scan{Folder: "/media/series/Show/Season 01"}); err != nil {
				t.Fatal(err)
			}
			if got := requests.Load(); got != 1 {
				t.Errorf("season folder scan made %d requests, want 1", got)
			}
			if got := libraryRequests.Load(); got != 1 {
				t.Errorf("library discovery requests = %d, want 1", got)
			}
		})
	}
}

func TestMediaTargetsConcurrentLibraryDiscovery(t *testing.T) {
	for _, targetType := range mediaTargets {
		t.Run(targetType.name, func(t *testing.T) {
			var libraryRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/":
					_, _ = io.WriteString(w, `{"MediaContainer":{"version":"1.40.0"}}`)
				case targetType.libraries:
					libraryRequests.Add(1)
					_, _ = io.WriteString(w, targetType.payload)
				default:
					t.Errorf("unexpected scan request to %s", r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			defer server.Close()
			target, err := targetType.new(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			var workers sync.WaitGroup
			start := make(chan struct{})
			for range 8 {
				workers.Go(func() {
					<-start
					if err := target.Scan(autoscan.Scan{Folder: "/media/series"}); !errors.Is(err, autoscan.ErrScanRejected) {
						t.Errorf("Scan() = %v, want ErrScanRejected", err)
					}
				})
			}
			close(start)
			workers.Wait()
			if got := libraryRequests.Load(); got != 1 {
				t.Errorf("concurrent discovery made %d requests, want 1", got)
			}
		})
	}
}
