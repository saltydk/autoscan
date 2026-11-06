package jellyfin

import (
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
	libraries *library.Cache[jellyfinLibrary]

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

	t := &target{
		url:   c.URL,
		token: c.Token,

		log:     l,
		rewrite: rewriter,
		api:     api,
	}
	t.libraries = library.New(api.Libraries,
		func(l jellyfinLibrary) string { return l.Path }, func(l jellyfinLibrary) string { return l.Name }, l)
	return t, nil
}

func (t *target) Available() error {
	if err := t.api.Available(); err != nil {
		return err
	}
	return t.libraries.Refresh()
}

func (t *target) Scan(scan autoscan.Scan) error {
	// determine library for this scan
	scanFolder := t.rewrite(scan.Folder)

	libs, err := t.libraries.Match(scanFolder)
	if err != nil {
		return err
	}
	lib := libs[0]

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
