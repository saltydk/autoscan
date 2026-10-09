package autoscan

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"testing"
	"time"
)

func TestHTTPStatusError(t *testing.T) {
	for _, tt := range []struct {
		status int
		want   error
	}{
		{http.StatusOK, nil},
		{http.StatusNoContent, nil},
		{http.StatusBadRequest, ErrFatal},
		{http.StatusUnauthorized, ErrFatal},
		{http.StatusForbidden, ErrFatal},
		{http.StatusNotFound, ErrTargetUnavailable},
		{http.StatusRequestTimeout, ErrTargetUnavailable},
		{http.StatusTooManyRequests, ErrTargetUnavailable},
		{http.StatusInternalServerError, ErrTargetUnavailable},
		{http.StatusBadGateway, ErrTargetUnavailable},
		{http.StatusServiceUnavailable, ErrTargetUnavailable},
		{http.StatusGatewayTimeout, ErrTargetUnavailable},
	} {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			response := &http.Response{
				StatusCode: tt.status,
				Status:     fmt.Sprintf("%d %s", tt.status, http.StatusText(tt.status)),
				Header:     http.Header{"Retry-After": []string{"120"}},
			}
			err := HTTPStatusError(response)
			if !errors.Is(err, tt.want) {
				t.Fatalf("HTTPStatusError() = %v, want %v", err, tt.want)
			}
			wantDelay := time.Duration(0)
			if tt.want == ErrTargetUnavailable {
				wantDelay = 120 * time.Second
			}
			if got := RetryDelay(err); got != wantDelay {
				t.Errorf("RetryDelay() = %v, want %v", got, wantDelay)
			}
		})
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		value string
		want  time.Duration
	}{
		{"", 0},
		{"invalid", 0},
		{"-1", 0},
		{"0", 0},
		{" 15 ", 15 * time.Second},
		{now.Add(2 * time.Minute).Format(http.TimeFormat), 2 * time.Minute},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0},
		{"9223372036854775807", time.Duration(math.MaxInt64)},
	} {
		t.Run(tt.value, func(t *testing.T) {
			if got := parseRetryAfter(tt.value, now); got != tt.want {
				t.Errorf("parseRetryAfter() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRetryDelayJoinedErrors(t *testing.T) {
	err := errors.Join(
		fmt.Errorf("first target: %w", &retryAfterError{err: ErrTargetUnavailable, delay: time.Minute}),
		fmt.Errorf("second target: %w", &retryAfterError{err: ErrTargetUnavailable, delay: 2 * time.Minute}),
		ErrFatal,
	)
	if got := RetryDelay(err); got != 2*time.Minute {
		t.Fatalf("RetryDelay() = %v, want 2m", got)
	}
	if got := RetryDelay(nil); got != 0 {
		t.Errorf("RetryDelay(nil) = %v, want 0", got)
	}
}

func TestHTTPResponseError(t *testing.T) {
	if err := HTTPResponseError(context.DeadlineExceeded); !errors.Is(err, ErrTargetUnavailable) || errors.Is(err, ErrFatal) {
		t.Errorf("deadline error = %v, want only ErrTargetUnavailable", err)
	}
	if err := HTTPResponseError(errors.New("invalid JSON")); !errors.Is(err, ErrFatal) {
		t.Errorf("invalid JSON error = %v, want ErrFatal", err)
	}
}
