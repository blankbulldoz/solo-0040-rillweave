package disk

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

type Directory struct {
	root string
	lock *os.File
	dir  *os.File
}

func Open(root string) (*Directory, error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	dir, err := os.Open(root)
	if err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(root, ".rillweave.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		_ = dir.Close()
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		_ = dir.Close()
		return nil, fmt.Errorf("cannot acquire graph directory lock: %w", err)
	}
	return &Directory{root, lock, dir}, nil
}

func (d *Directory) Close() error {
	dirErr := d.dir.Close()
	lockErr := d.lock.Close()
	if dirErr != nil {
		return dirErr
	}
	return lockErr
}
