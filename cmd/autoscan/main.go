package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/alecthomas/kong"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/saltydk/autoscan"
	"github.com/saltydk/autoscan/migrate"
	"github.com/saltydk/autoscan/processor"
	ast "github.com/saltydk/autoscan/targets/autoscan"
	"github.com/saltydk/autoscan/targets/emby"
	"github.com/saltydk/autoscan/targets/jellyfin"
	"github.com/saltydk/autoscan/targets/plex"
	"github.com/saltydk/autoscan/triggers/a_train"
	"github.com/saltydk/autoscan/triggers/bernard"
	"github.com/saltydk/autoscan/triggers/inotify"
	"github.com/saltydk/autoscan/triggers/lidarr"
	"github.com/saltydk/autoscan/triggers/manual"
	"github.com/saltydk/autoscan/triggers/radarr"
	"github.com/saltydk/autoscan/triggers/readarr"
	"github.com/saltydk/autoscan/triggers/sonarr"

	// sqlite3 driver
	_ "modernc.org/sqlite"
)

type config struct {
	// General configuration
	Host       []string      `yaml:"host"`
	Port       int           `yaml:"port"`
	MinimumAge time.Duration `yaml:"minimum-age"`
	ScanDelay  time.Duration `yaml:"scan-delay"`
	ScanStats  time.Duration `yaml:"scan-stats"`
	Anchors    []string      `yaml:"anchors"`

	// Authentication for autoscan.HTTPTrigger
	Auth struct {
		Username string `yaml:"username"`
		Password string `yaml:"password"`
	} `yaml:"authentication"`

	// autoscan.HTTPTrigger
	Triggers struct {
		Manual  manual.Config    `yaml:"manual"`
		ATrain  a_train.Config   `yaml:"a-train"`
		Bernard []bernard.Config `yaml:"bernard"`
		Inotify []inotify.Config `yaml:"inotify"`
		Lidarr  []lidarr.Config  `yaml:"lidarr"`
		Radarr  []radarr.Config  `yaml:"radarr"`
		Readarr []readarr.Config `yaml:"readarr"`
		Sonarr  []sonarr.Config  `yaml:"sonarr"`
	} `yaml:"triggers"`

	// autoscan.Target
	Targets struct {
		Autoscan []ast.Config      `yaml:"autoscan"`
		Emby     []emby.Config     `yaml:"emby"`
		Jellyfin []jellyfin.Config `yaml:"jellyfin"`
		Plex     []plex.Config     `yaml:"plex"`
	} `yaml:"targets"`
}

var (
	// release variables
	Version   string
	Timestamp string
	GitCommit string

	// CLI
	cli struct {
		globals

		// flags
		Config    string `type:"path" default:"${config_file}" env:"AUTOSCAN_CONFIG" help:"Config file path"`
		Database  string `type:"path" default:"${database_file}" env:"AUTOSCAN_DATABASE" help:"Database file path"`
		Log       string `type:"path" default:"${log_file}" env:"AUTOSCAN_LOG" help:"Log file path"`
		Verbosity int    `type:"counter" default:"0" short:"v" env:"AUTOSCAN_VERBOSITY" help:"Log level verbosity"`
	}
)

type globals struct {
	Version versionFlag `name:"version" help:"Print version information and quit"`
}

type versionFlag string

func (v versionFlag) Decode(ctx *kong.DecodeContext) error { return nil }
func (v versionFlag) IsBool() bool                         { return true }
func (v versionFlag) BeforeApply(app *kong.Kong, vars kong.Vars) error {
	fmt.Println(vars["version"])
	app.Exit(0)
	return nil
}

