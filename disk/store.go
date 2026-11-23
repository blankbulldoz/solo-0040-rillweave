package disk

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"rillweave/image"
)

const (
	lockName     = ".rillweave.lock"
	databaseName = "rillweave.db"
	stageName    = "rillweave.db.stage"
	// headsPrefix marks the temporary images produced by the legacy file
	// backend; the same prefix is used by an interrupted publication.
	headsPrefix = ".rill-pending-"
	// archiveDir holds superseded per-graph head files after migration.
	archiveDir = "legacy-heads"
)

// Store is the single active durable storage of a data directory. A data
// directory is either in legacy file mode (one .rill head per graph) or in
// SQLite mode (rillweave.db). The two modes are never mixed at runtime.
type Store interface {
	// RestoreHeads hands every durable head envelope to accept, in name order.
	RestoreHeads(accept func(string, image.Envelope) error) error
	// Write installs one graph head. uncertain is true only when the durable
	// pathname may already hold the new state but durability is unconfirmed.
	Write(name string, revision uint64, encoded image.Envelope) (uncertain bool, err error)
	Close() error
}

// Open exclusively owns root and selects the only active storage backend.
// When rillweave.db exists it is authoritative; any leftover .rill head is
// ignored rather than silently served (and reported through strayHeads).
func Open(root string) (Store, []string, error) {
	lock, dir, err := acquireLock(root)
	if err != nil {
		return nil, nil, err
	}
	owned := &ownership{root: root, lock: lock, dir: dir}
	if _, statErr := os.Stat(filepath.Join(root, databaseName)); statErr == nil {
		stray := listLegacyHeads(root)
		store, err := openSQLite(owned)
		if err != nil {
			_ = owned.release()
			return nil, nil, err
		}
		return store, stray, nil
	}
	return &fileStore{owned: owned}, nil, nil
}

type ownership struct {
	root string
	lock *os.File
	dir  *os.File
}

func acquireLock(root string) (*os.File, *os.File, error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, nil, err
	}
	dir, err := os.Open(root)
	if err != nil {
		return nil, nil, err
	}
	lock, err := os.OpenFile(filepath.Join(root, lockName), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		_ = dir.Close()
		return nil, nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		_ = dir.Close()
		return nil, nil, fmt.Errorf("cannot acquire graph directory lock: %w", err)
	}
	return lock, dir, nil
}

func (o *ownership) release() error {
	dirErr := o.dir.Close()
	lockErr := o.lock.Close()
	if dirErr != nil {
		return dirErr
	}
	return lockErr
}

func (o *ownership) syncDir() error { return o.dir.Sync() }

func listLegacyHeads(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var heads []string
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), headsSuffix) {
			heads = append(heads, entry.Name())
		}
	}
	slices.Sort(heads)
	return heads
}
