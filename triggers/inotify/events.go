package inotify

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	directoryHistoryTTL   = time.Minute
	directoryHistoryLimit = 4096
)

func (d *daemon) handleEvent(event fsnotify.Event) error {
	if !event.Has(fsnotify.Create | fsnotify.Rename | fsnotify.Remove) {
		return nil
	}
	name := filepath.Clean(event.Name)
	d.trimDirectoryHistory(name)
	switch {
	case event.Has(fsnotify.Create):
		delete(d.retiredDirectories, name)
		info, err := os.Stat(name)
		if err != nil {
			return fmt.Errorf("stat new path: %w", err)
		}
		if info.IsDir() {
			return d.watchCreatedDirectory(name)
		}
		delete(d.directories, name)
		return d.submitEvent(name, false)

	case event.Has(fsnotify.Rename), event.Has(fsnotify.Remove):
		if _, known := d.directories[name]; known {
			d.retireDirectory(name)
			return d.submitEvent(name, true)
		}
		if _, duplicate := d.retiredDirectories[name]; duplicate {
			return nil
		}
		return d.submitEvent(name, false)
	default:
		return nil
	}
}

func (d *daemon) watchCreatedDirectory(name string) error {
	if err := filepath.Walk(name, d.walkFunc); err != nil {
		return fmt.Errorf("watch new directory: %w", err)
	}
	return nil
}

func (d *daemon) submitEvent(name string, directory bool) error {
	folder, allowed, err := d.filteredFolder(name, directory)
	if err != nil || !allowed {
		return err
	}
	d.queue.inputs <- folder
	return nil
}

func (d *daemon) filteredFolder(name string, directory bool) (string, bool, error) {
	p, err := d.getPathObject(name)
	if err != nil {
		return "", false, err
	}
	rewritten := p.Rewriter(name)
	if !p.Allowed(rewritten) {
		return "", false, nil
	}
	if !directory {
		rewritten = filepath.Dir(rewritten)
	}
	return rewritten, true, nil
}

func (d *daemon) rememberDirectory(name string) {
	if d.directories == nil {
		d.directories = make(map[string]struct{})
	}
	name = filepath.Clean(name)
	d.directories[name] = struct{}{}
	delete(d.retiredDirectories, name)
}

func (d *daemon) retireDirectory(root string) {
	if d.retiredDirectories == nil {
		d.retiredDirectories = make(map[string]time.Time)
	}
	deadline := time.Now().Add(directoryHistoryTTL)
	for name := range d.directories {
		if !withinDirectory(name, root) {
			continue
		}
		delete(d.directories, name)
		d.retiredDirectories[name] = deadline
		if err := d.watcher.Remove(name); err != nil && !errors.Is(err, fsnotify.ErrNonExistentWatch) && !errors.Is(err, fsnotify.ErrClosed) {
			d.log.Error().Err(err).Str("path", name).Msg("Failed removing old directory watch")
		}
	}
	d.trimDirectoryHistory(root)
}

func (d *daemon) trimDirectoryHistory(keep string) {
	now := time.Now()
	for name, deadline := range d.retiredDirectories {
		if !now.Before(deadline) {
			delete(d.retiredDirectories, name)
		}
	}
	if len(d.retiredDirectories) <= directoryHistoryLimit {
		return
	}
	type historyEntry struct {
		name    string
		expires time.Time
	}
	entries := make([]historyEntry, 0, len(d.retiredDirectories))
	for name, expires := range d.retiredDirectories {
		if name != keep {
			entries = append(entries, historyEntry{name, expires})
		}
	}
	slices.SortFunc(entries, func(a, b historyEntry) int { return a.expires.Compare(b.expires) })
	for _, entry := range entries[:len(d.retiredDirectories)-directoryHistoryLimit] {
		delete(d.retiredDirectories, entry.name)
	}
}
