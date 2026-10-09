package autoscan

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// TargetHTTPTimeout bounds the entire outbound request, including its body.
const TargetHTTPTimeout = 30 * time.Second

type retryAfterError struct {
	err   error
	delay time.Duration
}

func (e *retryAfterError) Error() string { return e.err.Error() }
func (e *retryAfterError) Unwrap() error { return e.err }

// HTTPStatusError classifies rejected target responses and preserves Retry-After.
func HTTPStatusError(response *http.Response) error {
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return nil
	}

	switch response.StatusCode {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusNotFound,
		http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return &retryAfterError{
			err:   fmt.Errorf("%s: %w", response.Status, ErrTargetUnavailable),
			delay: parseRetryAfter(response.Header.Get("Retry-After"), time.Now()),
		}
	default:
		return fmt.Errorf("%s: %w", response.Status, ErrFatal)
	}
}

// HTTPResponseError distinguishes timed-out response bodies from invalid JSON.
func HTTPResponseError(err error) error {
	var networkError net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &networkError) && networkError.Timeout() {
		return fmt.Errorf("%v: %w", err, ErrTargetUnavailable)
	}
	return fmt.Errorf("%v: %w", err, ErrFatal)
}

// RetryDelay returns the longest server-requested delay in wrapped or joined errors.
func RetryDelay(err error) time.Duration {
	var delay time.Duration
	if retry, ok := err.(*retryAfterError); ok {
		delay = retry.delay
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		for _, child := range wrapped.Unwrap() {
			delay = max(delay, RetryDelay(child))
		}
	case interface{ Unwrap() error }:
		delay = max(delay, RetryDelay(wrapped.Unwrap()))
	}
	return delay
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds < 0 {
			return 0
		}
		if seconds > math.MaxInt64/int64(time.Second) {
			return time.Duration(math.MaxInt64)
		}
		return time.Duration(seconds) * time.Second
	}
	if deadline, err := http.ParseTime(value); err == nil {
		return max(0, deadline.Sub(now))
	}
	return 0
}
