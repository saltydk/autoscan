package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/saltydk/autoscan"
	"github.com/saltydk/autoscan/processor"
)

func scanStats(proc *processor.Processor, interval time.Duration) {
	st := time.NewTicker(interval)
	for {
		select {
		case _ = <-st.C:
			// retrieve amount of scans remaining
			sm, err := proc.ScansRemaining()
			switch {
			case err == nil:
				metrics := proc.Metrics()
				log.Info().
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
			case errors.Is(err, autoscan.ErrFatal):
				log.Error().
					Err(err).
					Msg("Fatal error determining amount of remaining scans, scan stats stopped...")
				st.Stop()
				return
			default:
				// ErrNoScans should never occur as COUNT should always at-least return 0
				log.Error().
					Err(err).
					Msg("Failed determining amount of remaining scans")
			}
		}
	}
}
