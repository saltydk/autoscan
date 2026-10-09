package main

import (
	"errors"
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
