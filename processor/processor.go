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
	anchors        []string
	minimumAge     time.Duration
	store          *datastore
	processed      atomic.Int64
	received       atomic.Int64
	retried        atomic.Int64
	skipped        atomic.Int64
	rejected       atomic.Int64
	anchorMu       sync.Mutex
	anchorObserved bool
	anchorMissing  []string
	targetMu       sync.RWMutex
	targetIDs      []string
}

func (p *Processor) Add(scans ...autoscan.Scan) error {
	if len(scans) == 0 {
		return nil
	}
	validScans := make([]autoscan.Scan, 0, len(scans))
	for _, scan := range scans {
		if !autoscan.ValidScanPath(scan.Folder) {
			log.Debug().Str("path", scan.Folder).Msg("Ignoring scan with invalid folder path")
			continue
		}
		validScans = append(validScans, scan)
	}
	if len(validScans) == 0 {
		return nil
	}
	p.targetMu.RLock()
	defer p.targetMu.RUnlock()
	var err error
	if len(p.targetIDs) > 0 {
		err = p.store.UpsertTargets(p.targetIDs, validScans)
	} else {
		err = p.store.Upsert(validScans)
	}
	if err == nil {
		p.recordReceived(len(validScans))
	}
	return err
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

func (p *Processor) callTargets(targets []autoscan.Target, scan autoscan.Scan) (bool, int64, int64, error) {
	g := new(errgroup.Group)
	var delivered atomic.Bool
	var skipped, rejected atomic.Int64

	for _, target := range targets {
		g.Go(func() error {
			err := target.Scan(scan)
			switch {
			case err == nil:
				delivered.Store(true)
			case errors.Is(err, autoscan.ErrScanRejected):
				log.Warn().Err(err).Str("path", scan.Folder).Msg("Target rejected scan")
				rejected.Add(1)
				return nil
			case errors.Is(err, autoscan.ErrLibraryNotMatched):
				log.Debug().Err(err).Str("path", scan.Folder).Msg("Target skipped unmatched library")
				skipped.Add(1)
				return nil
			}
			return err
		})
	}

	if err := g.Wait(); err != nil {
		return false, 0, 0, err
	}
	return delivered.Load(), skipped.Load(), rejected.Load(), nil
}

func (p *Processor) Process(targets []autoscan.Target) error {
	scan, err := p.store.GetAvailableScan(p.minimumAge)
	if err != nil {
		return err
	}

	// Check whether all anchors are present
	if err := p.checkAnchors(); err != nil {
		return err
	}

	// Fatal or Target Unavailable -> return original error
	delivered, skipped, rejected, err := p.callTargets(targets, scan)
	if err != nil {
		return err
	}

	err = p.store.Delete(scan)
	if err != nil {
		return err
	}

	if delivered {
		p.recordProcessed()
	}
	for range skipped {
		p.recordSkipped()
	}
	for range rejected {
		p.recordRejected()
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
	if err := p.checkAnchors(); err != nil {
		return err
	}
	err = target.Scan(scan.Scan)
	if err != nil && !errors.Is(err, autoscan.ErrScanRejected) && !errors.Is(err, autoscan.ErrLibraryNotMatched) {
		return err
	}
	if ackErr := p.store.AcknowledgeTarget(targetID, scan); ackErr != nil {
		return ackErr
	}
	if errors.Is(err, autoscan.ErrScanRejected) {
		p.recordRejected()
		log.Warn().Err(err).Str("target_queue", targetID).Str("path", scan.Folder).
			Msg("Target rejected scan, removing this delivery from its queue")
	} else if errors.Is(err, autoscan.ErrLibraryNotMatched) {
		p.recordSkipped()
		log.Debug().Err(err).Str("target_queue", targetID).Str("path", scan.Folder).
			Msg("Target skipped unmatched library")
	} else {
		p.recordProcessed()
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
