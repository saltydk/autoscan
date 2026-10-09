package autoscan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// TargetHTTPTimeout bounds the entire outbound request, including its body.
const TargetHTTPTimeout = 30 * time.Second

// DefaultTargetResponseLimit bounds decoded media-server responses to 10 MiB.
const DefaultTargetResponseLimit int64 = 10 * 1024 * 1024

// ResolveTargetResponseLimit keeps the default for omitted values and permits
// an explicit zero to disable the byte limit for legitimate large responses.
func ResolveTargetResponseLimit(limit *int64) (int64, error) {
	if limit == nil {
		return DefaultTargetResponseLimit, nil
	}
	if *limit < 0 {
		return 0, fmt.Errorf("response-limit must be zero or positive, got %d: %w", *limit, ErrFatal)
	}
	return *limit, nil
}

// DecodeResponseBody reads the complete response before decoding so a valid JSON
// prefix cannot hide a body larger than the configured limit. The caller closes
// the body on every outcome. A zero limit explicitly permits an unlimited body.
func DecodeResponseBody(response *http.Response, limit int64, value any) error {
	tooLarge := func() error {
		return fmt.Errorf("response body exceeds response-limit of %d bytes; increase response-limit or set it to 0 to disable: %w", limit, ErrTargetUnavailable)
	}
	if limit > 0 && response.ContentLength > limit {
		return tooLarge()
	}
	reader := io.Reader(response.Body)
	if limit > 0 && limit < math.MaxInt64 {
		reader = io.LimitReader(reader, limit+1)
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		return HTTPResponseError(err)
	}
	if limit > 0 && int64(len(body)) > limit {
		return tooLarge()
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(value); err != nil {
		return HTTPResponseError(err)
	}
	return nil
}

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
