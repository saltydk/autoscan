package plex

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/saltydk/autoscan"
)

func TestAPIClientRequestTimeout(t *testing.T) {
	for _, operation := range []string{"version", "libraries"} {
		for _, body := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s_stalled_body_%t", operation, body), func(t *testing.T) {
				release := make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if body {
						_, _ = io.WriteString(w, `{"MediaContainer":{`)
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
				var err error
				if operation == "version" {
					_, err = client.Version()
				} else {
					_, err = client.Libraries()
				}
				if !errors.Is(err, autoscan.ErrTargetUnavailable) || errors.Is(err, autoscan.ErrFatal) {
					t.Fatalf("timeout = %v, want only ErrTargetUnavailable", err)
				}
			})
		}
	}
}

func TestAPIClientResponseLimitClosesBody(t *testing.T) {
	for _, limit := range []int64{8, 128} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			body := &plexTrackedBody{Reader: strings.NewReader(`{"MediaContainer":{"version":"1.40.0"}}`)}
			client := newAPIClient("http://plex", "token", zerolog.Nop())
			client.responseLimit = limit
			client.client.Transport = plexRoundTrip(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: body, ContentLength: -1, Request: req}, nil
			})
			version, err := client.Version()
			if limit == 8 {
				if !errors.Is(err, autoscan.ErrTargetUnavailable) || errors.Is(err, autoscan.ErrFatal) {
					t.Fatalf("Version() oversized response = %v", err)
				}
			} else if err != nil || version != "1.40.0" {
				t.Fatalf("Version() = %q, %v", version, err)
			}
			if !body.closed {
				t.Error("response body was not closed")
			}
		})
	}
}

type plexTrackedBody struct {
	io.Reader
	closed bool
}

func (body *plexTrackedBody) Close() error { body.closed = true; return nil }

type plexRoundTrip func(*http.Request) (*http.Response, error)

func (fn plexRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) { return fn(request) }