func main() {
	// parse cli
	ctx := kong.Parse(&cli,
		kong.Name("autoscan"),
		kong.Description("Scan media into target media servers"),
		kong.UsageOnError(),
		kong.ConfigureHelp(kong.HelpOptions{
			Summary: true,
			Compact: true,
		}),
		kong.Vars{
			"version":       fmt.Sprintf("%s (%s@%s)", Version, GitCommit, Timestamp),
			"config_file":   filepath.Join(defaultConfigDirectory("autoscan", "config.yml"), "config.yml"),
			"log_file":      filepath.Join(defaultConfigDirectory("autoscan", "config.yml"), "activity.log"),
			"database_file": filepath.Join(defaultConfigDirectory("autoscan", "config.yml"), "autoscan.db"),
		},
	)

	if err := ctx.Validate(); err != nil {
		fmt.Println("Failed parsing cli:", err)
		os.Exit(1)
	}

	// logger
	logger := log.Output(io.MultiWriter(zerolog.ConsoleWriter{
		TimeFormat: time.Stamp,
		Out:        os.Stderr,
	}, zerolog.ConsoleWriter{
		TimeFormat: time.Stamp,
		Out: &lumberjack.Logger{
			Filename:   cli.Log,
			MaxSize:    5,
			MaxAge:     14,
			MaxBackups: 5,
		},
		NoColor: true,
	}))

	switch {
	case cli.Verbosity == 1:
		log.Logger = logger.Level(zerolog.DebugLevel)
	case cli.Verbosity > 1:
		log.Logger = logger.Level(zerolog.TraceLevel)
	default:
		log.Logger = logger.Level(zerolog.InfoLevel)
	}

	// datastore
	db, err := sql.Open("sqlite", cli.Database)
	if err != nil {
		log.Fatal().
			Err(err).
			Msg("Failed opening datastore")
	}
	db.SetMaxOpenConns(1)

	// config
	file, err := os.Open(cli.Config)
	if err != nil {
		log.Fatal().
			Err(err).
			Msg("Failed opening config")
	}
	defer file.Close()

	c, err := decodeConfig(file)
	if err != nil {
		log.Fatal().
			Err(err).
			Msg("Failed decoding config")
	}

	// migrator
	mg, err := migrate.New(db, "migrations")
	if err != nil {
		log.Fatal().
			Err(err).
			Msg("Failed initialising migrator")
	}

	// processor
	proc, err := processor.New(processor.Config{
		Anchors:    c.Anchors,
		MinimumAge: c.MinimumAge,
		Db:         db,
		Mg:         mg,
	})

	if err != nil {
		log.Fatal().
			Err(err).
			Msg("Failed initialising processor")
	}

	log.Info().
		Stringer("min_age", c.MinimumAge).
		Strs("anchors", c.Anchors).
		Msg("Initialised processor")

	// Check authentication. If no auth -> warn user.
	if c.Auth.Username == "" || c.Auth.Password == "" {
		log.Warn().Msg("Webhooks running without authentication")
	}

	// Configure durable queues before any trigger can submit work.
	targets, err := configureTargets(c, proc)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed initialising targets")
	}

	// daemon triggers
	for _, t := range c.Triggers.Bernard {
		trigger, err := bernard.New(t, db)
		if err != nil {
			log.Fatal().
				Err(err).
				Str("trigger", "bernard").
				Msg("Failed initialising trigger")
		}

		go trigger(proc.Add)
	}

	for _, t := range c.Triggers.Inotify {
		trigger, err := inotify.New(t)
		if err != nil {
			log.Fatal().
				Err(err).
				Str("trigger", "inotify").
				Msg("Failed initialising trigger")
		}

		go trigger(proc.Add)
	}

	// http triggers
	router := getRouter(c, proc)

	for _, h := range c.Host {
		go func(host string) {
			addr := host
			if !strings.Contains(addr, ":") {
				addr = fmt.Sprintf("%s:%d", host, c.Port)
			}

			log.Info().Msgf("Starting server on %s", addr)
			if err := http.ListenAndServe(addr, router); err != nil {
				log.Fatal().
					Str("addr", addr).
					Err(err).
					Msg("Failed starting web server")
			}
		}(h)
	}

	log.Info().
		Int("manual", 1).
		Int("bernard", len(c.Triggers.Bernard)).
		Int("inotify", len(c.Triggers.Inotify)).
		Int("lidarr", len(c.Triggers.Lidarr)).
		Int("radarr", len(c.Triggers.Radarr)).
		Int("readarr", len(c.Triggers.Readarr)).
		Int("sonarr", len(c.Triggers.Sonarr)).
		Msg("Initialised triggers")

	log.Info().
		Int("autoscan", len(c.Targets.Autoscan)).
		Int("plex", len(c.Targets.Plex)).
		Int("emby", len(c.Targets.Emby)).
		Int("jellyfin", len(c.Targets.Jellyfin)).
		Msg("Initialised targets")

	// scan stats
	if c.ScanStats.Seconds() > 0 {
		go scanStats(proc, c.ScanStats)
	}

	// display initialised banner
	log.Info().
		Str("version", fmt.Sprintf("%s (%s@%s)", Version, GitCommit, Timestamp)).
		Msg("Initialised")

	// processor
	log.Info().Msg("Processor started")

	if len(targets) == 0 {
		log.Warn().Msg("No targets initialised, processor stopped, triggers will continue...")
		select {}
	}
	var workers sync.WaitGroup
	for _, target := range targets {
		workers.Go(func() {
			if err := runTarget(context.Background(), proc, target, c.ScanDelay); err != nil {
				log.Error().Err(err).Str("target_queue", target.id).
					Msg("Target worker stopped, queued scans retained; other targets continue")
			}
		})
	}
	workers.Wait()
	// Triggers continue accepting work if every target has a permanent failure.
	select {}
}

func configureTargets(c config, proc *processor.Processor) ([]configuredTarget, error) {
	var targets []configuredTarget
	occurrences := make(map[string]int)
	appendTarget := func(kind, name, url string, rewrite []autoscan.Rewrite, target autoscan.Target) {
		id := targetQueueID(kind, name, url, rewrite)
		if name == "" {
			occurrences[id]++
			if occurrences[id] > 1 {
				id = fmt.Sprintf("%s#%d", id, occurrences[id])
			}
		}
		targets = append(targets, configuredTarget{id: id, target: target})
	}
	for _, t := range c.Targets.Autoscan {
		target, err := ast.New(t)
		if err != nil {
			return nil, fmt.Errorf("autoscan target: %w", err)
		}
		appendTarget("autoscan", t.Name, t.URL, t.Rewrite, target)
	}
	for _, t := range c.Targets.Plex {
		target, err := plex.New(t)
		if err != nil {
			return nil, fmt.Errorf("plex target: %w", err)
		}
		appendTarget("plex", t.Name, t.URL, t.Rewrite, target)
	}
	for _, t := range c.Targets.Emby {
		target, err := emby.New(t)
		if err != nil {
			return nil, fmt.Errorf("emby target: %w", err)
		}
		appendTarget("emby", t.Name, t.URL, t.Rewrite, target)
	}
	for _, t := range c.Targets.Jellyfin {
		target, err := jellyfin.New(t)
		if err != nil {
			return nil, fmt.Errorf("jellyfin target: %w", err)
		}
		appendTarget("jellyfin", t.Name, t.URL, t.Rewrite, target)
	}
	ids := make([]string, len(targets))
	for i, target := range targets {
		ids[i] = target.id
	}
	if err := proc.ConfigureTargets(ids); err != nil {
		return nil, fmt.Errorf("configure target queues (use distinct target names for duplicate URLs and rewrites): %w", err)
	}
	return targets, nil
}
