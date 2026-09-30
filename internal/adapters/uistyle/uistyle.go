// Package uistyle hands embedded NeferGUI stylesheets to NeferGUI, which loads
// styles only by file path.
package uistyle

import (
	"errors"
	"fmt"
	"os"
)

// Write stores styles in a private (0600) temporary file named after pattern
// (see os.CreateTemp) and returns its path and a remove function. NeferGUI
// reads the file once when Run starts, so callers remove it when Run returns.
// remove is safe to call more than once; a removal failure only leaves a small
// file in the temporary directory and is not worth failing a capture over.
func Write(styles, pattern string) (path string, remove func(), err error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", nil, fmt.Errorf("stylesheet: %w", err)
	}
	path = f.Name()
	remove = func() { _ = os.Remove(path) }
	if _, err := f.WriteString(styles); err != nil {
		err = errors.Join(err, f.Close())
		remove()
		return "", nil, fmt.Errorf("stylesheet: %w", err)
	}
	if err := f.Close(); err != nil {
		remove()
		return "", nil, fmt.Errorf("stylesheet: %w", err)
	}
	return path, remove, nil
}
