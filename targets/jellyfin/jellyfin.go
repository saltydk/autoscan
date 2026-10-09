package jellyfin

import (
	"fmt"
	"path"
	"strings"
	"sync"

	"github.com/rs/zerolog"

	"github.com/saltydk/autoscan"
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
	libraries []library
	libraryMu sync.Mutex

	log     zerolog.Logger
	rewrite autoscan.Rewriter
	api     apiClient
}

func New(c Config) (autoscan.Target, error) {
	l := autoscan.GetLogger(c.Verbosity).With().
		Str("target", "jellyfin").
		Str("url", c.URL).
		Logger()

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

	return &target{
		url:   c.URL,
		token: c.Token,

		log:     l,
		rewrite: rewriter,
		api:     api,
	}, nil
}

func (t *target) Available() error {
	if err := t.api.Available(); err != nil {
		return err
	}
	_, err := t.loadLibraries()
	return err
}

func (t *target) Scan(scan autoscan.Scan) error {
	// determine library for this scan
	scanFolder := t.rewrite(scan.Folder)

	lib, err := t.getScanLibrary(scanFolder)
	if err != nil {
		return err
	}

	l := t.log.With().
		Str("path", scanFolder).
		Str("library", lib.Name).
		Logger()

	// send scan request
	l.Trace().Msg("Sending scan request")

	if err := t.api.Scan(scanFolder); err != nil {
		return err
	}

	l.Info().Msg("Scan moved to target")
	return nil
}

func (t *target) loadLibraries() ([]library, error) {
	t.libraryMu.Lock()
	defer t.libraryMu.Unlock()
	if t.libraries == nil {
		libraries, err := t.api.Libraries()
		if err != nil {
			return nil, err
		}
		t.libraries = libraries
		t.log.Debug().Interface("libraries", libraries).Msg("Retrieved libraries")
	}
	return t.libraries, nil
}

func (t *target) getScanLibrary(folder string) (*library, error) {
	libraries, err := t.loadLibraries()
	if err != nil {
		return nil, err
	}
	cleanFolder := path.Clean(folder)
	for _, l := range libraries {
		if l.Path != "" && cleanFolder == path.Clean(l.Path) {
			return nil, fmt.Errorf("%s is the root of library %q; scan a movie, show, or season folder instead: %w", folder, l.Name, autoscan.ErrScanRejected)
		}
	}
	for _, l := range libraries {
		if strings.HasPrefix(cleanFolder, strings.TrimRight(path.Clean(l.Path), "/")+"/") {
			return &l, nil
		}
	}

	return nil, fmt.Errorf("%s: %w", folder, autoscan.ErrLibraryNotMatched)
}
