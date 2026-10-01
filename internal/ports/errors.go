package ports

import (
	"errors"
	"io/fs"
)

var (
	ErrClosed               = errors.New("capture resource is closed")
	ErrGeometryChanged      = errors.New("capture geometry or pixel format changed")
	ErrUnsupportedFormat    = errors.New("unsupported capture pixel format")
	ErrInvalidRegion        = errors.New("invalid output-local capture region")
	ErrUnsupportedTransform = errors.New("unsupported output transform")
	ErrOutputNotFound       = errors.New("capture output not found")
	// ErrCaptureStopped means the compositor ended the capture session or
	// refused the client.
	ErrCaptureStopped = errors.New("capture stopped by the compositor")
	// ErrWorkspaceUnavailable means the workspace is gone, or is not displayed
	// on a compositor that cannot capture hidden workspaces.
	ErrWorkspaceUnavailable = errors.New("capture workspace unavailable")
	// ErrPathExists is the standard filesystem conflict sentinel.
	ErrPathExists = fs.ErrExist
)
