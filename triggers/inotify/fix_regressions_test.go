//go:build linux

package inotify

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/saltydk/autoscan"
)

func TestInotifyFixRegressionFilteredRenameCleansWatches(t *testing.T) {
	t.Parallel()
	if !runInotifyScenario(t) {
		return
	}
	root := filepath.Join(t.TempDir(), "TV")
	old := filepath.Join(root, "Excluded.Show.2026")
	next := filepath.Join(root, "Allowed.Show.2026")
	oldSeason := filepath.Join(old, "Season 01")
	nextSeason := filepath.Join(next, "Season 01")
	inotifyScenarioMakeDir(t, oldSeason)
	inotifyScenarioWrite(t, filepath.Join(oldSeason, "Example Show - S01E01.mkv"))
	p := inotifyScenarioRoot(root)
	var err error
	p.Allowed, err = autoscan.NewFilterer(nil, []string{regexp.QuoteMeta(old)})
	if err != nil {
		t.Fatal(err)
	}
	d := newInotifyScenarioDaemon(t, p)
	inotifyScenarioRename(t, old, next)
	assertInotifyScenarioFolders(t, collectInotifyScenarioFolders(d), []string{next})
	for _, watch := range d.watcher.WatchList() {
		if watch == old || strings.HasPrefix(watch, old+string(filepath.Separator)) {
			t.Errorf("filtered old directory retained a stale watch: %s", watch)
		}
	}
	if !slices.Contains(d.watcher.WatchList(), nextSeason) {
		t.Errorf("renamed season lacks its destination watch: %v", d.watcher.WatchList())
	}
	inotifyScenarioWrite(t, filepath.Join(nextSeason, "Example Show - S01E02.mkv"))
	assertInotifyScenarioFolders(t, collectInotifyScenarioFolders(d), []string{nextSeason})
}

func TestInotifyFixRegressionCleanupPreservesPrefixSibling(t *testing.T) {
	t.Parallel()
	if !runInotifyScenario(t) {
		return
	}
	base := t.TempDir()
	root := filepath.Join(base, "TV")
	old := filepath.Join(root, "Example Show")
	sibling := filepath.Join(root, "Example Show Remastered")
	siblingSeason := filepath.Join(sibling, "Season 01")
	for _, season := range []string{filepath.Join(old, "Season 01"), siblingSeason} {
		inotifyScenarioMakeDir(t, season)
		inotifyScenarioWrite(t, filepath.Join(season, "Example Show - S01E01.mkv"))
	}
	d := newInotifyScenarioDaemon(t, inotifyScenarioRoot(root))
	inotifyScenarioRename(t, old, filepath.Join(base, "Removed Show"))
	assertInotifyScenarioFolders(t, collectInotifyScenarioFolders(d), []string{old})
	if !slices.Contains(d.watcher.WatchList(), siblingSeason) {
		t.Errorf("subtree cleanup removed a prefix sibling's season watch: %v", d.watcher.WatchList())
	}
	inotifyScenarioWrite(t, filepath.Join(siblingSeason, "Example Show - S01E02.mkv"))
	assertInotifyScenarioFolders(t, collectInotifyScenarioFolders(d), []string{siblingSeason})
}

