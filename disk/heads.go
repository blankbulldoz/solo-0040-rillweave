package disk

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"rillweave/graph"
	"rillweave/image"
)

const suffix = ".rill"

// HeadEntry is one candidate durable head discovered by ScanHeads. Err and
// Structural describe a file that cannot be a usable head; Encoded is only
// meaningful when Err is nil.
type HeadEntry struct {
	Name       string
	File       string
	Encoded    image.Envelope
	Err        error
	Structural bool
}

// ScanHeads visits candidate head files in directory order. It only reads the
// directory: a rejected entry is reported, never renamed or removed.
func (d *Directory) ScanHeads(visit func(HeadEntry) error) error {
	entries, err := os.ReadDir(d.root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), suffix) {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), suffix)
		if !graph.ValidID(name) || !entry.Type().IsRegular() {
			next := HeadEntry{Name: name, File: entry.Name(), Structural: true,
				Err: fmt.Errorf("invalid graph head entry %q", entry.Name())}
			if err := visit(next); err != nil {
				return err
			}
			continue
		}
		encoded, readErr := readEnvelope(filepath.Join(d.root, entry.Name()))
		if err := visit(HeadEntry{Name: name, File: entry.Name(), Encoded: encoded, Err: readErr}); err != nil {
			return err
		}
	}
	return nil
}

// RestoreHeads enforces the default strict startup: the first unusable head
// file stops recovery.
func (d *Directory) RestoreHeads(accept func(string, image.Envelope) error) error {
	return d.ScanHeads(func(entry HeadEntry) error {
		if entry.Err != nil {
			if entry.Structural {
				return entry.Err
			}
			return fmt.Errorf("recover graph %q: %w", entry.Name, entry.Err)
		}
		if err := accept(entry.Name, entry.Encoded); err != nil {
			return fmt.Errorf("recover graph %q: %w", entry.Name, err)
		}
		return nil
	})
}

func readEnvelope(path string) (image.Envelope, error) {
	var e image.Envelope
	f, err := os.Open(path)
	if err != nil {
		return e, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, image.MaxImage+1))
	if err != nil {
		return e, err
	}
	if len(raw) > image.MaxImage {
		return e, fmt.Errorf("image exceeds size limit")
	}
	err = image.ReadJSON(bytes.NewReader(raw), &e)
	return e, err
}

// Write reports uncertainty only after the durable pathname has changed.
func (d *Directory) Write(name string, encoded image.Envelope) (uncertain bool, err error) {
	if !graph.ValidID(name) {
		return false, fmt.Errorf("invalid graph name")
	}
	f, err := os.CreateTemp(d.root, ".rill-pending-")
	if err != nil {
		return false, err
	}
	defer os.Remove(f.Name())
	if err = json.NewEncoder(f).Encode(encoded); err == nil {
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
