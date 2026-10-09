package bernard

import (
	"testing"
	"time"
)

func TestBernardSkipsInvalidInputAndRewrittenFolders(t *testing.T) {
	for _, rewritten := range []string{"", ".", "relative/film", "/media/Bad\x00Path"} {
		t.Run(rewritten, func(t *testing.T) {
			d := daemon{priority: 7}
			task := d.getScanTask(&drive{
				Rewriter: func(string) string { return rewritten },
				Allowed:  func(string) bool { return true },
				ScanTime: func() time.Time { return time.Unix(1, 0) },
			}, &Paths{NewFolders: []string{"/unmounted/Movies/New.2026"}, OldFolders: []string{"/unmounted/Movies/Old.2026"}})
			if len(task.scans) != 0 || task.added != 0 || task.removed != 0 {
				t.Fatalf("invalid rewritten folders produced task: %+v", task)
			}
		})
	}
	d := daemon{}
	task := d.getScanTask(&drive{
		Rewriter: func(string) string { return "/media/Movies/Rewrite.Heals.Invalid" },
		Allowed:  func(string) bool { return true },
		ScanTime: func() time.Time { return time.Unix(1, 0) },
	}, &Paths{NewFolders: []string{""}, OldFolders: []string{"."}})
	if len(task.scans) != 0 {
		t.Fatal("rewrite healed malformed source metadata into queued work")
	}
}
