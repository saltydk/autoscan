package main

import (
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestListenAddressPreservesConfiguredPorts(t *testing.T) {
	for _, tc := range []struct{ host, want string }{
		{"", ":3030"}, {"localhost", "localhost:3030"},
		{"127.0.0.1", "127.0.0.1:3030"}, {"127.0.0.1:4040", "127.0.0.1:4040"},
		{"::1", "[::1]:3030"}, {"[::1]", "[::1]:3030"}, {"[::1]:4040", "[::1]:4040"},
	} {
		if got := listenAddress(tc.host, 3030); got != tc.want {
			t.Errorf("listenAddress(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
}

func TestHTTPBindFailureClosesEarlierListeners(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	first, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	firstAddress := first.Addr().String()
	first.Close()
	servers, err := startHTTPServers(t.Context(), config{Host: []string{firstAddress, occupied.Addr().String()}}, http.NotFoundHandler(), time.Second)
	if err == nil || len(servers) != 0 {
		t.Fatalf("occupied listener accepted: servers = %d, error = %v", len(servers), err)
	}
	rebound, err := net.Listen("tcp", firstAddress)
	if err != nil {
		t.Fatalf("earlier listener leaked after startup failure: %v", err)
	}
	rebound.Close()
}

func testHTTPServer(t *testing.T, handler http.Handler, timeout time.Duration) *http.Server {
	t.Helper()
	servers, err := startHTTPServers(t.Context(), config{Host: []string{"127.0.0.1"}, Port: 0}, handler, timeout)
	if err != nil {
		t.Fatal(err)
	}
	for _, server := range servers {
		t.Cleanup(func() { server.Close() })
	}
	return servers[0]
}

func TestIncomingHTTPTimeoutsAndDisableOverride(t *testing.T) {
	for _, timeout := range []time.Duration{0, 30 * time.Second} {
		server := testHTTPServer(t, http.NotFoundHandler(), timeout)
		if server.ReadTimeout != timeout || server.WriteTimeout != timeout || server.ReadHeaderTimeout != min(timeout, 5*time.Second) {
			t.Fatalf("timeout settings = %+v", server)
		}
	}
	if _, err := startHTTPServers(t.Context(), config{Host: []string{"127.0.0.1"}}, http.NotFoundHandler(), -time.Second); err == nil {
		t.Fatal("negative HTTP timeout accepted")
	}
	readFailure := make(chan error, 1)
	server := testHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(io.Discard, r.Body)
		readFailure <- err
	}), 200*time.Millisecond)
	conn, err := net.Dial("tcp", server.Addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "POST / HTTP/1.1\r\nHost: localhost\r\nContent-Length: 10\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-readFailure:
		if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
			t.Fatalf("stalled request body error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stalled request body was not timed out")
	}
}

func TestSlowHTTPHeadersAreTimedOut(t *testing.T) {
	server := testHTTPServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("incomplete request reached handler")
	}), 200*time.Millisecond)
	conn, err := net.Dial("tcp", server.Addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(conn); err != nil {
		t.Fatalf("connection did not close on header deadline: %v", err)
	}
}
