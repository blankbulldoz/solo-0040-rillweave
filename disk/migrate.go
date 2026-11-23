package disk

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"rillweave/image"
)

// MigrationReport records exactly what one migration run changed so operators
// can audit reruns and interrupted cutovers.
type MigrationReport struct {
	Database         string         `json:"database"`
	AlreadyMigrated  bool           `json:"already_migrated"`
	Heads            []MigratedHead `json:"heads"`
	ArchivedHeads    []string       `json:"archived_heads"`
	RemovedTempFiles []string       `json:"removed_temp_files"`
}

// MigratedHead names a converted graph and the revision preserved from its
// legacy head file. Revisions are carried over verbatim and never reset to 1.
type MigratedHead struct {
	Name     string `json:"name"`
	Revision uint64 `json:"revision"`
}

// Migrate converts a legacy data directory (one .rill file per graph) to the
// SQLite database. It requires exclusive ownership, so a running server blocks
// it. The operation is crash-safe and rerunnable:
//
//   - A crash before the database is renamed leaves only removable staging
//     files; every .rill head is untouched and the old server keeps working.
//   - A crash at or after the rename leaves rillweave.db in place; the rerun
//     verifies it, finishes archiving head files and returns success.
func Migrate(root string) (MigrationReport, error) {
	lock, dir, err := acquireLock(root)
	if err != nil {
		return MigrationReport{}, err
	}
	owned := &ownership{root: root, lock: lock, dir: dir}
	defer func() { _ = owned.release() }()

	report := MigrationReport{
		Database:         filepath.Join(root, databaseName),
		Heads:            []MigratedHead{},
		ArchivedHeads:    []string{},
		RemovedTempFiles: []string{},
	}

	if databaseExists(root) {
		// Cutover already happened (possibly during an interrupted earlier
		// run): verify the database instead of rebuilding it.
		store, err := openSQLite(owned)
		if err != nil {
			return MigrationReport{}, err
		}
		if err := store.Close(); err != nil {
			return MigrationReport{}, err
		}
		report.AlreadyMigrated = true
		if err := finishCutover(root, &report); err != nil {
			return MigrationReport{}, err
		}
		if err := removeLegacyTempFiles(root, &report); err != nil {
			return MigrationReport{}, err
		}
		return report, nil
	}

	if err := removeStaging(root); err != nil {
		return MigrationReport{}, err
	}

	heads, err := readLegacyHeads(root)
	if err != nil {
		return MigrationReport{}, err
	}

	store, err := attachSQLite(owned, filepath.Join(root, stageName))
	if err != nil {
		return MigrationReport{}, err
	}
	store.ownsLock = false
	for _, head := range heads {
		// Write commits per graph; the staging database is not visible to any
		// server until it is renamed into place below.
		if _, err := store.Write(head.name, head.revision, head.envelope); err != nil {
			_ = store.Close()
			_ = removeStaging(root)
			return MigrationReport{}, fmt.Errorf("stage graph %q: %w", head.name, err)
		}
		report.Heads = append(report.Heads, MigratedHead{head.name, head.revision})
	}
	if err := verifyStaging(store, heads); err != nil {
		_ = store.Close()
		_ = removeStaging(root)
		return MigrationReport{}, err
	}
	if err := store.Close(); err != nil {
		return MigrationReport{}, err
	}
	if err := removeStagingSidecars(root); err != nil {
		return MigrationReport{}, err
	}

	if os.Getenv("RILLWEAVE_MIGRATE_ABORT_BEFORE_RENAME") != "" {
		// Test/injection hook: simulate a crash before the commit point. The
		// staging database is removed and every legacy head stays active.
		_ = removeStaging(root)
		return MigrationReport{}, fmt.Errorf("migration interrupted before database activation (injected)")
	}

	// Commit point: once this rename and directory sync succeed, the data
	// directory is in SQLite mode and the legacy heads are no longer active.
	if err := os.Rename(filepath.Join(root, stageName), filepath.Join(root, databaseName)); err != nil {
		_ = removeStaging(root)
		return MigrationReport{}, err
	}
	if err := dir.Sync(); err != nil {
		return MigrationReport{}, fmt.Errorf("activate database: %w", err)
	}

	if err := finishCutover(root, &report); err != nil {
		return MigrationReport{}, err
	}
	// Legacy temp files can only be removed once the new database is active.
	if err := removeLegacyTempFiles(root, &report); err != nil {
		return MigrationReport{}, err
	}
	if err := dir.Sync(); err != nil {
		return MigrationReport{}, err
	}
	return report, nil
}

