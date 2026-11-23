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

const headsSuffix = ".rill"

// fileStore is the legacy one-head-file-per-graph storage. It is used only by
// data directories that have never been migrated.
type fileStore struct {
	owned *ownership
}

func (f *fileStore) RestoreHeads(accept func(string, image.Envelope) error) error {
	entries, err := os.ReadDir(f.owned.root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), headsSuffix) {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), headsSuffix)
		if !graph.ValidID(name) || !entry.Type().IsRegular() {
			return fmt.Errorf("invalid graph head entry %q", entry.Name())
		}
		encoded, err := readEnvelope(filepath.Join(f.owned.root, entry.Name()))
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
func (f *fileStore) Write(name string, revision uint64, encoded image.Envelope) (uncertain bool, err error) {
	if !graph.ValidID(name) {
		return false, fmt.Errorf("invalid graph name")
	}
	file, err := os.CreateTemp(f.owned.root, headsPrefix)
	if err != nil {
		return false, err
	}
	defer os.Remove(file.Name())
	if err = json.NewEncoder(file).Encode(encoded); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return false, err
	}
	if closeErr != nil {
		return false, closeErr
	}
	if err = os.Rename(file.Name(), filepath.Join(f.owned.root, name+headsSuffix)); err != nil {
		return false, err
	}
	if err = f.owned.syncDir(); err != nil {
		return true, err
	}
	return false, nil
}

func (f *fileStore) Close() error { return f.owned.release() }
