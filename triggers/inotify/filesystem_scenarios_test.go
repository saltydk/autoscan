package inotify

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/saltydk/autoscan"
)

type inotifyFilesystemFixture struct {
	root, staging, movies, tv, movie, other, show, season, album string
	d                                                            *daemon
}

func newInotifyFilesystemFixture(t *testing.T) inotifyFilesystemFixture {
	t.Helper()
	base := t.TempDir()
	fixture := inotifyFilesystemFixture{
		root: filepath.Join(base, "media"), staging: filepath.Join(base, "staging"),
	}
	fixture.movies = filepath.Join(fixture.root, "Movies")
	fixture.tv = filepath.Join(fixture.root, "TV")
	fixture.movie = filepath.Join(fixture.movies, "Example Movie (2026)")
	fixture.other = filepath.Join(fixture.movies, "Another Movie (2026)")
	fixture.show = filepath.Join(fixture.tv, "Example Show (2026)")
	fixture.season = filepath.Join(fixture.show, "Season 01")
	fixture.album = filepath.Join(fixture.root, "Music", "Example Artist", "Example Album (2026)")
	for _, directory := range []string{fixture.movie, fixture.other, fixture.season, fixture.album, fixture.staging} {
		inotifyScenarioMakeDir(t, directory)
	}
	inotifyScenarioWrite(t, filepath.Join(fixture.movie, "existing.mkv"))
	inotifyScenarioWrite(t, filepath.Join(fixture.season, "existing.mkv"))
	fixture.d = newInotifyScenarioDaemon(t, inotifyScenarioRoot(fixture.root))
	return fixture
}

