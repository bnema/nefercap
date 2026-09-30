// Package capturefile reserves capture output paths and removes only the file
// that was reserved, never a different file that later took its place.
package capturefile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/bnema/nefercap/internal/ports"
)

// Reservation identifies the file created by Create.
type Reservation struct {
	path string
	info fs.FileInfo
}

// Create makes a new private (0600) file at path with O_EXCL, so an existing
// file or symlink is never opened or overwritten. An existing path yields an
// error matching both ports.ErrPathExists and fs.ErrExist. The returned
// Reservation records the file's identity for a later Remove.
func Create(path string) (*os.File, *Reservation, error) {
	return create(path, os.O_WRONLY)
}

// CreateReadable reserves a file whose original descriptor can be reused for copying.
func CreateReadable(path string) (*os.File, *Reservation, error) {
	return create(path, os.O_RDWR)
}

func create(path string, access int) (*os.File, *Reservation, error) {
	f, err := os.OpenFile(path, access|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, nil, fmt.Errorf("%w: %w", ports.ErrPathExists, err)
		}
		return nil, nil, fmt.Errorf("create output: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		_ = os.Remove(path) // just created by us; identity is unknown, so remove now
		return nil, nil, fmt.Errorf("stat output: %w", err)
	}
	return f, &Reservation{path: path, info: info}, nil
}

// Remove deletes the reserved file if path still names it. A missing path is
// success; a path that now names a different file (the original was moved and
// replaced) is left untouched and is not an error. There is an unavoidable
// short window between the identity check and the unlink.
func (r *Reservation) Remove() error {
	cur, err := os.Lstat(r.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("inspect output: %w", err)
	}
	if !os.SameFile(r.info, cur) {
		return nil
	}
	if err := os.Remove(r.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove output: %w", err)
	}
	return nil
}
