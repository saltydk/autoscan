package inotify

import (
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestInotifyMediaWorkflowScenarios(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("these scenarios validate the Linux inotify backend")
	}
	for _, mode := range []string{
		"downloader_finalizes_mkv_part",
		"video_subtitle_and_nfo_coalesce",
		"video_only_filter_excludes_sidecar_only_folder",
		"flac_and_mp3_album_tracks_coalesce",
	} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			if !runInotifyScenario(t) {
				return
			}
			root := filepath.Join(t.TempDir(), "media")
			movie := filepath.Join(root, "Movies", "Example Movie (2026)")
			other := filepath.Join(root, "Movies", "Another Movie (2026)")
			album := filepath.Join(root, "Music", "Example Artist", "Example Album (2026)")
			for _, directory := range []string{movie, other, album} {
				inotifyScenarioMakeDir(t, directory)
			}
			c := Config{Priority: 4, Paths: []configScenarioPath{{Path: root}}}
			switch mode {
			case "downloader_finalizes_mkv_part", "video_only_filter_excludes_sidecar_only_folder":
				c.Include = []string{`\.(mkv|mp4)$`}
			case "flac_and_mp3_album_tracks_coalesce":
				c.Include = []string{`\.(flac|mp3)$`}
			}
			recorder := startPublicInotifyScenario(t, c)
			want := movie
			switch mode {
			case "downloader_finalizes_mkv_part":
				partial := filepath.Join(movie, "Example Movie (2026).mkv.part")
				inotifyScenarioWrite(t, partial)
				inotifyScenarioRename(t, partial, filepath.Join(movie, "Example Movie (2026).mkv"))
			case "video_subtitle_and_nfo_coalesce":
				for _, extension := range []string{"mp4", "en.srt", "nfo"} {
					inotifyScenarioWrite(t, filepath.Join(movie, "Example Movie (2026)."+extension))
				}
			case "video_only_filter_excludes_sidecar_only_folder":
				inotifyScenarioWrite(t, filepath.Join(movie, "Example Movie (2026).mkv"))
				inotifyScenarioWrite(t, filepath.Join(other, "Another Movie (2026).en.srt"))
				inotifyScenarioWrite(t, filepath.Join(other, "Another Movie (2026).nfo"))
			case "flac_and_mp3_album_tracks_coalesce":
				want = album
				for track := range 10 {
					extension := "flac"
					if track%2 == 1 {
						extension = "mp3"
					}
					inotifyScenarioWrite(t, filepath.Join(album, fmt.Sprintf("%02d - Example Track.%s", track+1, extension)))
				}
			}
			time.Sleep(queueDebounceDelay + time.Second)
			assertInotifyScenarioFolders(t, recorder.folders(), []string{want})
			scans := recorder.snapshot()
			if len(scans) != 1 {
				t.Fatalf("media workflow submitted %d scans, want one folder scan: %+v", len(scans), scans)
			}
			if scans[0].Priority != 4 || scans[0].Time.IsZero() {
				t.Errorf("media workflow lost scan metadata: %+v", scans[0])
			}
		})
	}
}

func TestInotifyNewSeasonEpisodeBurstDelivery(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("this scenario validates the Linux inotify backend")
	}
	t.Parallel()
	if !runInotifyScenario(t) {
		return
	}
	root := filepath.Join(t.TempDir(), "media", "TV")
	show := filepath.Join(root, "Example Show (2026)")
	season := filepath.Join(show, "Season 02")
	inotifyScenarioMakeDir(t, show)
	recorder := &inotifyScenarioRecorder{}
	d := &daemon{
		paths: []path{inotifyScenarioRoot(root)},
		queue: newQueue(recorder.callback, zerolog.Nop(), 4),
		log:   zerolog.Nop(),
	}
	if err := d.startMonitoring(); err != nil {
		t.Fatal(err)
	}
	inotifyScenarioMakeDir(t, season)
	inotifyScenarioWaitForWatch(t, d, season)
	for episode := range 12 {
		inotifyScenarioWrite(t, filepath.Join(season, fmt.Sprintf("Example Show - S02E%02d.mkv", episode+1)))
	}
	time.Sleep(queueDebounceDelay + time.Second)
	assertInotifyScenarioFolders(t, recorder.folders(), []string{season})
	if scans := recorder.snapshot(); len(scans) != 1 {
		t.Fatalf("new season submitted %d scans, want one: %+v", len(scans), scans)
	}
}
