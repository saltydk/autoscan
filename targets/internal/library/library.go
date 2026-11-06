// Package library resolves scan folders to media-server libraries for the
// Plex, Emby, and Jellyfin targets, keeping the library list current so that
// libraries added or remapped on the server are found without a restart.
package library

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/saltydk/autoscan"
)

const (
	// RefreshInterval is the minimum time between refreshes triggered by a scan
	// that matched no library, bounding requests when such scans are routine.
	RefreshInterval = time.Minute
	// SummaryInterval spaces the summaries of scans skipped as unmatched.
	SummaryInterval = time.Hour
)

var now = time.Now

// Cache holds one target's libraries. Root returns a library's path on the
// media server and Name its display name.
type Cache[L any] struct {
	fetch func() ([]L, error)
	root  func(L) string
	name  func(L) string
	log   zerolog.Logger

	mu          sync.Mutex
	libraries   []L
	loaded      bool
	loadedAt    time.Time
	summarized  time.Time
	unmatched   int
	lastMissing string
}

// New returns an empty cache; libraries are fetched on first use.
func New[L any](fetch func() ([]L, error), root, name func(L) string, log zerolog.Logger) *Cache[L] {
	return &Cache[L]{fetch: fetch, root: root, name: name, log: log}
}

// Refresh fetches the library list unconditionally. Targets call it from
// Available, which runs at startup and before resuming after any failure. An
// empty list does not replace a previously retrieved one.
func (c *Cache[L]) Refresh() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.refreshLocked()
}

func (c *Cache[L]) refreshLocked() error {
	libraries, err := c.fetch()
	if err != nil {
		return err
	}
	if len(libraries) == 0 && len(c.libraries) > 0 {
		// Keep a known list rather than replace it with an empty answer, so a
		// refresh never leaves the target worse off than not refreshing.
		c.log.Warn().Msg("Target reported no libraries, keeping the previously retrieved libraries")
		c.loadedAt = now()
		return nil
	}
	c.libraries, c.loaded, c.loadedAt = libraries, true, now()
	c.log.Debug().Interface("libraries", libraries).Msg("Retrieved libraries")
	return nil
}

// Match returns every library strictly containing folder.
//
// A folder that matches no library triggers a rate-limited refresh before it is
// reported as ErrLibraryNotMatched, so a library added after the list was
// fetched is not skipped; a failed refresh returns its error so the scan is
// retried rather than skipped. A folder that is exactly a library root, and is
// not inside another library, is rejected with ErrScanRejected because
// delivering it would rescan that entire library.
func (c *Cache[L]) Match(folder string) ([]L, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loaded {
		if err := c.refreshLocked(); err != nil {
			return nil, err
		}
	}
	matched, err := c.match(folder)
	if err == nil || !isUnmatched(err) {
		return matched, err
	}
	if now().Sub(c.loadedAt) >= RefreshInterval {
		if rerr := c.refreshLocked(); rerr != nil {
			return nil, rerr
		}
		if matched, err = c.match(folder); err == nil || !isUnmatched(err) {
			return matched, err
		}
	}
	c.unmatched++
	c.lastMissing = folder
	if c.summarized.IsZero() || now().Sub(c.summarized) >= SummaryInterval {
		c.log.Info().Int("skipped", c.unmatched).Str("example_path", c.lastMissing).
			Msg("Skipping scans outside this target's libraries; check rewrite rules if this target should receive them")
		c.summarized, c.unmatched = now(), 0
	}
	return nil, err
}

func (c *Cache[L]) match(folder string) ([]L, error) {
	clean := path.Clean(folder)
	var contained []L
	var rootOf *L
	for i, l := range c.libraries {
		root := c.root(l)
		if root == "" {
			continue
		}
		root = path.Clean(root)
		switch {
		case clean == root:
			rootOf = &c.libraries[i]
		case strings.HasPrefix(clean, strings.TrimRight(root, "/")+"/"):
			contained = append(contained, l)
		}
	}
	if len(contained) > 0 {
		return contained, nil
	}
	if rootOf != nil {
		return nil, fmt.Errorf("%s is the root of library %q; scan a movie, show, or season folder instead: %w",
			folder, c.name(*rootOf), autoscan.ErrScanRejected)
	}
	return nil, fmt.Errorf("%s: %w", folder, autoscan.ErrLibraryNotMatched)
}

func isUnmatched(err error) bool {
	return errors.Is(err, autoscan.ErrLibraryNotMatched)
}
