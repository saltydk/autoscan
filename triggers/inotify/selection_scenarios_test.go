package inotify

import "testing"

func TestInotifySelectionScenarioDirectoryBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name  string
		root  string
		event string
		match bool
	}{
		{"root_itself", "/media", "/media", true},
		{"descendant_file", "/media", "/media/Show/Season 01/episode.mkv", true},
		{"numeric_prefix_sibling", "/media", "/media2/episode.mkv", false},
		{"punctuated_prefix_sibling", "/media", "/media-backup/episode.mkv", false},
		{"word_prefix_sibling", "/media", "/medialibrary/episode.mkv", false},
		{"unrelated_absolute_path", "/media", "/downloads/episode.mkv", false},
		{"case_sensitive_root", "/media", "/Media/episode.mkv", false},
		{"case_sensitive_child_root", "/media/TV", "/media/tv/episode.mkv", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := daemon{paths: []path{{Path: tt.root}}}
			selected, err := d.getPathObject(tt.event)
			if !tt.match {
				if err == nil {
					t.Fatalf("event %q incorrectly selected watched root %q", tt.event, selected.Path)
				}
				return
			}
			if err != nil {
				t.Fatalf("event %q did not select watched root %q: %v", tt.event, tt.root, err)
			}
			if selected.Path != tt.root {
				t.Errorf("selected root = %q, want %q", selected.Path, tt.root)
			}
		})
	}
}

func TestInotifySelectionScenarioPrefixSiblingRoots(t *testing.T) {
	for _, tt := range []struct {
		name  string
		roots []string
		event string
		want  string
	}{
		{"numeric_broad_first", []string{"/media", "/media2"}, "/media2/Show/episode.mkv", "/media2"},
		{"numeric_broad_second", []string{"/media2", "/media"}, "/media2/Show/episode.mkv", "/media2"},
		{"punctuated_broad_first", []string{"/media", "/media-backup"}, "/media-backup/Show/episode.mkv", "/media-backup"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := daemon{}
			for _, root := range tt.roots {
				d.paths = append(d.paths, path{Path: root})
			}
			selected, err := d.getPathObject(tt.event)
			if err != nil {
				t.Fatal(err)
			}
			if selected.Path != tt.want {
				t.Fatalf("event %q selected sibling %q, want its own watched root %q", tt.event, selected.Path, tt.want)
			}
		})
	}
}

// Overlapping roots currently use the first matching configured entry. These
// scenarios characterize that ordering and do not require longest-root matching.
func TestInotifySelectionScenarioConfiguredOrder(t *testing.T) {
	for _, tt := range []struct {
		name  string
		roots []string
		event string
		want  string
	}{
		{"broad_before_specific", []string{"/media", "/media/TV"}, "/media/TV/Show/episode.mkv", "/media"},
		{"specific_before_broad", []string{"/media/TV", "/media"}, "/media/TV/Show/episode.mkv", "/media/TV"},
		{"broad_before_exact_specific_root", []string{"/media", "/media/TV"}, "/media/TV", "/media"},
		{"broad_middle_deep", []string{"/media", "/media/TV", "/media/TV/Shows"}, "/media/TV/Shows/Show/episode.mkv", "/media"},
		{"middle_deep_broad", []string{"/media/TV", "/media/TV/Shows", "/media"}, "/media/TV/Shows/Show/episode.mkv", "/media/TV"},
		{"deep_broad_middle", []string{"/media/TV/Shows", "/media", "/media/TV"}, "/media/TV/Shows/Show/episode.mkv", "/media/TV/Shows"},
		{"different_case_specific_root_falls_back_to_parent", []string{"/media", "/media/TV"}, "/media/tv/Show/episode.mkv", "/media"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := daemon{}
			for _, root := range tt.roots {
				d.paths = append(d.paths, path{Path: root})
			}
			selected, err := d.getPathObject(tt.event)
			if err != nil {
				t.Fatal(err)
			}
			if selected.Path != tt.want {
				t.Fatalf("configured-order selection: event %q selected %q, want %q", tt.event, selected.Path, tt.want)
			}
		})
	}
}
