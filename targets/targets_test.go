package targets_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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