// These tests exercise real kernel events through the production watcher and
// event handler. They compare folder destinations, not event order or counts.
func TestInotifyFilesystemScenarios(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("this scenario suite validates the Linux inotify backend")
	}
	scenarios := []struct {
		name string
		run  func(*testing.T, inotifyFilesystemFixture)
	}{
		{"existing_tree_watched_without_initial_scan", func(t *testing.T, f inotifyFilesystemFixture) {
			if !slices.Contains(f.d.watcher.WatchList(), f.season) {
				t.Error("existing nested season directory is not watched")
			}
			assertInotifyScenarioFolders(t, collectInotifyScenarioFolders(f.d), nil)
		}},
		{"create_media_file", func(t *testing.T, f inotifyFilesystemFixture) {
			inotifyScenarioWrite(t, filepath.Join(f.movie, "new.mkv"))
			assertInotifyScenarioFolders(t, collectInotifyScenarioFolders(f.d), []string{f.movie})
		}},
		{"create_media_in_existing_nested_season", func(t *testing.T, f inotifyFilesystemFixture) {
			inotifyScenarioWrite(t, filepath.Join(f.season, "episode.mkv"))
			assertInotifyScenarioFolders(t, collectInotifyScenarioFolders(f.d), []string{f.season})
		}},
		{"create_movie_mp4", func(t *testing.T, f inotifyFilesystemFixture) {
			inotifyScenarioWrite(t, filepath.Join(f.movie, "Example Movie (2026).mp4"))
			assertInotifyScenarioFolders(t, collectInotifyScenarioFolders(f.d), []string{f.movie})
		}},
		{"create_album_flac", func(t *testing.T, f inotifyFilesystemFixture) {
			inotifyScenarioWrite(t, filepath.Join(f.album, "01 - Example Track.flac"))
			assertInotifyScenarioFolders(t, collectInotifyScenarioFolders(f.d), []string{f.album})
		}},
		{"create_subtitle_and_nfo_sidecars", func(t *testing.T, f inotifyFilesystemFixture) {
			inotifyScenarioWrite(t, filepath.Join(f.movie, "Example Movie (2026).en.srt"))
			inotifyScenarioWrite(t, filepath.Join(f.movie, "Example Movie (2026).nfo"))
			assertInotifyScenarioFolders(t, collectInotifyScenarioFolders(f.d), []string{f.movie})
		}},
		{"create_spaces_unicode_and_multiple_dots", func(t *testing.T, f inotifyFilesystemFixture) {
			inotifyScenarioWrite(t, filepath.Join(f.movie, "Episode æøå 01.en.mkv"))
			assertInotifyScenarioFolders(t, collectInotifyScenarioFolders(f.d), []string{f.movie})
		}},
		{"remove_media_file", func(t *testing.T, f inotifyFilesystemFixture) {
			inotifyScenarioRemove(t, filepath.Join(f.movie, "existing.mkv"))
			assertInotifyScenarioFolders(t, collectInotifyScenarioFolders(f.d), []string{f.movie})
		}},
		{"rename_media_file_within_movie", func(t *testing.T, f inotifyFilesystemFixture) {
			inotifyScenarioRename(t, filepath.Join(f.movie, "existing.mkv"), filepath.Join(f.movie, "renamed.mkv"))
			assertInotifyScenarioFolders(t, collectInotifyScenarioFolders(f.d), []string{f.movie})
		}},
		{"downloader_finalizes_partial_movie", func(t *testing.T, f inotifyFilesystemFixture) {
			partial := filepath.Join(f.movie, "Example Movie (2026).mkv.part")
			inotifyScenarioWrite(t, partial)
			_ = collectInotifyScenarioFolders(f.d)
			inotifyScenarioRename(t, partial, filepath.Join(f.movie, "Example Movie (2026).mkv"))
			assertInotifyScenarioFolders(t, collectInotifyScenarioFolders(f.d), []string{f.movie})
		}},
		{"move_media_file_between_watched_movies", func(t *testing.T, f inotifyFilesystemFixture) {
			inotifyScenarioRename(t, filepath.Join(f.movie, "existing.mkv"), filepath.Join(f.other, "existing.mkv"))
			assertInotifyScenarioFolders(t, collectInotifyScenarioFolders(f.d), []string{f.movie, f.other})
		}},
		{"move_media_file_from_unwatched_staging", func(t *testing.T, f inotifyFilesystemFixture) {
			source := filepath.Join(f.staging, "incoming.mkv")
			inotifyScenarioWrite(t, source)
			inotifyScenarioRename(t, source, filepath.Join(f.movie, "incoming.mkv"))
			assertInotifyScenarioFolders(t, collectInotifyScenarioFolders(f.d), []string{f.movie})
		}},
		{"move_media_file_out_of_watched_tree", func(t *testing.T, f inotifyFilesystemFixture) {
			inotifyScenarioRename(t, filepath.Join(f.movie, "existing.mkv"), filepath.Join(f.staging, "existing.mkv"))
			assertInotifyScenarioFolders(t, collectInotifyScenarioFolders(f.d), []string{f.movie})
		}},
		{"atomic_replacement_of_existing_media", func(t *testing.T, f inotifyFilesystemFixture) {
			source := filepath.Join(f.staging, "replacement.tmp")
			inotifyScenarioWrite(t, source)
			inotifyScenarioRename(t, source, filepath.Join(f.movie, "existing.mkv"))
			assertInotifyScenarioFolders(t, collectInotifyScenarioFolders(f.d), []string{f.movie})
		}},
		{"rapid_create_then_remove_media", func(t *testing.T, f inotifyFilesystemFixture) {
			file := filepath.Join(f.movie, "temporary.mkv")
			inotifyScenarioWrite(t, file)
			inotifyScenarioRemove(t, file)
			assertInotifyScenarioFolders(t, collectInotifyScenarioFolders(f.d), []string{f.movie})
		}},
		{"existing_file_write_is_ignored_policy", func(t *testing.T, f inotifyFilesystemFixture) {
			inotifyScenarioWrite(t, filepath.Join(f.movie, "existing.mkv"))
			assertInotifyScenarioFolders(t, collectInotifyScenarioFolders(f.d), nil)
		}},
		{"chmod_is_ignored_policy", func(t *testing.T, f inotifyFilesystemFixture) {
			if err := os.Chmod(filepath.Join(f.movie, "existing.mkv"), 0400); err != nil {
				t.Fatal(err)
			}
			assertInotifyScenarioFolders(t, collectInotifyScenarioFolders(f.d), nil)
		}},
		{"new_empty_directory_installs_watch_without_scan", func(t *testing.T, f inotifyFilesystemFixture) {
			directory := filepath.Join(f.show, "Season 02")
			inotifyScenarioMakeDir(t, directory)
			inotifyScenarioWaitForWatch(t, f.d, directory)
			assertInotifyScenarioFolders(t, collectInotifyScenarioFolders(f.d), nil)
		}},
		{"file_created_after_new_directory_watch", func(t *testing.T, f inotifyFilesystemFixture) {
			directory := filepath.Join(f.show, "Season 02")
			inotifyScenarioMakeDir(t, directory)
			inotifyScenarioWaitForWatch(t, f.d, directory)
			inotifyScenarioWrite(t, filepath.Join(directory, "episode.mkv"))
			assertInotifyScenarioFolders(t, collectInotifyScenarioFolders(f.d), []string{directory})
		}},
		{"episode_burst_in_new_watched_season", func(t *testing.T, f inotifyFilesystemFixture) {
			directory := filepath.Join(f.show, "Season 02")
			inotifyScenarioMakeDir(t, directory)
			inotifyScenarioWaitForWatch(t, f.d, directory)
			for episode := range 12 {
				inotifyScenarioWrite(t, filepath.Join(directory, fmt.Sprintf("Example Show - S02E%02d.mkv", episode+1)))
			}
			assertInotifyScenarioFolders(t, collectInotifyScenarioFolders(f.d), []string{directory})
		}},
		{"later_file_in_imported_nested_season", func(t *testing.T, f inotifyFilesystemFixture) {
			source := filepath.Join(f.staging, "Imported Show")
			destination := filepath.Join(f.tv, "Imported Show (2026)")
			season := filepath.Join(destination, "Season 1")
			inotifyScenarioMakeDir(t, filepath.Join(source, "Season 1"))
			inotifyScenarioWrite(t, filepath.Join(source, "Season 1", "episode.mkv"))
			inotifyScenarioRename(t, source, destination)
			inotifyScenarioWaitForWatch(t, f.d, season)
			_ = collectInotifyScenarioFolders(f.d)
			inotifyScenarioWrite(t, filepath.Join(season, "later.mkv"))
			assertInotifyScenarioFolders(t, collectInotifyScenarioFolders(f.d), []string{season})
		}},
		{"move_populated_directory_out_of_watched_tree", func(t *testing.T, f inotifyFilesystemFixture) {
			inotifyScenarioRename(t, f.movie, filepath.Join(f.staging, "Removed Movie"))
			assertInotifyScenarioFolders(t, collectInotifyScenarioFolders(f.d), []string{f.movie})
		}},
		{"moved_out_descendants_do_not_emit_stale_paths", func(t *testing.T, f inotifyFilesystemFixture) {
			next := filepath.Join(f.staging, "Removed Show")
			inotifyScenarioRename(t, f.show, next)
			_ = collectInotifyScenarioFolders(f.d)
			inotifyScenarioWrite(t, filepath.Join(next, "Season 01", "outside.mkv"))
			assertInotifyScenarioFolders(t, collectInotifyScenarioFolders(f.d), nil)
		}},
		{"renamed_descendant_watch_uses_new_path", func(t *testing.T, f inotifyFilesystemFixture) {
			next := filepath.Join(f.tv, "Renamed Show (2026)")
			season := filepath.Join(next, "Season 01")
			inotifyScenarioRename(t, f.show, next)
			_ = collectInotifyScenarioFolders(f.d)
			if !slices.Contains(f.d.watcher.WatchList(), season) {
				t.Errorf("renamed season has no watch at its new path; watches = %q", f.d.watcher.WatchList())
			}
			inotifyScenarioWrite(t, filepath.Join(season, "later.mkv"))
			assertInotifyScenarioFolders(t, collectInotifyScenarioFolders(f.d), []string{season})
		}},
		{"remove_populated_movie_directory", func(t *testing.T, f inotifyFilesystemFixture) {
			if err := os.RemoveAll(f.movie); err != nil {
				t.Fatal(err)
			}
			assertInotifyScenarioFolders(t, collectInotifyScenarioFolders(f.d), []string{f.movie})
		}},
		{"remove_dotted_movie_directory", func(t *testing.T, f inotifyFilesystemFixture) {
			directory := filepath.Join(f.movies, "Example.Movie.2026.1080p")
			inotifyScenarioMakeDir(t, directory)
			inotifyScenarioWaitForWatch(t, f.d, directory)
			inotifyScenarioRemove(t, directory)
			assertInotifyScenarioFolders(t, collectInotifyScenarioFolders(f.d), []string{directory})
		}},
		{"hardlink_creation_and_removal", func(t *testing.T, f inotifyFilesystemFixture) {
			link := filepath.Join(f.other, "linked.mkv")
			if err := os.Link(filepath.Join(f.movie, "existing.mkv"), link); err != nil {
				t.Fatal(err)
			}
			assertInotifyScenarioFolders(t, collectInotifyScenarioFolders(f.d), []string{f.other})
			inotifyScenarioRemove(t, link)
			assertInotifyScenarioFolders(t, collectInotifyScenarioFolders(f.d), []string{f.other})
		}},
		{"file_symlink_creation", func(t *testing.T, f inotifyFilesystemFixture) {
			if err := os.Symlink(filepath.Join(f.movie, "existing.mkv"), filepath.Join(f.other, "linked.mkv")); err != nil {
				t.Fatal(err)
			}
			assertInotifyScenarioFolders(t, collectInotifyScenarioFolders(f.d), []string{f.other})
		}},
		{"directory_symlink_not_recursed_policy", func(t *testing.T, f inotifyFilesystemFixture) {
			source := filepath.Join(f.staging, "External")
			link := filepath.Join(f.root, "Linked")
			inotifyScenarioMakeDir(t, source)
			if err := os.Symlink(source, link); err != nil {
				t.Fatal(err)
			}
			_ = collectInotifyScenarioFolders(f.d)
			inotifyScenarioWrite(t, filepath.Join(source, "outside.mkv"))
			assertInotifyScenarioFolders(t, collectInotifyScenarioFolders(f.d), nil)
		}},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			if !runInotifyScenario(t) {
				return
			}
			scenario.run(t, newInotifyFilesystemFixture(t))
		})
	}
}

