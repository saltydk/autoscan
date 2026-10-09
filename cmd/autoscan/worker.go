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

func runTarget(ctx context.Context, proc *processor.Processor, target configuredTarget, scanDelay time.Duration) error {
	available := false
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
		delay := 15 * time.Second
		switch {
		case err == nil:
			delay = scanDelay
		case errors.Is(err, autoscan.ErrNoScans):
			logger.Trace().Msg("No scans available for target")
		case errors.Is(err, autoscan.ErrAnchorUnavailable):
			logger.Error().Err(err).Msg("Anchor unavailable, retaining queued scans")
		case errors.Is(err, autoscan.ErrTargetUnavailable):
			proc.RecordRetry()
			available = false
			delay = max(delay, autoscan.RetryDelay(err))
			logger.Error().Err(err).Dur("retry_delay", delay).Msg("Target unavailable, retaining queued scans")
		default:
			return err
		}
		if err := waitForTarget(ctx, delay); err != nil {
			return err
		}
	}
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
