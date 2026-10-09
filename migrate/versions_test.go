package migrate

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"
)

func TestMigrationVersionsRejectsIncompleteRead(t *testing.T) {
	readErr := errors.New("migration version read interrupted")
	db := sql.OpenDB(versionReadConnector{err: readErr})
	t.Cleanup(func() { db.Close() })
	m := &Migrator{db: db}
	versions, err := m.versions("processor")
	if !errors.Is(err, readErr) {
		t.Fatalf("versions() = %v, want wrapped read error", err)
	}
	if versions != nil {
		t.Fatalf("failed version query returned a partial migration history: %v", versions)
	}
}

type versionReadConnector struct{ err error }

func (c versionReadConnector) Connect(context.Context) (driver.Conn, error) {
	return &versionReadConn{err: c.err}, nil
}

func (c versionReadConnector) Driver() driver.Driver { return versionReadDriver(c) }

type versionReadDriver struct{ err error }

func (d versionReadDriver) Open(string) (driver.Conn, error) {
	return &versionReadConn{err: d.err}, nil
}

type versionReadConn struct{ err error }

func (*versionReadConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("version reader does not prepare statements")
}

func (*versionReadConn) Close() error { return nil }

func (*versionReadConn) Begin() (driver.Tx, error) {
	return nil, errors.New("version reader does not start transactions")
}

func (c *versionReadConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return &versionReadRows{err: c.err}, nil
}

type versionReadRows struct {
	err     error
	started bool
}

func (*versionReadRows) Columns() []string { return []string{"version"} }
func (*versionReadRows) Close() error      { return nil }

func (r *versionReadRows) Next(values []driver.Value) error {
	if !r.started {
		r.started = true
		values[0] = int64(1)
		return nil
	}
	return r.err
}
