package ports

import (
	"errors"
	"io/fs"
)

var (
	ErrClosed            = errors.New("capture resource is closed")
	ErrGeometryChanged   = errors.New("capture geometry or pixel format changed")
	ErrUnsupportedFormat = errors.New("unsupported capture pixel format")
	ErrInvalidRegion     = errors.New("invalid output-local capture region")
	ErrOutputNotFound    = errors.New("capture output not found")
	// ErrPathExists is the standard filesystem conflict sentinel.
	ErrPathExists = fs.ErrExist
)