// removeLegacyTempFiles deletes uninstalled leftovers of legacy publications
// (.rill-pending-*). They are not heads and the SQLite backend never creates
// files with this prefix, so removing them cannot touch active storage.
func removeLegacyTempFiles(root string, report *MigrationReport) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	var temps []string
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasPrefix(entry.Name(), headsPrefix) {
			temps = append(temps, entry.Name())
		}
	}
	slices.Sort(temps)
	for _, name := range temps {
		if err := os.Remove(filepath.Join(root, name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove legacy temp file %q: %w", name, err)
		}
		report.RemovedTempFiles = append(report.RemovedTempFiles, name)
	}
	if len(temps) == 0 {
		return nil
	}
	dir, err := os.Open(root)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

type legacyHead struct {
	name     string
	revision uint64
	envelope image.Envelope
}

func readLegacyHeads(root string) ([]legacyHead, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var heads []legacyHead
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), headsSuffix) {
			// Pending temp files and unrelated entries are handled elsewhere;
			// only .rill heads participate in the conversion.
			continue
		}
		name := strings.TrimSuffix(entry.Name(), headsSuffix)
		head, err := readLegacyHead(root, entry.Name(), name)
		if err != nil {
			return nil, err
		}
		heads = append(heads, head)
	}
	slices.SortFunc(heads, func(a, b legacyHead) int { return strings.Compare(a.name, b.name) })
	return heads, nil
}

func readLegacyHead(root, file, name string) (legacyHead, error) {
	envelope, err := readEnvelope(filepath.Join(root, file))
	if err != nil {
		return legacyHead{}, fmt.Errorf("read legacy head %q: %w", name, err)
	}
	g, err := image.Decode(envelope)
	if err != nil {
		return legacyHead{}, fmt.Errorf("verify legacy head %q: %w", name, err)
	}
	if g.Name != name {
		return legacyHead{}, fmt.Errorf("legacy head %q carries graph name %q", name, g.Name)
	}
	return legacyHead{name: name, revision: g.Revision, envelope: envelope}, nil
}

func verifyStaging(store *sqliteStore, heads []legacyHead) error {
	rows, err := store.db.Query(`SELECT name, revision FROM heads ORDER BY name`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var got []legacyHead
	for rows.Next() {
		var head legacyHead
		if err := rows.Scan(&head.name, &head.revision); err != nil {
			return err
		}
		got = append(got, head)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(got) != len(heads) {
		return fmt.Errorf("staged database holds %d heads, expected %d", len(got), len(heads))
	}
	for i, want := range heads {
		if got[i].name != want.name || got[i].revision != want.revision {
			return fmt.Errorf("staged head %d mismatch: got %q rev %d, want %q rev %d",
				i, got[i].name, got[i].revision, want.name, want.revision)
		}
	}
	return nil
}

// finishCutover moves every remaining legacy head out of the active directory
// into legacy-heads/. Archived files are kept for rollback but never reopened,
// because once rillweave.db exists it is the sole active storage.
func finishCutover(root string, report *MigrationReport) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	var heads []string
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), headsSuffix) {
			heads = append(heads, entry.Name())
		}
	}
	if len(heads) == 0 {
		return nil
	}
	archive := filepath.Join(root, archiveDir)
	if err := os.MkdirAll(archive, 0700); err != nil {
		return err
	}
	slices.Sort(heads)
	for _, file := range heads {
		target := filepath.Join(archive, file)
		if _, err := os.Stat(target); err == nil {
			// A previous run archived an identical name; keep both copies.
			target = filepath.Join(archive, file+".rerun")
		}
		if err := os.Rename(filepath.Join(root, file), target); err != nil {
			return fmt.Errorf("archive legacy head %q: %w", file, err)
		}
		report.ArchivedHeads = append(report.ArchivedHeads, file)
	}
	dir, err := os.Open(root)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func removeStaging(root string) error {
	for _, name := range []string{stageName, stageName + "-wal", stageName + "-shm"} {
		if err := os.Remove(filepath.Join(root, name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove staging file %q: %w", name, err)
		}
	}
	return nil
}

func removeStagingSidecars(root string) error {
	for _, name := range []string{stageName + "-wal", stageName + "-shm"} {
		if err := os.Remove(filepath.Join(root, name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove staging sidecar %q: %w", name, err)
		}
	}
	return nil
}
