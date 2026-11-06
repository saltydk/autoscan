package plex

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/rs/zerolog"

	"github.com/saltydk/autoscan"
	"github.com/saltydk/autoscan/targets/internal/library"
)

type Config struct {
	Name          string             `yaml:"name"`
	ResponseLimit *int64             `yaml:"response-limit"`
	URL           string             `yaml:"url"`
	Token         string             `yaml:"token"`
	Rewrite       []autoscan.Rewrite `yaml:"rewrite"`
	Verbosity     string             `yaml:"verbosity"`
}

type target struct {
	url       string
	token     string
	libraries *library.Cache[plexLibrary]

	log     zerolog.Logger
	rewrite autoscan.Rewriter
	api     *apiClient
}

func New(c Config) (autoscan.Target, error) {
	l := autoscan.GetLogger(c.Verbosity).With().
		Str("target", "plex").
		Str("url", c.URL).Logger()

	rewriter, err := autoscan.NewRewriter(c.Rewrite)
	if err != nil {
		return nil, err
	}
	limit, err := autoscan.ResolveTargetResponseLimit(c.ResponseLimit)
	if err != nil {
		return nil, err
	}
	api := newAPIClient(c.URL, c.Token, l)
	api.responseLimit = limit

	t := &target{
		url:   c.URL,
		token: c.Token,

		log:     l,
		rewrite: rewriter,
		api:     api,
	}
	t.libraries = library.New(t.fetchLibraries,
		func(l plexLibrary) string { return l.Path }, func(l plexLibrary) string { return l.Name }, l)
	return t, nil
}

// Available verifies the server version and refreshes the library list.
func (t *target) Available() error {
	return t.libraries.Refresh()
}

func (t *target) Scan(scan autoscan.Scan) error {
	// determine library for this scan
	scanFolder := t.rewrite(scan.Folder)

	libs, err := t.libraries.Match(scanFolder)
	if err != nil {
		return err
	}

	// send scan request
	for _, lib := range libs {
		l := t.log.With().
			Str("path", scanFolder).
			Str("library", lib.Name).
			Logger()

		l.Trace().Msg("Sending scan request")

		if err := t.api.Scan(scanFolder, lib.ID); err != nil {
			return err
		}

		l.Info().Msg("Scan moved to target")
	}

	return nil
}

// fetchLibraries checks the server version before every library fetch so an
// unsupported server is never sent scans.
func (t *target) fetchLibraries() ([]plexLibrary, error) {
	version, err := t.api.Version()
	if err != nil {
		return nil, err
	}
	if !isSupportedVersion(version) {
		return nil, fmt.Errorf("plex running unsupported version %s: %w", version, autoscan.ErrFatal)
	}
	t.log.Debug().Msgf("Plex version: %s", version)
	return t.api.Libraries()
}

func isSupportedVersion(version string) bool {
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return false
	}

	major, _ := strconv.Atoi(parts[0])
	minor, _ := strconv.Atoi(parts[1])

	if major >= 2 || (major == 1 && minor >= 20) {
		return true
	}

	return false
}
