package processor

import (
	"fmt"
	"slices"
	"strings"

	"github.com/rs/zerolog/log"

	"github.com/saltydk/autoscan"
)

func (p *Processor) checkAnchors() error {
	p.anchorMu.Lock()
	defer p.anchorMu.Unlock()

	var missing []string
	for _, anchor := range p.anchors {
		if !fileExists(anchor) {
			missing = append(missing, anchor)
		}
	}
	if !p.anchorObserved || !slices.Equal(missing, p.anchorMissing) {
		if len(missing) > 0 {
			log.Error().Strs("missing_anchors", missing).
				Msg("Anchor files unavailable, retaining queued scans")
		} else if p.anchorObserved && len(p.anchorMissing) > 0 {
			log.Info().Strs("anchors", p.anchors).
				Msg("Anchor files available, processing resumed")
		}
		p.anchorObserved = true
		p.anchorMissing = slices.Clone(missing)
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s: %w", strings.Join(missing, ", "), autoscan.ErrAnchorUnavailable)
	}
	return nil
}
