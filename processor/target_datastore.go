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

type queueAdoption struct {
	from, to string
	scans    int
}

type dormantQueue struct {
	id    string
	scans int
}

// targetKind returns the target type prefix of a queue ID ("plex" for
// "plex:<hash>"), or "" when the ID carries no type and must never be adopted.
func targetKind(id string) string {
	kind, _, ok := strings.Cut(id, ":")
	if !ok {
		return ""
	}
	return kind
}

// AdoptRenamedQueues moves a queue to a target whose identity changed, such as
// an unnamed target with an edited URL or rewrite rules, and records which
// identities are configured for the next run.
//
// A source is a queue holding work whose identity was configured on the
// previous run and is missing now. A destination is a configured identity
// holding no work that has never been configured, or that the source itself
// was adopted from (a reverted edit). Adoption happens only when one target
// type has exactly one source and one destination; anything else stays dormant.
// Without identity history (the first run after upgrading) no identity is
// active yet, so nothing is adopted: a renamed target cannot be told apart from
// a removed one. It returns
// the adoptions made and the dormant queues that still hold work.
func (store *datastore) AdoptRenamedQueues(targetIDs []string) ([]queueAdoption, []dormantQueue, error) {
	tx, err := store.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()

	queued := make(map[string]int)
	rows, err := tx.Query(`SELECT target_id, COUNT(*) FROM target_scan GROUP BY target_id`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var id string
		var count int
		if err := rows.Scan(&id, &count); err != nil {
			rows.Close()
			return nil, nil, err
		}
		queued[id] = count
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, nil, err
	}

	type identity struct {
		active      bool
		adoptedFrom string
	}
	known := make(map[string]identity)
	rows, err = tx.Query(`SELECT id, active, COALESCE(adopted_from, '') FROM target_identity`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var id string
		var ident identity
		if err := rows.Scan(&id, &ident.active, &ident.adoptedFrom); err != nil {
			rows.Close()
			return nil, nil, err
		}
		known[id] = ident
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, nil, err
	}

	configured := make(map[string]bool, len(targetIDs))
	for _, id := range targetIDs {
		configured[id] = true
	}

	var adopted []queueAdoption
	sources := make(map[string][]string)
	for id := range queued {
		if kind := targetKind(id); kind != "" && !configured[id] && known[id].active {
			sources[kind] = append(sources[kind], id)
		}
	}
	for kind, from := range sources {
		if len(from) != 1 {
			continue
		}
		source := from[0]
		var candidates []string
		for _, id := range targetIDs {
			if targetKind(id) != kind || queued[id] > 0 {
				continue
			}
			if _, seen := known[id]; !seen || id == known[source].adoptedFrom {
				candidates = append(candidates, id)
			}
		}
		if len(candidates) != 1 {
			continue
		}
		to := candidates[0]
		if _, err := tx.Exec(`UPDATE target_scan SET target_id = ? WHERE target_id = ?`, to, source); err != nil {
			return nil, nil, fmt.Errorf("adopt queue %s into %s: %w", source, to, err)
		}
		if _, err := tx.Exec(`INSERT INTO target_identity (id, adopted_from) VALUES (?, ?)
ON CONFLICT (id) DO UPDATE SET adopted_from = excluded.adopted_from`, to, source); err != nil {
			return nil, nil, err
		}
		adopted = append(adopted, queueAdoption{from: source, to: to, scans: queued[source]})
		delete(queued, source)
	}

	if _, err := tx.Exec(`UPDATE target_identity SET active = 0`); err != nil {
		return nil, nil, err
	}
	for _, id := range targetIDs {
		if _, err := tx.Exec(`INSERT INTO target_identity (id, active) VALUES (?, 1)
ON CONFLICT (id) DO UPDATE SET active = 1`, id); err != nil {
			return nil, nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}

	var dormant []dormantQueue
	for id, count := range queued {
		if !configured[id] {
			dormant = append(dormant, dormantQueue{id: id, scans: count})
		}
	}
	return adopted, dormant, nil
}

// DormantScansRemaining counts queued scans held by identities not in targetIDs.
func (store *datastore) DormantScansRemaining(targetIDs []string) (int, error) {
	query := `SELECT COUNT(*) FROM target_scan`
	args := make([]any, len(targetIDs))
	if len(targetIDs) > 0 {
		query += ` WHERE target_id NOT IN (` + strings.TrimSuffix(strings.Repeat("?,", len(targetIDs)), ",") + `)`
		for i, id := range targetIDs {
			args[i] = id
		}
	}
	var dormant int
	if err := store.QueryRow(query, args...).Scan(&dormant); err != nil {
		return 0, fmt.Errorf("get dormant target scans: %v: %w", err, autoscan.ErrFatal)
	}
	return dormant, nil
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
