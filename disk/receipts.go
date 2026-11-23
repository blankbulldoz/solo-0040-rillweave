package disk

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"rillweave/graph"
)

const (
	receiptSuffix = ".restore"
	pendingSuffix = ".restore-pending"
	maxReceipt    = 64 << 10
)

// ReadReceipts loads the retained overwrite receipts. Temporary files, graph
// images and entries that cannot name a graph are ignored.
func (d *Directory) ReadReceipts() (map[string][]byte, error) {
	return d.readSuffix(receiptSuffix)
}

// ReadPendings loads the in-flight overwrite intents that were written before
// a head publication and not yet confirmed or discarded.
func (d *Directory) ReadPendings() (map[string][]byte, error) {
	return d.readSuffix(pendingSuffix)
}

func (d *Directory) readSuffix(suffix string) (map[string][]byte, error) {
	entries, err := os.ReadDir(d.root)
	if err != nil {
		return nil, err
	}
	found := make(map[string][]byte)
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), suffix)
		if !ok || !graph.ValidID(name) || !entry.Type().IsRegular() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(d.root, entry.Name()))
		if err != nil {
			return nil, err
		}
		if len(raw) > maxReceipt {
			// A receipt this large cannot have been written by this package,
			// so it is stray input rather than recoverable state.
			continue
		}
		found[name] = raw
	}
	return found, nil
}

// WriteReceipt replaces the retained receipt for one graph.
func (d *Directory) WriteReceipt(name string, raw []byte) (bool, error) {
	return d.writeAtomic(name, receiptSuffix, raw)
}

// WritePending records an in-flight overwrite for one graph.
func (d *Directory) WritePending(name string, raw []byte) (bool, error) {
	return d.writeAtomic(name, pendingSuffix, raw)
}

// RemoveReceipt drops the retained receipt for one graph. A missing receipt is
// not an error and does not force a directory synchronization.
func (d *Directory) RemoveReceipt(name string) error {
	return d.remove(name, receiptSuffix)
}

// RemovePending drops an in-flight overwrite for one graph.
func (d *Directory) RemovePending(name string) error {
	return d.remove(name, pendingSuffix)
}

func (d *Directory) writeAtomic(name, suffix string, raw []byte) (uncertain bool, err error) {
	if !graph.ValidID(name) {
		return false, fmt.Errorf("invalid graph name")
	}
	if len(raw) > maxReceipt {
		return false, fmt.Errorf("restore receipt exceeds size limit")
	}
	f, err := os.CreateTemp(d.root, ".restore-tmp-")
	if err != nil {
		return false, err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return false, err
	}
	if closeErr != nil {
		return false, closeErr
	}
	if err = os.Rename(f.Name(), filepath.Join(d.root, name+suffix)); err != nil {
		return false, err
	}
	if err = d.dir.Sync(); err != nil {
		return true, err
	}
	return false, nil
}

func (d *Directory) remove(name, suffix string) error {
	if !graph.ValidID(name) {
		return fmt.Errorf("invalid graph name")
	}
	path := filepath.Join(d.root, name+suffix)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return d.dir.Sync()
}
