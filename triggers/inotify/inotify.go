package inotify

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/rs/zerolog"

	"github.com/saltydk/autoscan"
)

type Config struct {
	Priority  int                `yaml:"priority"`
	Verbosity string             `yaml:"verbosity"`
	Rewrite   []autoscan.Rewrite `yaml:"rewrite"`
	Include   []string           `yaml:"include"`
	Exclude   []string           `yaml:"exclude"`
	Paths     []struct {
		Path    string             `yaml:"path"`
		Rewrite []autoscan.Rewrite `yaml:"rewrite"`
		Include []string           `yaml:"include"`
		Exclude []string           `yaml:"exclude"`
	} `yaml:"paths"`
}

type daemon struct {
	callback           autoscan.ProcessorFunc
	paths              []path
	watcher            *fsnotify.Watcher
	queue              *queue
	log                zerolog.Logger
	directories        map[string]struct{}
	retiredDirectories map[string]time.Time
}

type path struct {
	Path     string
	Rewriter autoscan.Rewriter
	Allowed  autoscan.Filterer
}

func New(c Config) (autoscan.Trigger, error) {
	l := autoscan.GetLogger(c.Verbosity).With().
		Str("trigger", "inotify").
		Logger()

	var paths []path
	for _, p := range c.Paths {
		rewriter, err := autoscan.NewRewriter(append(p.Rewrite, c.Rewrite...))
		if err != nil {
			return nil, err
		}

		filterer, err := autoscan.NewFilterer(append(p.Include, c.Include...), append(p.Exclude, c.Exclude...))
		if err != nil {
			return nil, err
		}

		paths = append(paths, path{
			Path:     p.Path,
			Rewriter: rewriter,
			Allowed:  filterer,
		})
	}

	trigger := func(callback autoscan.ProcessorFunc) {
		d := daemon{
			log:      l,
			callback: callback,
			paths:    paths,
			queue:    newQueue(callback, l, c.Priority),
		}

		// start job(s)
		if err := d.startMonitoring(); err != nil {
			d.queue.stop()
			l.Error().
				Err(err).
				Msg("Failed initialising jobs")
			return
		}
	}

	return trigger, nil
}

func (d *daemon) startMonitoring() error {
	// create watcher
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create watcher: %w", err)
	}
	d.watcher = watcher

	// setup watcher
	for _, p := range d.paths {
		if err := filepath.Walk(p.Path, d.walkFunc); err != nil {
			_ = d.watcher.Close()
			return err
		}
	}

	// start worker
	go d.worker()

	return nil
}

func (d *daemon) walkFunc(path string, fi os.FileInfo, err error) error {
	// handle error
	if err != nil {
		return fmt.Errorf("walk func: %v: %w", path, err)
	}

	// ignore non-directory
	if !fi.Mode().IsDir() {
		return nil
	}

	if err := d.watcher.Add(path); err != nil {
		return fmt.Errorf("watch directory: %v: %w", path, err)
	}
	d.rememberDirectory(path)

	d.log.Trace().
		Str("path", path).
		Msg("Watching directory")

	return nil
}

func (d *daemon) getPathObject(path string) (*path, error) {
	for _, p := range d.paths {
		if withinDirectory(path, p.Path) {
			return &p, nil
		}
	}

	return nil, fmt.Errorf("path object not found: %v", path)
}

func (d *daemon) worker() {
	// close watcher
	defer d.queue.stop()
	defer d.watcher.Close()

	// process events
	for {
		select {
		case event, open := <-d.watcher.Events:
			if !open {
				return
			}
			// new filesystem event
			d.log.Trace().
				Interface("event", event).
				Msg("Filesystem event")

			if err := d.handleEvent(event); err != nil {
				d.log.Error().
					Err(err).
					Str("path", event.Name).
					Msg("Failed processing filesystem event")
			}

		case err, open := <-d.watcher.Errors:
			if !open {
				return
			}
			d.log.Error().
				Err(err).
				Msg("Failed receiving filesystem events")
		}
	}
}

type queue struct {
	callback autoscan.ProcessorFunc
	log      zerolog.Logger
	priority int
	inputs   chan string
	scans    map[string]time.Time
	lock     *sync.Mutex
	shutdown chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

const (
	queueDebounceDelay = 10 * time.Second
	queueRetryDelay    = 5 * time.Second
)

func newQueue(cb autoscan.ProcessorFunc, log zerolog.Logger, priority int) *queue {
	q := &queue{
		callback: cb,
		log:      log,
		priority: priority,
		inputs:   make(chan string),
		scans:    make(map[string]time.Time),
		lock:     &sync.Mutex{},
		shutdown: make(chan struct{}),
		done:     make(chan struct{}),
	}

	go q.worker()

	return q
}

func (q *queue) add(path string) {
	// acquire lock
	q.lock.Lock()
	defer q.lock.Unlock()

	// queue scan task
	q.scans[path] = time.Now().Add(queueDebounceDelay)
}

func (q *queue) worker() {
	if q.done != nil {
		defer close(q.done)
	}
	timer := time.NewTimer(0)
	timer.Stop()
	defer timer.Stop()

	for {
		q.lock.Lock()
		var next time.Time
		for _, deadline := range q.scans {
			if next.IsZero() || deadline.Before(next) {
				next = deadline
			}
		}
		q.lock.Unlock()

		var ready <-chan time.Time
		if next.IsZero() {
			timer.Stop()
		} else {
			timer.Reset(time.Until(next))
			ready = timer.C
		}

		select {
		case <-q.shutdown:
			return
		case path, ok := <-q.inputs:
			if !ok {
				// channel closed
				return
			}

			// add path to queue
			q.add(path)

		case <-ready:
			// process queue
			q.process()
		}
	}
}

func (q *queue) stop() {
	// Only queues created by newQueue own a worker and a shutdown signal.
	if q == nil || q.shutdown == nil {
		return
	}
	q.stopOnce.Do(func() { close(q.shutdown) })
	<-q.done
}

func withinDirectory(name, root string) bool {
	name, root = filepath.Clean(name), filepath.Clean(root)
	if name == root {
		return true
	}
	return strings.HasPrefix(name, strings.TrimRight(root, string(os.PathSeparator))+string(os.PathSeparator))
}

func (q *queue) process() {
	// acquire lock
	q.lock.Lock()
	defer q.lock.Unlock()

	// move scans to processor
	for p, t := range q.scans {
		// time has not elapsed
		if time.Now().Before(t) {
			continue
		}

		// move to processor
		err := q.callback(autoscan.Scan{
			Folder:   filepath.Clean(p),
			Priority: q.priority,
			Time:     time.Now(),
		})

		if err != nil {
			q.log.Error().
				Err(err).
				Str("path", p).
				Msg("Failed moving scan to processor")
			q.scans[p] = time.Now().Add(queueRetryDelay)
			continue
		}

		q.log.Info().
			Str("path", p).
			Msg("Scan moved to processor")

		// remove queued scan
		delete(q.scans, p)
	}
}
