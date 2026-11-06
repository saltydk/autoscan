package main

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/saltydk/autoscan/processor"
)

// scanStats reports queue statistics every interval until ctx is cancelled.
// A failed count is logged and retried on the next tick, never ending reporting.
func scanStats(ctx context.Context, proc *processor.Processor, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		sm, err := proc.ScansRemaining()
		if err != nil {
			log.Error().Err(err).Msg("Failed determining amount of remaining scans, retrying at next interval")
			continue
		}
		dormant, err := proc.DormantScans()
		if err != nil {
			log.Error().Err(err).Msg("Failed determining amount of dormant scans, retrying at next interval")
			continue
		}
		metrics := proc.Metrics()
		event := log.Info()
		if dormant > 0 {
			// Queued for targets that are no longer configured; see the startup warnings.
			event = event.Int("dormant", dormant)
		}
		event.
			Int("remaining", sm).
			Int64("received", metrics.Received).
			Int64("processed", metrics.Processed).
			Int64("retried", metrics.Retried).
			Int64("skipped", metrics.Skipped).
			Int64("rejected", metrics.Rejected).
			Msg("Scan stats")
		status := fmt.Sprintf("STATUS=Received %d events; processed %d deliveries, %d retryable target failures, skipped %d, rejected %d; %d queued",
			metrics.Received, metrics.Processed, metrics.Retried, metrics.Skipped, metrics.Rejected, sm)
		if err := notifyService(status); err != nil {
			log.Debug().Err(err).Msg("Failed notifying service status")
		}
	}
}
