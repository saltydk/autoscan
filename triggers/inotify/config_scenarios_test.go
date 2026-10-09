package inotify

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/saltydk/autoscan"
)

type configScenarioPath = struct {
	Path    string             `yaml:"path"`
	Rewrite []autoscan.Rewrite `yaml:"rewrite"`
	Include []string           `yaml:"include"`
	Exclude []string           `yaml:"exclude"`
}

func TestInotifyConfigScenarioRegexValidation(t *testing.T) {
	for _, tt := range []struct {
		name      string
		configure func(*Config)
		wantError bool
	}{
		{"global_rewrite_invalid", func(c *Config) { c.Rewrite = []autoscan.Rewrite{{From: "[", To: "/mnt/unionfs/Media"}} }, true},
		{"local_rewrite_invalid", func(c *Config) { c.Paths[0].Rewrite = []autoscan.Rewrite{{From: "[", To: "/mnt/unionfs/Media"}} }, true},
		{"global_include_invalid", func(c *Config) { c.Include = []string{"["} }, true},
		{"local_include_invalid", func(c *Config) { c.Paths[0].Include = []string{"["} }, true},
		{"global_exclude_invalid", func(c *Config) { c.Exclude = []string{"["} }, true},
		{"local_exclude_invalid", func(c *Config) { c.Paths[0].Exclude = []string{"["} }, true},
		{"global_rewrite_valid", func(c *Config) { c.Rewrite = []autoscan.Rewrite{{From: `^/mnt/local/(.*)$`, To: "/mnt/unionfs/$1"}} }, false},
		{"local_rewrite_valid", func(c *Config) {
			c.Paths[0].Rewrite = []autoscan.Rewrite{{From: `^/mnt/local/(.*)$`, To: "/mnt/unionfs/$1"}}
		}, false},
		{"global_include_valid", func(c *Config) { c.Include = []string{`(?i)\.(mkv|mp4)$`} }, false},
		{"local_include_valid", func(c *Config) { c.Paths[0].Include = []string{`(?i)\.(mkv|mp4)$`} }, false},
		{"global_exclude_valid", func(c *Config) { c.Exclude = []string{`(^|/)extras(/|$)`} }, false},
		{"local_exclude_valid", func(c *Config) { c.Paths[0].Exclude = []string{`(^|/)extras(/|$)`} }, false},
		{"invalid_second_path_is_checked", func(c *Config) {
			c.Paths = append(c.Paths, configScenarioPath{Path: "/mnt/local/Media2", Include: []string{"["}})
		}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := Config{Paths: []configScenarioPath{{Path: "/mnt/local/Media"}}}
			tt.configure(&c)
			trigger, err := New(configScenarioYAML(t, c))
			if (err != nil) != tt.wantError {
				t.Fatalf("New() error = %v, want error %t", err, tt.wantError)
			}
			if tt.wantError && trigger != nil {
				t.Error("invalid configuration returned a usable trigger")
			}
			if !tt.wantError && trigger == nil {
				t.Error("valid configuration returned a nil trigger")
			}
		})
	}
}

