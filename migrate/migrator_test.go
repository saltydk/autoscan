package migrate

import (
	"database/sql"
	"embed"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

//go:embed testdata/migrations/*.sql
var testMigrations embed.FS

func openTestDatabase(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})
	if _, err := db.Exec("PRAGMA busy_timeout = 0"); err != nil {
		t.Fatal(err)
	}
	return db
}

func checkMigrationState(t *testing.T, db *sql.DB, want int) {
	t.Helper()
	for _, query := range []string{
		"SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'migration_result'",
		"SELECT count(*) FROM schema_migration WHERE component = 'test' AND version = 1",
	} {
		var count int
		if err := db.QueryRow(query).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != want {
			t.Errorf("%s = %d, want %d", query, count, want)
		}
	}
}

func TestMigrateCommitFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migrations.db")
	db := openTestDatabase(t, path)
	if _, err := db.Exec("PRAGMA journal_mode = DELETE"); err != nil {
		t.Fatal(err)
	}
	m, err := New(db, "testdata/migrations")
	if err != nil {
		t.Fatal(err)
	}
	reader := openTestDatabase(t, path)
	readTx, err := reader.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer readTx.Rollback()
	var count int
	if err := readTx.QueryRow("SELECT count(*) FROM schema_migration").Scan(&count); err != nil {
		t.Fatal(err)
	}

	// The reader permits migration statements but blocks the exclusive lock needed to commit.
	err = m.Migrate(&testMigrations, "test")
	if err == nil || !strings.Contains(err.Error(), "commit:") {
		t.Fatalf("Migrate() = %v, want a commit error", err)
	}
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) || sqliteErr.Code() != sqlite3.SQLITE_BUSY {
		t.Fatalf("Migrate() = %v, want wrapped SQLITE_BUSY", err)
	}
	if err := readTx.Rollback(); err != nil {
		t.Fatal(err)
	}

	checkMigrationState(t, db, 0)
	if err := m.Migrate(&testMigrations, "test"); err != nil {
		t.Fatalf("retry migration: %v", err)
	}
	checkMigrationState(t, db, 1)
}

func TestMigrationStatementFailureRollsBack(t *testing.T) {
	db := openTestDatabase(t, filepath.Join(t.TempDir(), "migrations.db"))
	m, err := New(db, "testdata/migrations")
	if err != nil {
		t.Fatal(err)
	}
	failed := &migration{
		Version: 1,
		Schema:  "CREATE TABLE migration_result (id INTEGER PRIMARY KEY); INSERT INTO missing_table VALUES (1);",
	}
	err = m.exec("test", failed)
	if err == nil || !strings.HasPrefix(err.Error(), "exec:") {
		t.Fatalf("exec() = %v, want a statement error", err)
	}
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) || sqliteErr.Code() != sqlite3.SQLITE_ERROR {
		t.Fatalf("exec() = %v, want wrapped SQLITE_ERROR", err)
	}
	checkMigrationState(t, db, 0)
	if err := m.Migrate(&testMigrations, "test"); err != nil {
		t.Fatalf("retry migration: %v", err)
	}
	checkMigrationState(t, db, 1)
}
