package plex

import (
	"fmt"
	"path"
	"strconv"
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

	return &target{
		url:   c.URL,
		token: c.Token,

		log:     l,
		rewrite: rewriter,
		api:     api,
	}, nil
}

func (t *target) Available() error {
	_, err := t.loadLibraries(true)
	return err
}

func (t *target) Scan(scan autoscan.Scan) error {
	// determine library for this scan
	scanFolder := t.rewrite(scan.Folder)

	libs, err := t.getScanLibrary(scanFolder)
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

func (t *target) loadLibraries(checkAvailability bool) ([]library, error) {
	t.libraryMu.Lock()
	defer t.libraryMu.Unlock()
	if checkAvailability || t.libraries == nil {
		version, err := t.api.Version()
		if err != nil {
			return nil, err
		}
		if !isSupportedVersion(version) {
			return nil, fmt.Errorf("plex running unsupported version %s: %w", version, autoscan.ErrFatal)
		}
		t.log.Debug().Msgf("Plex version: %s", version)
	}
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

func (t *target) getScanLibrary(folder string) ([]library, error) {
	available, err := t.loadLibraries(false)
	if err != nil {
		return nil, err
	}
	libraries := make([]library, 0)
	cleanFolder := path.Clean(folder)

	for _, l := range available {
		libraryRoot := path.Clean(l.Path)
		if l.Path != "" && cleanFolder == libraryRoot {
			return nil, fmt.Errorf("%s is the root of library %q; scan a movie, show, or season folder instead: %w", folder, l.Name, autoscan.ErrScanRejected)
		}
		if strings.HasPrefix(cleanFolder, strings.TrimRight(libraryRoot, "/")+"/") {
			libraries = append(libraries, l)
		}
	}

	if len(libraries) == 0 {
		return nil, fmt.Errorf("%s: %w", folder, autoscan.ErrLibraryNotMatched)
	}

	return libraries, nil
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