func TestInotifyPublicFilesystemDelivery(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("this scenario suite validates the Linux inotify backend")
	}
	for _, mode := range []string{"episode_burst_coalesces", "renamed_season_later_episode"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			if !runInotifyScenario(t) {
				return
			}
			base := t.TempDir()
			root := filepath.Join(base, "media", "TV")
			old := filepath.Join(root, "Example Show (2026)", "Season 01")
			inotifyScenarioMakeDir(t, old)
			inotifyScenarioWrite(t, filepath.Join(old, "existing.mkv"))
			var c Config
			c.Priority = 7
			c.Paths = append(c.Paths, struct {
				Path    string             `yaml:"path"`
				Rewrite []autoscan.Rewrite `yaml:"rewrite"`
				Include []string           `yaml:"include"`
				Exclude []string           `yaml:"exclude"`
			}{Path: root})
			recorder := startPublicInotifyScenario(t, c)
			var want []string
			baseline := 0
			switch mode {
			case "episode_burst_coalesces":
				for episode := range 12 {
					inotifyScenarioWrite(t, filepath.Join(old, fmt.Sprintf("episode-%02d.mkv", episode)))
				}
				want = []string{old}
			case "populated_directory_import":
				source := filepath.Join(base, "staging", "Imported Show")
				destination := filepath.Join(root, "Imported Show")
				inotifyScenarioMakeDir(t, filepath.Join(source, "Season 1"))
				inotifyScenarioWrite(t, filepath.Join(source, "Season 1", "episode.mkv"))
				inotifyScenarioRename(t, source, destination)
				want = []string{destination}
			case "directory_rename":
				next := filepath.Join(root, "Example Show (2026)", "Season 02")
				inotifyScenarioRename(t, old, next)
				want = []string{old, next}
			case "renamed_season_later_episode":
				next := filepath.Join(root, "Example Show (2026)", "Season 02")
				inotifyScenarioRename(t, old, next)
				time.Sleep(queueDebounceDelay + time.Second)
				baseline = len(recorder.snapshot())
				inotifyScenarioWrite(t, filepath.Join(next, "later.mkv"))
				want = []string{next}
			}
			// Observe the actual ten-second debounce and enough quiet time to catch
			// a duplicate callback, without accelerating production timers.
			time.Sleep(queueDebounceDelay + time.Second)
			scans := recorder.snapshot()[baseline:]
			var folders []string
			for _, scan := range scans {
				folders = append(folders, scan.Folder)
			}
			assertInotifyScenarioFolders(t, folders, want)
			if len(scans) != len(want) {
				t.Errorf("callback scans = %d, want %d: %+v", len(scans), len(want), scans)
			}
			for _, scan := range scans {
				if scan.Priority != 7 || scan.Time.IsZero() {
					t.Errorf("callback lost priority or timestamp: %+v", scan)
				}
			}
		})
	}
}