// Include combinations characterize the existing union of path and global
// patterns. They do not require path patterns to replace global patterns.
func TestInotifyConfigScenarioPublicRewriteAndFilterWiring(t *testing.T) {
	for _, tt := range []struct {
		name      string
		configure func(string, *Config)
		files     []string
		want      []string
	}{
		{
			name: "global_rewrite", configure: func(string, *Config) {},
			files: []string{"Show/episode.mkv"}, want: []string{"/merged/Show"},
		},
		{
			name: "local_rewrite", configure: func(root string, c *Config) {
				c.Rewrite = nil
				c.Paths[0].Rewrite = configScenarioRewrite(root, "/specific")
			},
			files: []string{"Show/episode.mkv"}, want: []string{"/specific/Show"},
		},
		{
			name: "local_rewrite_before_global", configure: func(root string, c *Config) {
				c.Paths[0].Rewrite = configScenarioRewrite(root, "/specific")
			},
			files: []string{"Show/episode.mkv"}, want: []string{"/specific/Show"},
		},
		{
			name: "global_rewrite_when_local_does_not_match", configure: func(root string, c *Config) {
				c.Paths[0].Rewrite = configScenarioRewrite(filepath.Join(root, "Special"), "/specific")
			},
			files: []string{"Show/episode.mkv"}, want: []string{"/merged/Show"},
		},
		{
			name: "first_local_rewrite_wins", configure: func(root string, c *Config) {
				c.Paths[0].Rewrite = append(configScenarioRewrite(root, "/first"), configScenarioRewrite(root, "/second")...)
			},
			files: []string{"Show/episode.mkv"}, want: []string{"/first/Show"},
		},
		{
			name: "first_global_rewrite_wins", configure: func(root string, c *Config) {
				c.Rewrite = append(configScenarioRewrite(root, "/first"), configScenarioRewrite(root, "/second")...)
			},
			files: []string{"Show/episode.mkv"}, want: []string{"/first/Show"},
		},
		{
			name: "rewrites_are_not_chained", configure: func(root string, c *Config) {
				c.Paths[0].Rewrite = configScenarioRewrite(root, "/intermediate")
				c.Rewrite = configScenarioRewrite("/intermediate", "/final")
			},
			files: []string{"Show/episode.mkv"}, want: []string{"/intermediate/Show"},
		},
		{
			name: "rewrite_capture_group", configure: func(root string, c *Config) {
				c.Rewrite = []autoscan.Rewrite{{From: "^" + regexp.QuoteMeta(root) + "/(.*)$", To: "/captured/$1"}}
			},
			files: []string{"Show/Season 01/episode.mkv"}, want: []string{"/captured/Show/Season 01"},
		},
		{
			name: "global_include_filters_files", configure: func(_ string, c *Config) { c.Include = []string{`\.mkv$`} },
			files: []string{"Blocked/metadata.nfo", "Allowed/episode.mkv"}, want: []string{"/merged/Allowed"},
		},
		{
			name: "local_include_filters_files", configure: func(_ string, c *Config) { c.Paths[0].Include = []string{`\.mp4$`} },
			files: []string{"Blocked/metadata.nfo", "Allowed/episode.mp4"}, want: []string{"/merged/Allowed"},
		},
		{
			name: "local_and_global_includes_are_unioned", configure: func(_ string, c *Config) {
				c.Include = []string{`\.mkv$`}
				c.Paths[0].Include = []string{`\.mp4$`}
			},
			files: []string{"Blocked/metadata.nfo", "Local/episode.mp4", "Global/episode.mkv"},
			want:  []string{"/merged/Local", "/merged/Global"},
		},
		{
			name: "global_exclude_filters_files", configure: func(_ string, c *Config) { c.Exclude = []string{`/Blocked/`} },
			files: []string{"Blocked/episode.mkv", "Allowed/episode.mkv"}, want: []string{"/merged/Allowed"},
		},
		{
			name: "local_exclude_filters_files", configure: func(_ string, c *Config) { c.Paths[0].Exclude = []string{`/Blocked/`} },
			files: []string{"Blocked/episode.mkv", "Allowed/episode.mkv"}, want: []string{"/merged/Allowed"},
		},
		{
			name: "global_exclude_overrides_local_include", configure: func(_ string, c *Config) {
				c.Exclude = []string{`/Blocked/`}
				c.Paths[0].Include = []string{`\.mkv$`}
			},
			files: []string{"Blocked/episode.mkv", "Allowed/episode.mkv"}, want: []string{"/merged/Allowed"},
		},
		{
			name: "local_exclude_overrides_global_include", configure: func(_ string, c *Config) {
				c.Include = []string{`\.mkv$`}
				c.Paths[0].Exclude = []string{`/Blocked/`}
			},
			files: []string{"Blocked/episode.mkv", "Allowed/episode.mkv"}, want: []string{"/merged/Allowed"},
		},
		{
			name: "local_and_global_excludes_are_unioned", configure: func(_ string, c *Config) {
				c.Exclude = []string{`/GlobalBlocked/`}
				c.Paths[0].Exclude = []string{`/LocalBlocked/`}
			},
			files: []string{"GlobalBlocked/episode.mkv", "LocalBlocked/episode.mkv", "Allowed/episode.mkv"},
			want:  []string{"/merged/Allowed"},
		},
		{
			name: "global_filter_sees_rewritten_path", configure: func(_ string, c *Config) { c.Include = []string{`^/merged/Allowed/`} },
			files: []string{"Blocked/episode.mkv", "Allowed/episode.mkv"}, want: []string{"/merged/Allowed"},
		},
		{
			name: "local_filter_sees_rewritten_path", configure: func(_ string, c *Config) { c.Paths[0].Include = []string{`^/merged/Allowed/`} },
			files: []string{"Blocked/episode.mkv", "Allowed/episode.mkv"}, want: []string{"/merged/Allowed"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if !runInotifyScenario(t) {
				return
			}
			root := filepath.Join(t.TempDir(), "Media")
			c := Config{Priority: 7, Rewrite: configScenarioRewrite(root, "/merged"), Paths: []configScenarioPath{{Path: root}}}
			tt.configure(root, &c)
			for _, file := range tt.files {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(root, file)), 0700); err != nil {
					t.Fatal(err)
				}
			}
			recorder := startPublicInotifyScenario(t, configScenarioYAML(t, c))
			for _, file := range tt.files {
				if err := os.WriteFile(filepath.Join(root, file), []byte("scenario"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			// A full debounce window also gives excluded events an opportunity to
			// reveal an erroneous callback, rather than checking immediately.
			observation := time.NewTimer(queueDebounceDelay + time.Second)
			defer observation.Stop()
			awaitInotifyScenario(t, "allowed scan callbacks", func() bool {
				return len(recorder.folders()) >= len(tt.want)
			})
			<-observation.C
			assertInotifyScenarioFolders(t, recorder.folders(), tt.want)
			for _, scan := range recorder.snapshot() {
				if scan.Priority != c.Priority {
					t.Errorf("scan priority = %d, want %d", scan.Priority, c.Priority)
				}
			}
		})
	}
}

func TestInotifyConfigScenarioPublicSiblingRootsKeepSeparateSettings(t *testing.T) {
	for _, tt := range []struct {
		name    string
		first   string
		second  string
		reverse bool
	}{
		{"independent_roots", "first", "second", false},
		{"prefix_siblings_broad_first", "media", "media2", false},
		{"prefix_siblings_broad_second", "media", "media2", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if !runInotifyScenario(t) {
				return
			}
			base := t.TempDir()
			first := filepath.Join(base, tt.first)
			second := filepath.Join(base, tt.second)
			c := Config{
				Rewrite: configScenarioRewrite(base, "/global"),
				Exclude: []string{`/GlobalBlocked/`},
				Paths: []configScenarioPath{
					{Path: first, Rewrite: configScenarioRewrite(first, "/first"), Include: []string{`\.mkv$`}},
					{Path: second, Rewrite: configScenarioRewrite(second, "/second"), Include: []string{`\.mp4$`}},
				},
			}
			if tt.reverse {
				c.Paths[0], c.Paths[1] = c.Paths[1], c.Paths[0]
			}
			files := []string{
				filepath.Join(first, "Allowed", "episode.mkv"),
				filepath.Join(first, "WrongExtension", "episode.mp4"),
				filepath.Join(first, "GlobalBlocked", "episode.mkv"),
				filepath.Join(second, "Allowed", "episode.mp4"),
				filepath.Join(second, "WrongExtension", "episode.mkv"),
				filepath.Join(second, "GlobalBlocked", "episode.mp4"),
			}
			for _, file := range files {
				if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
					t.Fatal(err)
				}
			}
			recorder := startPublicInotifyScenario(t, configScenarioYAML(t, c))
			for _, file := range files {
				if err := os.WriteFile(file, []byte("scenario"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			observation := time.NewTimer(queueDebounceDelay + time.Second)
			defer observation.Stop()
			awaitInotifyScenario(t, "separate root callbacks", func() bool { return len(recorder.folders()) >= 2 })
			<-observation.C
			assertInotifyScenarioFolders(t, recorder.folders(), []string{"/first/Allowed", "/second/Allowed"})
		})
	}
}

func configScenarioRewrite(root, destination string) []autoscan.Rewrite {
	return []autoscan.Rewrite{{From: "^" + regexp.QuoteMeta(root), To: destination}}
}

// Public scenarios decode config.yml-shaped YAML rather than relying on Go-only
// slice aliasing. Temporary watched libraries still use full absolute paths.
func configScenarioYAML(t *testing.T, c Config) Config {
	t.Helper()
	data, err := yaml.Marshal(c)
	if err != nil {
		t.Fatalf("encode media library configuration: %v", err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var decoded Config
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatalf("decode media library configuration: %v", err)
	}
	return decoded
}
