package disk

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"rillweave/compress"
	"rillweave/graph"
	"rillweave/image"
)

const (
	suffix        = ".rill"
	compactSuffix = ".rillc"
)

func (d *Directory) RestoreHeads(accept func(string, image.Envelope) error) error {
	return d.restore(suffix, image.MaxImage, func(name string, raw []byte) error {
		var envelope image.Envelope
		if err := image.ReadJSON(bytes.NewReader(raw), &envelope); err != nil {
			return err
		}
		return accept(name, envelope)
	})
}

// RestoreCompacts reopens durable compressed graph heads alongside regular
// graph heads. The distinct suffix keeps the two image kinds from being
// mistaken for each other on startup.
func (d *Directory) RestoreCompacts(accept func(string, compress.Envelope) error) error {
	return d.restore(compactSuffix, compress.MaxDocument, func(name string, raw []byte) error {
		var envelope compress.Envelope
		if err := compress.ReadJSON(bytes.NewReader(raw), &envelope); err != nil {
			return err
		}
		return accept(name, envelope)
	})
}

func (d *Directory) restore(suffixName string, limit int64, parse func(string, []byte) error) error {
	entries, err := os.ReadDir(d.root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), suffixName) {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), suffixName)
		if !graph.ValidID(name) || !entry.Type().IsRegular() {
			return fmt.Errorf("invalid graph head entry %q", entry.Name())
		}
		raw, err := os.ReadFile(filepath.Join(d.root, entry.Name()))
		if err == nil {
			if int64(len(raw)) > limit {
				err = fmt.Errorf("image exceeds size limit")
			} else {
				err = parse(name, raw)
			}
		}
		if err != nil {
			return fmt.Errorf("recover graph %q: %w", name, err)
		}
	}
	return nil
}

// Write reports uncertainty only after the durable pathname has changed.
func (d *Directory) Write(name string, encoded image.Envelope) (uncertain bool, err error) {
	return d.write(name, suffix, encoded)
}

// WriteCompact durably installs a compressed graph head with its own suffix.
func (d *Directory) WriteCompact(name string, encoded compress.Envelope) (uncertain bool, err error) {
	return d.write(name, compactSuffix, encoded)
}

func (d *Directory) write(name, suffixName string, payload any) (uncertain bool, err error) {
	if !graph.ValidID(name) {
		return false, fmt.Errorf("invalid graph name")
	}
	f, err := os.CreateTemp(d.root, ".rill-pending-")
	if err != nil {
		return false, err
	}
	defer os.Remove(f.Name())
	if err = json.NewEncoder(f).Encode(payload); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return false, err
	}
	if closeErr != nil {
		return false, closeErr
	}
	if err = os.Rename(f.Name(), filepath.Join(d.root, name+suffixName)); err != nil {
		return false, err
	}
	if err = d.dir.Sync(); err != nil {
		return true, err
	}
	return false, nil
}
