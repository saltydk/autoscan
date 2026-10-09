package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

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
		server := &http.Server{
			Addr: listener.Addr().String(), Handler: handler,
			ReadHeaderTimeout: min(timeout, 5*time.Second),
			ReadTimeout:       timeout, WriteTimeout: timeout, IdleTimeout: 2 * time.Minute,
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
