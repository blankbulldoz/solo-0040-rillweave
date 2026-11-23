package disk

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"

	"rillweave/graph"
	"rillweave/image"
)

const (
	sqliteDriver  = "sqlite"
	schemaVersion = "1"
)

type sqliteStore struct {
	owned    *ownership
	db       *sql.DB
	ownsLock bool
}

// openSQLite opens an existing migrated database and verifies its schema.
func openSQLite(owned *ownership) (*sqliteStore, error) {
	store, err := attachSQLite(owned, filepath.Join(owned.root, databaseName))
	if err != nil {
		return nil, err
	}
	var version string
	row := store.db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`)
	if err := row.Scan(&version); err != nil {
		store.ownsLock = false
		_ = store.Close()
		return nil, fmt.Errorf("verify database schema: %w", err)
	}
	if version != schemaVersion {
		store.ownsLock = false
		_ = store.Close()
		return nil, fmt.Errorf("unsupported storage schema version %q (server supports %q)", version, schemaVersion)
	}
	return store, nil
}

// attachSQLite opens path with crash-safe pragmas and creates the schema when
// absent. A single connection is sufficient: the engine serializes every
// storage call, and reads never touch disk after startup.
func attachSQLite(owned *ownership, path string) (*sqliteStore, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(FULL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open(sqliteDriver, dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &sqliteStore{owned: owned, db: db, ownsLock: true}, nil
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
INSERT OR IGNORE INTO meta(key, value) VALUES ('schema_version', '` + schemaVersion + `');
CREATE TABLE IF NOT EXISTS heads (
	name         TEXT PRIMARY KEY,
	revision     INTEGER NOT NULL CHECK (revision >= 1),
	format       INTEGER NOT NULL,
	digest       TEXT NOT NULL,
	payload      BLOB NOT NULL,
	installed_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);`

func (s *sqliteStore) RestoreHeads(accept func(string, image.Envelope) error) error {
	rows, err := s.db.Query(
		`SELECT name, revision, format, digest, payload FROM heads ORDER BY name`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name, digest string
		var revision, format int
		var payload []byte
		if err := rows.Scan(&name, &revision, &format, &digest, &payload); err != nil {
			return err
		}
		if err := accept(name, image.Envelope{Format: format, Digest: digest, Payload: payload}); err != nil {
			return fmt.Errorf("recover graph %q: %w", name, err)
		}
	}
	return rows.Err()
}

// Write commits one head in an atomic SQLite transaction. The revision guard
// refuses to overwrite a newer head with a replayed older publication.
func (s *sqliteStore) Write(name string, revision uint64, encoded image.Envelope) (bool, error) {
	if !graph.ValidID(name) {
		return false, fmt.Errorf("invalid graph name")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var current sql.NullInt64
	if err := tx.QueryRow(`SELECT revision FROM heads WHERE name = ?`, name).Scan(&current); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if current.Valid && uint64(current.Int64) >= revision {
		return false, fmt.Errorf("refusing to replace revision %d of graph %q with revision %d",
			current.Int64, name, revision)
	}
	if _, err := tx.Exec(
		`INSERT INTO heads(name, revision, format, digest, payload)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET
		   revision = excluded.revision, format = excluded.format,
		   digest = excluded.digest, payload = excluded.payload`,
		name, int64(revision), encoded.Format, encoded.Digest, []byte(encoded.Payload)); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		// synchronous=FULL makes every successful commit durable; unlike the
		// legacy rename there is no window where the outcome is uncertain.
		return false, err
	}
	return false, nil
}

func (s *sqliteStore) Close() error {
	// Fold the WAL into the main database file so a clean shutdown leaves a
	// single self-contained database behind.
	_, checkpointErr := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	closeErr := s.db.Close()
	if checkpointErr != nil {
		return checkpointErr
	}
	if closeErr != nil {
		return closeErr
	}
	// During migration the caller owns the exclusive directory lock.
	if !s.ownsLock {
		return nil
	}
	return s.owned.release()
}

// databaseExists reports whether root has already switched to SQLite mode.
func databaseExists(root string) bool {
	_, err := os.Stat(filepath.Join(root, databaseName))
	return err == nil
}
