package library

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/saltydk/autoscan"
)

type lib struct{ name, root string }

type fakeServer struct {
	libraries []lib
	fetches   int
	err       error
}

func (s *fakeServer) cache() *Cache[lib] {
	return New(func() ([]lib, error) {
		s.fetches++
		if s.err != nil {
			return nil, s.err
		}
		return slices.Clone(s.libraries), nil
	}, func(l lib) string { return l.root }, func(l lib) string { return l.name }, zerolog.Nop())
}

func useClock(t *testing.T) *time.Time {
	t.Helper()
	clock := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	previous := now
	now = func() time.Time { return clock }
	t.Cleanup(func() { now = previous })
	return &clock
}

func names(libraries []lib) []string {
	out := make([]string, 0, len(libraries))
	for _, l := range libraries {
		out = append(out, l.name)
	}
	return out
}

func TestMatchRootRules(t *testing.T) {
	useClock(t)
	server := &fakeServer{libraries: []lib{
		{"Media", "/media"}, {"Movies", "/media/movies"}, {"TV", "/tv/"}, {"Empty", ""},
	}}
	cache := server.cache()
	for _, tc := range []struct {
		folder string
		want   []string
		err    error
	}{
		{"/media/movies/Film (2026)", []string{"Media", "Movies"}, nil},
		// A nested library's root is inside its parent; v1.4.5 delivered it there.
		{"/media/movies", []string{"Media"}, nil},
		{"/media/movies/", []string{"Media"}, nil},
		{"/media", nil, autoscan.ErrScanRejected},
		{"/tv", nil, autoscan.ErrScanRejected},
		{"/tv/Show/Season 1", []string{"TV"}, nil},
		{"/media-old/Film", nil, autoscan.ErrLibraryNotMatched},
	} {
		got, err := cache.Match(tc.folder)
		if !errors.Is(err, tc.err) || (tc.err == nil && err != nil) {
			t.Errorf("Match(%q) error = %v, want %v", tc.folder, err, tc.err)
		}
		if !slices.Equal(names(got), tc.want) && !(len(got) == 0 && len(tc.want) == 0) {
			t.Errorf("Match(%q) = %v, want %v", tc.folder, names(got), tc.want)
		}
	}
	if server.fetches != 1 {
		t.Errorf("fetches = %d, want 1 (misses within RefreshInterval reuse the list)", server.fetches)
	}
}

func TestMatchRefreshesOnMissAfterInterval(t *testing.T) {
	clock := useClock(t)
	server := &fakeServer{libraries: []lib{{"TV", "/tv"}}}
	cache := server.cache()
	if _, err := cache.Match("/movies/Film"); !errors.Is(err, autoscan.ErrLibraryNotMatched) {
		t.Fatalf("Match before library exists = %v", err)
	}
	server.libraries = append(server.libraries, lib{"Movies", "/movies"})
	if _, err := cache.Match("/movies/Film"); !errors.Is(err, autoscan.ErrLibraryNotMatched) {
		t.Fatalf("Match within RefreshInterval = %v, want cached miss", err)
	}
	*clock = clock.Add(RefreshInterval)
	got, err := cache.Match("/movies/Film")
	if err != nil || !slices.Equal(names(got), []string{"Movies"}) {
		t.Fatalf("Match after library added = %v, %v", names(got), err)
	}
	if server.fetches != 2 {
		t.Errorf("fetches = %d, want 2", server.fetches)
	}
}

func TestMatchRetriesWhenRefreshFails(t *testing.T) {
	clock := useClock(t)
	server := &fakeServer{libraries: []lib{{"TV", "/tv"}}}
	cache := server.cache()
	if _, err := cache.Match("/tv/Show"); err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(RefreshInterval)
	server.err = autoscan.ErrTargetUnavailable
	if _, err := cache.Match("/movies/Film"); !errors.Is(err, autoscan.ErrTargetUnavailable) {
		t.Fatalf("Match with failing refresh = %v, want retryable error instead of a skip", err)
	}
}

func TestRefreshReplacesLibraries(t *testing.T) {
	useClock(t)
	server := &fakeServer{libraries: []lib{{"TV", "/old/tv"}}}
	cache := server.cache()
	if _, err := cache.Match("/old/tv/Show"); err != nil {
		t.Fatal(err)
	}
	server.libraries = []lib{{"TV", "/new/tv"}}
	if err := cache.Refresh(); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Match("/new/tv/Show"); err != nil {
		t.Fatalf("Match after remap = %v", err)
	}
}

// A target with no libraries skips every scan as unmatched, as in v1.4.6.
func TestEmptyLibraryListIsAccepted(t *testing.T) {
	useClock(t)
	server := &fakeServer{}
	cache := server.cache()
	if err := cache.Refresh(); err != nil {
		t.Fatalf("Refresh() with no libraries = %v", err)
	}
	if _, err := cache.Match("/tv/Show"); !errors.Is(err, autoscan.ErrLibraryNotMatched) {
		t.Fatalf("Match() with no libraries = %v, want ErrLibraryNotMatched", err)
	}
}

// Refreshing must not make things worse than never refreshing: an empty answer
// does not replace a list that was already known.
func TestEmptyRefreshKeepsPreviousLibraries(t *testing.T) {
	clock := useClock(t)
	server := &fakeServer{libraries: []lib{{"TV", "/tv"}}}
	cache := server.cache()
	if err := cache.Refresh(); err != nil {
		t.Fatal(err)
	}
	server.libraries = nil
	if err := cache.Refresh(); err != nil {
		t.Fatalf("Refresh() returning nothing = %v", err)
	}
	*clock = clock.Add(RefreshInterval)
	if _, err := cache.Match("/movies/Film"); !errors.Is(err, autoscan.ErrLibraryNotMatched) {
		t.Fatalf("Match() outside libraries = %v", err)
	}
	if _, err := cache.Match("/tv/Show"); err != nil {
		t.Fatalf("known library lost after empty refresh: %v", err)
	}
}
