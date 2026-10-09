package emby

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/rs/zerolog"

	"github.com/saltydk/autoscan"
)

type Config struct {
	Name      string             `yaml:"name"`
	URL       string             `yaml:"url"`
	Token     string             `yaml:"token"`
	Rewrite   []autoscan.Rewrite `yaml:"rewrite"`
	Verbosity string             `yaml:"verbosity"`
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
		Str("target", "emby").
		Str("url", c.URL).
		Logger()

	rewriter, err := autoscan.NewRewriter(c.Rewrite)
	if err != nil {
		return nil, err
	}
	api := newAPIClient(c.URL, c.Token, l)

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
		if errors.Is(err, autoscan.ErrTargetUnavailable) || errors.Is(err, autoscan.ErrFatal) {
			return err
		}
		t.log.Warn().
			Err(err).
			Msg("No target libraries found")

		return nil
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
	librariesSnapshot, err := t.loadLibraries()
	if err != nil {
		return nil, err
	}
	for _, l := range librariesSnapshot {
		if strings.HasPrefix(folder, l.Path) {
			return &l, nil
		}
	}

	return nil, fmt.Errorf("%v: failed determining library", folder)
}
