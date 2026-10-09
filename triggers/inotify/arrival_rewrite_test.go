package inotify

import (
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
	"time"

	"github.com/saltydk/autoscan"
)

func TestInotifyPopulatedArrivalRewriteDelivery(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("these public delivery cases validate the Linux inotify backend")
	}
	scenarios := []struct {
		name    string
		arrival string
		files   []string
		rules   func(string) []autoscan.Rewrite
		exclude []string
		want    []string
	}{
		{
			name: "prefix_movie_keeps_one_movie_folder", arrival: "Movies/Example.Movie.2026",
			files: []string{"movie.mkv", "sample.mkv", "movie.nfo"},
			rules: arrivalRewritePrefixRules,
			want:  []string{"/mapped/Movies/Example.Movie.2026"},
		},
		{
			name: "prefix_show_keeps_outer_show_folder", arrival: "TV/Example.Show.2026",
			files: []string{"Season 01/episode.mkv", "Season 02/episode.mkv"},
			rules: arrivalRewritePrefixRules,
			want:  []string{"/mapped/TV/Example.Show.2026"},
		},
		{
			name: "filename_capture_maps_movie_folder", arrival: "Movies/Example.Movie.2026",
			files: []string{"movie.mkv", "sample.mkv", "movie.nfo"},
			rules: arrivalRewriteFilenameRules,
			want:  []string{"/mapped/Movies/Example.Movie.2026"},
		},
		{
			name: "filename_capture_maps_each_season", arrival: "TV/Example.Show.2026",
			files: []string{"Season 01/episode.mkv", "Season 02/episode.mkv"},
			rules: arrivalRewriteFilenameRules,
			want:  []string{"/mapped/TV/Example.Show.2026/Season 01", "/mapped/TV/Example.Show.2026/Season 02"},
		},
		{
			name: "multiple_mapped_parents_deduplicate_and_honor_filters", arrival: "Movies/Example.Movie.2026",
			files: []string{"hd.mkv", "hd-sample.mkv", "uhd.mkv", "sd.mkv", "movie.nfo"},
			rules: func(root string) []autoscan.Rewrite {
				prefix := "^" + regexp.QuoteMeta(root) + `/Movies/([^/]+)/`
				return []autoscan.Rewrite{
					{From: prefix + `hd\.mkv$`, To: "/mapped/HD/$1/video.mkv"},
					{From: prefix + `hd-sample\.mkv$`, To: "/mapped/HD/$1/sample.mkv"},
					{From: prefix + `uhd\.mkv$`, To: "/mapped/UHD/$1/video.mkv"},
					{From: prefix + `sd\.mkv$`, To: "/mapped/SD/$1/video.mkv"},
				}
			},
			exclude: []string{`^/mapped/SD/`},
			want:    []string{"/mapped/HD/Example.Movie.2026", "/mapped/UHD/Example.Movie.2026"},
		},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			if !runInotifyScenario(t) {
				return
			}
			base := t.TempDir()
			root := filepath.Join(base, "Media")
			staged := filepath.Join(base, "staging", filepath.Base(scenario.arrival))
			for _, file := range scenario.files {
				name := filepath.Join(staged, file)
				inotifyScenarioMakeDir(t, filepath.Dir(name))
				inotifyScenarioWrite(t, name)
			}
			destination := filepath.Join(root, scenario.arrival)
			inotifyScenarioMakeDir(t, filepath.Dir(destination))
			c := Config{
				Priority: 7,
				Paths:    []configScenarioPath{{Path: root}},
				Rewrite:  scenario.rules(root),
				Include:  []string{`^/mapped/.*\.mkv$`},
				Exclude:  scenario.exclude,
			}
			recorder := startPublicInotifyScenario(t, c)
			inotifyScenarioRename(t, staged, destination)
			time.Sleep(queueDebounceDelay + time.Second)
			scans := recorder.snapshot()
			assertInotifyScenarioFolders(t, recorder.folders(), scenario.want)
			if len(scans) != len(scenario.want) {
				t.Fatalf("got %d callback scans, want %d: %+v", len(scans), len(scenario.want), scans)
			}
			for _, scan := range scans {
				if scan.Priority != 7 || scan.Time.IsZero() {
					t.Errorf("mapped arrival lost scan metadata: %+v", scan)
				}
			}
		})
	}
}

func arrivalRewritePrefixRules(root string) []autoscan.Rewrite {
	return []autoscan.Rewrite{{From: "^" + regexp.QuoteMeta(root) + "/", To: "/mapped/"}}
}

func arrivalRewriteFilenameRules(root string) []autoscan.Rewrite {
	return []autoscan.Rewrite{{From: "^" + regexp.QuoteMeta(root) + `/(.*)/[^/]+\.mkv$`, To: "/mapped/$1/video.mkv"}}
}
