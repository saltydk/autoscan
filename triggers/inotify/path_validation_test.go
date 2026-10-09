package inotify

import (
	"testing"

	"github.com/saltydk/autoscan"
)

func TestRewritesCannotCreateInvalidInotifyFolders(t *testing.T) {
	for _, output := range []string{"", ".", "relative/episode.mkv", "/media/Bad\x00Path"} {
		t.Run(output, func(t *testing.T) {
			rewriter, err := autoscan.NewRewriter([]autoscan.Rewrite{{From: "^.*$", To: output}})
			if err != nil {
				t.Fatal(err)
			}
			d := daemon{paths: []path{{Path: "/media", Rewriter: rewriter, Allowed: func(string) bool { return true }}}}
			for _, directory := range []bool{false, true} {
				folder, allowed, err := d.filteredFolder("/media/Movies/Film.2026/video.mkv", directory)
				if err != nil || allowed || folder != "" {
					t.Fatalf("directory=%t: invalid rewrite folder=%q allowed=%t error=%v", directory, folder, allowed, err)
				}
			}
		})
	}
}
