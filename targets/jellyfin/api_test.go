package jellyfin

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"

	"github.com/saltydk/autoscan"
)

const testToken = "jellyfin-api-token"

func TestAPIClientLibraries(t *testing.T) {
	client, closeServer := newTestAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertJellyfinRequest(t, r, http.MethodGet, "/proxy/jellyfin/Library/VirtualFolders")
		_, _ = io.WriteString(w, `[{"Name":"Movies","Locations":["/media/movies","/media/extras/"]},{"Name":"Empty","Locations":[]}]`)
	})
	defer closeServer()

	got, err := client.Libraries()
	if err != nil {
		t.Fatalf("Libraries() error = %v", err)
	}

	want := []library{
		{Name: "Movies", Path: "/media/movies/"},
		{Name: "Movies", Path: "/media/extras/"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Libraries() = %#v, want %#v", got, want)
	}
}

func TestAPIClientAvailable(t *testing.T) {
	client, closeServer := newTestAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertJellyfinRequest(t, r, http.MethodGet, "/proxy/jellyfin/System/Info")
		w.WriteHeader(http.StatusNoContent)
	})
	defer closeServer()

	if err := client.Available(); err != nil {
		t.Fatalf("Available() error = %v", err)
	}
}

func TestAPIClientScan(t *testing.T) {
	client, closeServer := newTestAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertJellyfinRequest(t, r, http.MethodPost, "/proxy/jellyfin/Library/Media/Updated")
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want %q", got, "application/json")
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
			return
		}
		want := `{"Updates":[{"path":"/media/movie.mkv","updateType":"Modified"}]}`
		if got := string(body); got != want {
			t.Errorf("request body = %q, want %q", got, want)
		}
	})
	defer closeServer()

	if err := client.Scan("/media/movie.mkv"); err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
}

func TestAPIClientRejectedRequestClassification(t *testing.T) {
	tests := []struct {
		status int
		want   error
	}{
		{status: http.StatusUnauthorized, want: autoscan.ErrFatal},
		{status: http.StatusForbidden, want: autoscan.ErrFatal},
		{status: http.StatusNotFound, want: autoscan.ErrTargetUnavailable},
		{status: http.StatusInternalServerError, want: autoscan.ErrTargetUnavailable},
		{status: http.StatusBadGateway, want: autoscan.ErrTargetUnavailable},
		{status: http.StatusServiceUnavailable, want: autoscan.ErrTargetUnavailable},
		{status: http.StatusGatewayTimeout, want: autoscan.ErrTargetUnavailable},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("status_%d", tt.status), func(t *testing.T) {
			var requests atomic.Int32
			client, closeServer := newTestAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.WriteHeader(tt.status)
			})
			defer closeServer()

			err := client.Available()
			if !errors.Is(err, tt.want) {
				t.Errorf("Available() error = %v, want error wrapping %v", err, tt.want)
			}

			other := autoscan.ErrFatal
			if tt.want == autoscan.ErrFatal {
				other = autoscan.ErrTargetUnavailable
			}
			if errors.Is(err, other) {
				t.Errorf("Available() error = %v, must not wrap %v", err, other)
			}
			if got := requests.Load(); got != 1 {
				t.Errorf("request count = %d, want 1", got)
			}
		})
	}
}

func newTestAPIClient(t *testing.T, handler http.HandlerFunc) (apiClient, func()) {
	t.Helper()

	server := httptest.NewServer(handler)
	client := newAPIClient(server.URL+"/proxy/jellyfin/", testToken, zerolog.Nop())
	return client, server.Close
}

func assertJellyfinRequest(t *testing.T, r *http.Request, method, path string) {
	t.Helper()

	if r.Method != method {
		t.Errorf("method = %q, want %q", r.Method, method)
	}
	if r.URL.Path != path {
		t.Errorf("path = %q, want %q", r.URL.Path, path)
	}
	if r.URL.RawQuery != "" {
		t.Errorf("query = %q, want no query parameters", r.URL.RawQuery)
	}
	if got := r.Header.Get("Authorization"); got != `MediaBrowser Token="jellyfin-api-token"` {
		t.Errorf("Authorization = %q, want %q", got, `MediaBrowser Token="jellyfin-api-token"`)
	}
	if got := r.Header.Get("X-Emby-Token"); got != "" {
		t.Errorf("X-Emby-Token = %q, want header absent", got)
	}
}
