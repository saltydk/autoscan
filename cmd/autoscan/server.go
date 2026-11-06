package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
)

var startupReady atomic.Bool

func listenAddress(host string, port int) string {
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	if strings.Contains(host, ":") {
		ip := strings.Trim(host, "[]")
		if net.ParseIP(ip) != nil {
			return net.JoinHostPort(ip, strconv.Itoa(port))
		}
		return host
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func startHTTPServers(ctx context.Context, c config, handler http.Handler, timeout time.Duration) ([]*http.Server, error) {
	if timeout < 0 {
		return nil, errors.New("HTTP timeout must not be negative")
	}
	listeners := make([]net.Listener, 0, len(c.Host))
	for _, host := range c.Host {
		var lc net.ListenConfig
		listener, err := lc.Listen(ctx, "tcp", listenAddress(host, c.Port))
		if err != nil {
			for _, bound := range listeners {
				_ = bound.Close()
			}
			return nil, fmt.Errorf("bind HTTP listener %q: %w", host, err)
		}
		listeners = append(listeners, listener)
	}
	servers := make([]*http.Server, 0, len(listeners))
	for _, listener := range listeners {
		// Read deadlines bound slow or stalled clients. There is deliberately no
		// WriteTimeout: it also counts handler time, and webhook handlers wait for
		// the single SQLite connection, which a Bernard full sync can hold for
		// minutes. Cutting that response loses nothing but makes the sender record
		// a failed delivery. Responses are a few bytes, so slow readers cannot
		// hold a handler open.
		server := &http.Server{
			Addr: listener.Addr().String(), Handler: handler,
			ReadHeaderTimeout: min(timeout, 5*time.Second),
			ReadTimeout:       timeout, IdleTimeout: 2 * time.Minute,
		}
		servers = append(servers, server)
		log.Info().Str("addr", server.Addr).Msg("HTTP listener bound")
		go func() {
			if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatal().Err(err).Str("addr", server.Addr).Msg("HTTP server stopped")
			}
		}()
	}
	return servers, nil
}

// notifyService is optional systemd integration. An unset socket is a no-op.
func notifyService(state string) error {
	path := os.Getenv("NOTIFY_SOCKET")
	if path == "" {
		return nil
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Net: "unixgram", Name: path})
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	_, err = conn.Write([]byte(state))
	return err
}