func TestInotifyFixRegressionPopulatedArrivalFilters(t *testing.T) {
	for _, scenario := range []struct {
		name             string
		include          []string
		exclude          []string
		files            []string
		wantScan         bool
		symlink          bool
		directorySymlink bool
	}{
		{
			name: "mkv_include_accepts_media_in_populated_movie", include: []string{`\.mkv$`},
			files: []string{"Example Movie (2026).mkv", "Example Movie (2026).nfo", "Example Movie (2026).en.srt"}, wantScan: true,
		},
		{
			name: "mkv_include_rejects_sidecar_only_movie", include: []string{`\.mkv$`},
			files: []string{"Example Movie (2026).nfo", "Example Movie (2026).en.srt"},
		},
		{
			name: "sidecar_exclusions_reject_all_contained_files", exclude: []string{`\.(nfo|srt)$`},
			files: []string{"Example Movie (2026).nfo", "Example Movie (2026).en.srt"},
		},
		{
			name: "mkv_include_accepts_imported_symlink_media", include: []string{`\.mkv$`},
			wantScan: true, symlink: true,
		},
		{name: "directory_symlink_alone_is_not_contained_media", directorySymlink: true},
		{name: "empty_arrival_waits_for_media"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			if !runInotifyScenario(t) {
				return
			}
			base := t.TempDir()
			root := filepath.Join(base, "Movies")
			source := filepath.Join(base, "staging", "Example Movie (2026)")
			destination := filepath.Join(root, "Example Movie (2026)")
			inotifyScenarioMakeDir(t, root)
			inotifyScenarioMakeDir(t, source)
			for _, file := range scenario.files {
				inotifyScenarioWrite(t, filepath.Join(source, file))
			}
			if scenario.symlink {
				media := filepath.Join(base, "downloaded.mkv")
				inotifyScenarioWrite(t, media)
				if err := os.Symlink(media, filepath.Join(source, "Example Movie (2026).mkv")); err != nil {
					t.Fatal(err)
				}
			}
			if scenario.directorySymlink {
				external := filepath.Join(base, "external", "Other Movie (2026)")
				inotifyScenarioMakeDir(t, external)
				inotifyScenarioWrite(t, filepath.Join(external, "Other Movie (2026).mkv"))
				if err := os.Symlink(external, filepath.Join(source, "Linked Directory")); err != nil {
					t.Fatal(err)
				}
			}
			p := inotifyScenarioRoot(root)
			var err error
			p.Allowed, err = autoscan.NewFilterer(scenario.include, scenario.exclude)
			if err != nil {
				t.Fatal(err)
			}
			d := newInotifyScenarioDaemon(t, p)
			inotifyScenarioRename(t, source, destination)
			inotifyScenarioWaitForWatch(t, d, destination)
			var want []string
			if scenario.wantScan {
				want = []string{destination}
			}
			assertInotifyScenarioFolders(t, collectInotifyScenarioFolders(d), want)
			// Filters control submitted work; they must not prevent directory watches.
			inotifyScenarioWrite(t, filepath.Join(destination, "later.mkv"))
			assertInotifyScenarioFolders(t, collectInotifyScenarioFolders(d), []string{destination})
		})
	}
}

func TestInotifyFixRegressionRepeatedDottedDirectoryChanges(t *testing.T) {
	t.Parallel()
	if !runInotifyScenario(t) {
		return
	}
	root := filepath.Join(t.TempDir(), "Movies")
	names := []string{
		"Example.Movie.2026.1080p", "Example.Movie.2026.2160p", "Example.Movie.2026.Remux",
	}
	current := filepath.Join(root, names[0])
	inotifyScenarioMakeDir(t, current)
	inotifyScenarioWrite(t, filepath.Join(current, "Example Movie (2026).mkv"))
	p := inotifyScenarioRoot(root)
	var err error
	p.Rewriter, err = autoscan.NewRewriter([]autoscan.Rewrite{{From: "^" + regexp.QuoteMeta(root), To: "/plex/Movies"}})
	if err != nil {
		t.Fatal(err)
	}
	d := newInotifyScenarioDaemon(t, p)
	for _, name := range names[1:] {
		next := filepath.Join(root, name)
		inotifyScenarioRename(t, current, next)
		got := collectInotifyScenarioFolders(d)
		assertInotifyScenarioFolders(t, got, []string{p.Rewriter(current), p.Rewriter(next)})
		if slices.Contains(got, "/plex/Movies") {
			t.Error("a directory rename widened its scan to the library root")
		}
		current = next
	}
	if err := os.RemoveAll(current); err != nil {
		t.Fatal(err)
	}
	got := collectInotifyScenarioFolders(d)
	assertInotifyScenarioFolders(t, got, []string{p.Rewriter(current)})
	if slices.Contains(got, "/plex/Movies") {
		t.Error("a directory removal widened its scan to the library root")
	}
}
