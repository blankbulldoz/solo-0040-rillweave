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

func (d *Directory) RestoreHeads(accept func(string, image.Envelope) error) error {
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
			return fmt.Errorf("invalid graph head entry %q", entry.Name())
		}
		encoded, err := readEnvelope(filepath.Join(d.root, entry.Name()))
		if err == nil {
			err = accept(name, encoded)
		}
		if err != nil {
			return fmt.Errorf("recover graph %q: %w", name, err)
		}
	}
	return nil
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
