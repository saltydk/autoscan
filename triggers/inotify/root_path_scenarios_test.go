package inotify

import (
	"path/filepath"
	"runtime"
	"testing"
)

// Media libraries are configured using absolute paths.
func TestInotifyConfiguredRootFilesystemScenarios(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("this scenario suite validates the Linux inotify backend")
	}
	for _, mode := range []string{"absolute", "absolute_trailing_separator"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			if !runInotifyScenario(t) {
				return
			}
			base := t.TempDir()
			root := filepath.Join(base, "media")
			movie := filepath.Join(root, "Movie")
			inotifyScenarioMakeDir(t, movie)
			configured, want := root, movie
			switch mode {
			case "absolute_trailing_separator":
				configured += "/"
			}
			d := newInotifyScenarioDaemon(t, inotifyScenarioRoot(configured))
			inotifyScenarioWrite(t, filepath.Join(movie, "new.mkv"))
			assertInotifyScenarioFolders(t, collectInotifyScenarioFolders(d), []string{want})
		})
	}
}
