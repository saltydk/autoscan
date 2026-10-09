package emby

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/saltydk/autoscan"
)

func TestAPIClientRequestTimeout(t *testing.T) {
	for _, body := range []bool{false, true} {
		t.Run(fmt.Sprintf("stalled_body_%t", body), func(t *testing.T) {
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if body {
					_, _ = io.WriteString(w, `[{"Name":"Series","SubFolders":[`)
					w.(http.Flusher).Flush()
				}
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			t.Cleanup(func() { close(release); server.Close() })
			client := newAPIClient(server.URL, "token", zerolog.Nop())
			if client.client.Timeout != autoscan.TargetHTTPTimeout {
				t.Fatalf("client Timeout = %v", client.client.Timeout)
			}
			client.client.Timeout = 30 * time.Millisecond
			_, err := client.Libraries()
			if !errors.Is(err, autoscan.ErrTargetUnavailable) || errors.Is(err, autoscan.ErrFatal) {
				t.Fatalf("Libraries() timeout = %v, want only ErrTargetUnavailable", err)
			}
		})
	}
}
