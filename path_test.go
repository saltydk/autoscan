package autoscan

import "testing"

func TestValidScanPath(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		valid       bool
	}{
		{"empty", "", false},
		{"current directory", ".", false},
		{"relative parent", "..", false},
		{"relative path", "Movies/Movie.2026", false},
		{"blank", "  ", false},
		{"NUL", "/media/Movies/Bad\x00Path", false},
		{"movie folder", "/media/Movies/Movie.2026", true},
		{"media file", "/media/TV/Show/Season 01/episode.mkv", true},
		{"unmounted Unicode and spaces", "/unmounted/Movies/æøå Film (2026)", true},
		{"literal trailing space", "/media/Movies/Title ", true},
		{"root handled by targets", "/", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidScanPath(tc.value); got != tc.valid {
				t.Fatalf("ValidScanPath(%q) = %t, want %t", tc.value, got, tc.valid)
			}
		})
	}
}

func TestValidRelativeFilePath(t *testing.T) {
	for _, tc := range []struct {
		value string
		valid bool
	}{
		{"", false}, {" ", false}, {".", false}, {"..", false},
		{"/absolute/movie.mkv", false}, {"../movie.mkv", false},
		{"Season 01/../../movie.mkv", false}, {"Season 01/", false},
		{"Season 01/..", false}, {"episode.mkv\x00", false},
		{"episode.mkv", true}, {"Season 01/episode.mkv", true},
		{"Season 01/æøå Episode (2026).mkv", true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			if got := ValidRelativeFilePath(tc.value); got != tc.valid {
				t.Fatalf("ValidRelativeFilePath(%q) = %t, want %t", tc.value, got, tc.valid)
			}
		})
	}
}
