package processor

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/saltydk/autoscan"
	"github.com/saltydk/autoscan/migrate"

	"golang.org/x/sync/errgroup"
)

type Config struct {
	Anchors    []string
	MinimumAge time.Duration

	Db *sql.DB
	Mg *migrate.Migrator
}

func New(c Config) (*Processor, error) {
	store, err := newDatastore(c.Db, c.Mg)
	if err != nil {
		return nil, err
	}

	proc := &Processor{
		anchors:    c.Anchors,
		minimumAge: c.MinimumAge,
		store:      store,
	}
	return proc, nil
}

type Processor struct {
	anchors    []string
	minimumAge time.Duration
	store      *datastore
	processed  atomic.Int64
	targetMu   sync.RWMutex
	targetIDs  []string
}

func (p *Processor) Add(scans ...autoscan.Scan) error {
	p.targetMu.RLock()
	defer p.targetMu.RUnlock()
	if len(p.targetIDs) > 0 {
		return p.store.UpsertTargets(p.targetIDs, scans)
	}
	return p.store.Upsert(scans)
}

// ConfigureTargets assigns legacy work and enables independent durable queues.
// Call it before starting target workers. Removed targets retain dormant work.
func (p *Processor) ConfigureTargets(targetIDs []string) error {
	p.targetMu.Lock()
	defer p.targetMu.Unlock()
	seen := make(map[string]bool, len(targetIDs))
	for _, id := range targetIDs {
		if id == "" || seen[id] {
			return fmt.Errorf("target queue IDs must be nonempty and unique: %q", id)
		}
		seen[id] = true
	}
	if err := p.store.AssignLegacy(targetIDs); err != nil {
		return err
	}
	p.targetIDs = append([]string(nil), targetIDs...)
	return nil
}

// ScansRemaining returns the amount of scans remaining
func (p *Processor) ScansRemaining() (int, error) {
	p.targetMu.RLock()
	defer p.targetMu.RUnlock()
	return p.store.TargetScansRemaining(p.targetIDs)
}

// ScansProcessed returns the amount of scans processed
func (p *Processor) ScansProcessed() int64 {
	return p.processed.Load()
}

// CheckAvailability checks whether all targets are available.
// If one target is not available, the error will return.
func (p *Processor) CheckAvailability(targets []autoscan.Target) error {
	g := new(errgroup.Group)

	for _, target := range targets {
		g.Go(func() error {
			return target.Available()
		})
	}

	return g.Wait()
}

func (p *Processor) callTargets(targets []autoscan.Target, scan autoscan.Scan) (bool, error) {
	g := new(errgroup.Group)
	var delivered atomic.Bool
	for _, target := range targets {
		g.Go(func() error {
			err := target.Scan(scan)
			switch {
			case err == nil:
				delivered.Store(true)
			case errors.Is(err, autoscan.ErrScanRejected):
				log.Warn().Err(err).Str("path", scan.Folder).Msg("Target rejected scan")
				return nil
			case errors.Is(err, autoscan.ErrLibraryNotMatched):
				log.Debug().Err(err).Str("path", scan.Folder).Msg("Target skipped unmatched library")
				return nil
			}
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return false, err
	}
	return delivered.Load(), nil
}

func (p *Processor) Process(targets []autoscan.Target) error {
	scan, err := p.store.GetAvailableScan(p.minimumAge)
	if err != nil {
		return err
	}

	// Check whether all anchors are present
	for _, anchor := range p.anchors {
		if !fileExists(anchor) {
			return fmt.Errorf("%s: %w", anchor, autoscan.ErrAnchorUnavailable)
		}
	}

	// Fatal or Target Unavailable -> return original error
	delivered, err := p.callTargets(targets, scan)
	if err != nil {
		return err
	}

	err = p.store.Delete(scan)
	if err != nil {
		return err
	}

	if delivered {
		p.processed.Add(1)
	}
	return nil
}

// ProcessTarget delivers one eligible folder from this target's durable queue.
// New events replace the queued folder while preserving an in-flight generation.
func (p *Processor) ProcessTarget(targetID string, target autoscan.Target) error {
	scan, err := p.store.GetTargetScan(targetID, p.minimumAge)
	if err != nil {
		return err
	}
	for _, anchor := range p.anchors {
		if !fileExists(anchor) {
			return fmt.Errorf("%s: %w", anchor, autoscan.ErrAnchorUnavailable)
		}
	}
	err = target.Scan(scan.Scan)
	if err != nil && !errors.Is(err, autoscan.ErrScanRejected) && !errors.Is(err, autoscan.ErrLibraryNotMatched) {
		return err
	}
	if ackErr := p.store.AcknowledgeTarget(targetID, scan); ackErr != nil {
		return ackErr
	}
	if errors.Is(err, autoscan.ErrScanRejected) {
		log.Warn().Err(err).Str("target_queue", targetID).Str("path", scan.Folder).
			Msg("Target rejected scan, removing this delivery from its queue")
	} else if errors.Is(err, autoscan.ErrLibraryNotMatched) {
		log.Debug().Err(err).Str("target_queue", targetID).Str("path", scan.Folder).
			Msg("Target skipped unmatched library")
	} else {
		p.processed.Add(1)
	}
	return nil
}

var fileExists = func(fileName string) bool {
	info, err := os.Stat(fileName)
	if err != nil {
		return false
	}
	return !info.IsDir()
}
