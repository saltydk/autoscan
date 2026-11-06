package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/saltydk/autoscan"
	"github.com/saltydk/autoscan/processor"
)

type configuredTarget struct {
	id     string
	target autoscan.Target
}

func targetQueueID(kind, name, url string, rewrite []autoscan.Rewrite) string {
	if name != "" {
		return kind + ":" + name
	}
	if len(rewrite) == 0 {
		rewrite = nil
	}
	identity := struct {
		URL     string
		Rewrite []autoscan.Rewrite
	}{URL: strings.TrimRight(url, "/"), Rewrite: rewrite}
	// This value contains only strings and cannot fail JSON encoding.
	data, _ := json.Marshal(identity)
	return fmt.Sprintf("%s:%x", kind, sha256.Sum256(data))
}

// runTarget delivers queued scans to one target until ctx is cancelled, which
// is its only exit. Target state is controlled outside autoscan (proxies,
// tokens, upgrades) and may be repaired at any time, so no failure is treated
// as permanent: every failure keeps the queue, backs off, and re-checks
// availability before resuming delivery.
func runTarget(ctx context.Context, proc *processor.Processor, target configuredTarget, scanDelay time.Duration) error {
	available := false
	failures := 0
	var failingSince time.Time
	logger := log.With().Str("target_queue", target.id).Logger()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var err error
		if !available {
			err = target.target.Available()
			if err == nil {
				available = true
			}
		}
		if err == nil {
			err = proc.ProcessTarget(target.id, target.target)
		}

		delay := retryBaseDelay
		switch {
		case err == nil, errors.Is(err, autoscan.ErrNoScans):
			if failures > 0 {
				logger.Info().Int("failures", failures).Dur("outage", time.Since(failingSince)).
					Msg("Target recovered, resuming queued scans")
				failures = 0
			}
			if err == nil {
				delay = scanDelay
			} else {
				logger.Trace().Msg("No scans available for target")
			}
		case errors.Is(err, autoscan.ErrAnchorUnavailable):
			// The processor logs anchor outages and recovery once per state change.
		default:
			if failures == 0 {
				failingSince = time.Now()
			}
			failures++
			available = false
			proc.RecordRetry()
			delay = retryDelay(failures, autoscan.RetryDelay(err))
			event := logger.Error().Err(err).Int("failures", failures).Dur("retry_delay", delay)
			if errors.Is(err, autoscan.ErrTargetUnavailable) {
				event.Msg("Target unavailable, retaining queued scans")
			} else {
				event.Msg("Target failing with a non-transient error, retaining queued scans; check target configuration")
			}
		}
		if err := waitForTarget(ctx, delay); err != nil {
			return err
		}
	}
}

const (
	// retryBaseDelay is the wait after a target's first consecutive failure.
	retryBaseDelay = 15 * time.Second
	// retryMaxDelay caps exponential backoff so a target that stays down for
	// days is still re-checked every few minutes and recovers without a restart.
	retryMaxDelay = 5 * time.Minute
	// retryAfterMaxDelay bounds server-requested delays so a bogus Retry-After
	// cannot park a worker indefinitely.
	retryAfterMaxDelay = time.Hour
)

// retryDelay returns the capped exponential backoff for the given number of
// consecutive failures, raised to honour a bounded server-requested delay.
func retryDelay(failures int, retryAfter time.Duration) time.Duration {
	delay := retryMaxDelay
	if shift := failures - 1; shift >= 0 && shift < 8 {
		delay = min(retryBaseDelay<<shift, retryMaxDelay)
	}
	return max(delay, min(retryAfter, retryAfterMaxDelay))
}

func waitForTarget(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
