package autoscan

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
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

func TestResolveTargetResponseLimit(t *testing.T) {
	for _, tt := range []struct {
		name    string
		limit   *int64
		want    int64
		wantErr bool
	}{
		{name: "omitted", want: DefaultTargetResponseLimit},
		{name: "disabled", limit: responseLimitPointer(0)},
		{name: "larger", limit: responseLimitPointer(2 * DefaultTargetResponseLimit), want: 2 * DefaultTargetResponseLimit},
		{name: "negative", limit: responseLimitPointer(-1), wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveTargetResponseLimit(tt.limit)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("ResolveTargetResponseLimit() = %d, %v; want %d, error %t", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestDecodeResponseBodyLimit(t *testing.T) {
	const valid = `{"Version":"1.40"}`
	for _, tt := range []struct {
		name        string
		body        string
		limit       int64
		knownLength bool
		wantErr     error
	}{
		{name: "under_limit", body: valid, limit: 32},
		{name: "exact_limit", body: valid, limit: int64(len(valid))},
		{name: "known_oversized_body", body: valid, limit: 8, knownLength: true, wantErr: ErrTargetUnavailable},
		{name: "unknown_oversized_body", body: valid, limit: 8, wantErr: ErrTargetUnavailable},
		{name: "valid_json_with_overflow_tail", body: valid + strings.Repeat(" ", 64), limit: 32, wantErr: ErrTargetUnavailable},
		{name: "valid_trailing_whitespace", body: valid + " \n\t", limit: 32},
		{name: "disabled_limit", body: valid + strings.Repeat(" ", 64)},
		{name: "truncated_json_below_limit", body: `{"Version":`, limit: 32, wantErr: ErrFatal},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := &responseCountingBody{reader: strings.NewReader(tt.body)}
			response := &http.Response{Body: body, ContentLength: -1}
			if tt.knownLength {
				response.ContentLength = int64(len(tt.body))
			}
			defer body.Close()
			var decoded struct{ Version string }
			err := DecodeResponseBody(response, tt.limit, &decoded)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("DecodeResponseBody() = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && decoded.Version != "1.40" {
				t.Errorf("decoded Version = %q", decoded.Version)
			}
			if tt.limit > 0 && body.read > tt.limit+1 {
				t.Errorf("read %d bytes with limit %d", body.read, tt.limit)
			}
			if tt.knownLength && body.read != 0 {
				t.Errorf("known oversized body read %d bytes, want 0", body.read)
			}
			if tt.wantErr == ErrTargetUnavailable && errors.Is(err, ErrFatal) {
				t.Errorf("overflow also classified as fatal: %v", err)
			}
		})
	}
}

func TestDecodeResponseBodyDeadline(t *testing.T) {
	response := &http.Response{Body: io.NopCloser(responseErrorReader{})}
	var value any
	if err := DecodeResponseBody(response, 32, &value); !errors.Is(err, ErrTargetUnavailable) || errors.Is(err, ErrFatal) {
		t.Fatalf("deadline classification = %v", err)
	}
}

func responseLimitPointer(value int64) *int64 { return &value }

type responseCountingBody struct {
	reader io.Reader
	read   int64
}

func (body *responseCountingBody) Read(p []byte) (int, error) {
	n, err := body.reader.Read(p)
	body.read += int64(n)
	return n, err
}
func (*responseCountingBody) Close() error { return nil }

type responseErrorReader struct{}

func (responseErrorReader) Read([]byte) (int, error) { return 0, context.DeadlineExceeded }
