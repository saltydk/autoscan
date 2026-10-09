package processor

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/saltydk/autoscan"
)

type queuedScan struct {
	autoscan.Scan
	generation int64
}

const sqlUpsertTarget = `
INSERT INTO target_scan (target_id, folder, priority, time)
VALUES (?, ?, ?, ?)
ON CONFLICT (target_id, folder) DO UPDATE SET
	priority = MAX(excluded.priority, target_scan.priority),
	time = excluded.time,
	generation = target_scan.generation + 1
`

func (store *datastore) UpsertTargets(targetIDs []string, scans []autoscan.Scan) error {
	tx, err := store.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range targetIDs {
		for _, scan := range scans {
			if _, err := tx.Exec(sqlUpsertTarget, id, scan.Folder, scan.Priority, scan.Time); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// AssignLegacy moves the shared queue to every configured target in one transaction.
func (store *datastore) AssignLegacy(targetIDs []string) error {
	if len(targetIDs) == 0 {
		return nil
	}
	tx, err := store.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	const assign = `
INSERT INTO target_scan (target_id, folder, priority, time)
SELECT ?, folder, priority, time FROM scan WHERE true
ON CONFLICT (target_id, folder) DO UPDATE SET
	priority = MAX(excluded.priority, target_scan.priority),
	time = MAX(excluded.time, target_scan.time),
	generation = target_scan.generation + 1
`
	for _, id := range targetIDs {
		if _, err := tx.Exec(assign, id); err != nil {
			return fmt.Errorf("assign legacy scans to %s: %w", id, err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM scan`); err != nil {
		return err
	}
	return tx.Commit()
}

func (store *datastore) GetTargetScan(targetID string, minAge time.Duration) (queuedScan, error) {
	const query = `
SELECT folder, priority, time, generation FROM target_scan
WHERE target_id = ? AND time < ?
ORDER BY priority DESC, time ASC
LIMIT 1
`
	var scan queuedScan
	err := store.QueryRow(query, targetID, now().Add(-minAge)).Scan(
		&scan.Folder, &scan.Priority, &scan.Time, &scan.generation,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return scan, autoscan.ErrNoScans
	}
	if err != nil {
		return scan, fmt.Errorf("get target scan: %v: %w", err, autoscan.ErrFatal)
	}
	return scan, nil
}

func (store *datastore) AcknowledgeTarget(targetID string, scan queuedScan) error {
	const query = `DELETE FROM target_scan WHERE target_id = ? AND folder = ? AND generation = ?`
	_, err := store.Exec(query, targetID, scan.Folder, scan.generation)
	if err != nil {
		return fmt.Errorf("acknowledge target scan: %v: %w", err, autoscan.ErrFatal)
	}
	return nil
}

func (store *datastore) TargetScansRemaining(targetIDs []string) (int, error) {
	if len(targetIDs) == 0 {
		return store.GetScansRemaining()
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(targetIDs)), ",")
	args := make([]any, len(targetIDs))
	for i, id := range targetIDs {
		args[i] = id
	}
	query := `SELECT COUNT(*) FROM target_scan WHERE target_id IN (` + placeholders + `)`
	var remaining int
	if err := store.QueryRow(query, args...).Scan(&remaining); err != nil {
		return 0, fmt.Errorf("get remaining target scans: %v: %w", err, autoscan.ErrFatal)
	}
	return remaining, nil
}
