package bernard

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/saltydk/autoscan"
)

func TestBernardMediaFolderRewriteCompatibility(t *testing.T) {
	for _, tt := range []struct {
		name          string
		rewrite       string
		sibling       bool
		extraFolders  bool
		duplicateOld  bool
		includeMapped bool
		exclude       string
		wantOld       bool
	}{
		{name: "identity_preserves_dotted_new_and_old_folders", wantOld: true},
		{name: "bounded_regex_rewrites_library", rewrite: "bounded", wantOld: true},
		{name: "bounded_regex_preserves_sibling", rewrite: "bounded", sibling: true, wantOld: true},
		{name: "loose_regex_matches_sibling", rewrite: "loose", sibling: true, wantOld: true},
		{name: "capture_regex_preserves_media_folder", rewrite: "capture", wantOld: true},
		{name: "filters_see_rewritten_new_and_old_folders", rewrite: "bounded", extraFolders: true, includeMapped: true, exclude: "/Extras/", wantOld: true},
		{name: "exclude_old_show_folder", rewrite: "bounded", exclude: "/TV/"},
		{name: "new_and_old_same_movie_are_coalesced", rewrite: "bounded", duplicateOld: true, wantOld: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// The metadata describes an unmounted/deleted remote library. Its
			// directories are intentionally absent from the local filesystem.
			root := filepath.Join(t.TempDir(), "media")
			sourceRoot := root
			if tt.sibling {
				sourceRoot += "2"
			}
			movie := sourceRoot + "/Movies/Dr.Strangelove.1964"
			show := sourceRoot + "/TV/Mr.Robot/Season.01"
			paths := &Paths{NewFolders: []string{movie}, OldFolders: []string{show}}
			if tt.extraFolders {
				paths.NewFolders = append(paths.NewFolders, sourceRoot+"/Movies/Extras/Bonus.1980")
				paths.OldFolders = append(paths.OldFolders, sourceRoot+"/TV/Extras/Deleted.Show")
			}
			if tt.duplicateOld {
				paths.OldFolders = append(paths.OldFolders, movie)
			}
			for _, folder := range append(append([]string(nil), paths.NewFolders...), paths.OldFolders...) {
				if _, err := os.Stat(folder); !os.IsNotExist(err) {
					t.Fatalf("remote fixture path %q should not exist locally: %v", folder, err)
				}
			}
			var rules []autoscan.Rewrite
			mappedRoot := sourceRoot
			switch tt.rewrite {
			case "bounded":
				rules = []autoscan.Rewrite{{From: "^" + regexp.QuoteMeta(root) + "/", To: "/mnt/unionfs/Media/"}}
				if !tt.sibling {
					mappedRoot = "/mnt/unionfs/Media"
				}
			case "loose":
				rules = []autoscan.Rewrite{{From: "^" + regexp.QuoteMeta(root), To: "/mnt/unionfs/Media"}}
				mappedRoot = "/mnt/unionfs/Media2"
			case "capture":
				rules = []autoscan.Rewrite{{From: "^" + regexp.QuoteMeta(root) + "/(.*)$", To: "/mnt/unionfs/Media/$1"}}
				mappedRoot = "/mnt/unionfs/Media"
			}
			rewriter, err := autoscan.NewRewriter(rules)
			if err != nil {
				t.Fatal(err)
			}
			var includes, excludes []string
			if tt.includeMapped {
				includes = []string{"^/mnt/unionfs/Media/"}
			}
			if tt.exclude != "" {
				excludes = []string{tt.exclude}
			}
			allowed, err := autoscan.NewFilterer(includes, excludes)
			if err != nil {
				t.Fatal(err)
			}
			scanTime := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
			d := daemon{priority: 7}
			task := d.getScanTask(&drive{
				ID:       "remote-media-drive",
				Rewriter: rewriter,
				Allowed:  allowed,
				ScanTime: func() time.Time { return scanTime },
			}, paths)
			want := []autoscan.Scan{{Folder: mappedRoot + "/Movies/Dr.Strangelove.1964", Priority: 7, Time: scanTime}}
			wantRemoved := 0
			if tt.wantOld {
				want = append(want, autoscan.Scan{Folder: mappedRoot + "/TV/Mr.Robot/Season.01", Priority: 7, Time: scanTime})
				wantRemoved = 1
			}
			if !slices.Equal(task.scans, want) {
				t.Errorf("metadata scans = %+v, want %+v", task.scans, want)
			}
			if task.added != 1 || task.removed != wantRemoved {
				t.Errorf("scan counters = added %d, removed %d; want added 1, removed %d", task.added, task.removed, wantRemoved)
			}
		})
	}
}
