package autoscan

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/saltydk/autoscan"
)

func TestAPIClientRequestTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release); server.Close() })
	client := newAPIClient(server.URL, "", "", zerolog.Nop())
	if client.client.Timeout != autoscan.TargetHTTPTimeout {
		t.Fatalf("client Timeout = %v", client.client.Timeout)
	}
	client.client.Timeout = 30 * time.Millisecond
	if err := client.Scan("/media/series/Show/Season 01"); !errors.Is(err, autoscan.ErrTargetUnavailable) || errors.Is(err, autoscan.ErrFatal) {
		t.Fatalf("Scan() timeout = %v, want only ErrTargetUnavailable", err)
	}
}
