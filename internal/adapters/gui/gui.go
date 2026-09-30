// Package gui is the NeferGUI control panel adapter: a synchronous window that
// returns the user's capture selection before any capture begins.
package gui

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/bnema/nefergui"

	"github.com/bnema/nefercap/internal/ports"
)

//go:embed style.css
var styleSheet string

var errNoOutputs = fmt.Errorf("gui: no outputs to select: %w", ports.ErrOutputNotFound)

// Selector shows the control panel. Its zero value is ready to use.
type Selector struct{}

var _ ports.Selector = (*Selector)(nil)

// New returns a control panel selector.
func New() *Selector { return &Selector{} }

// Select blocks until the user accepts a selection (true), closes the window
// (false, nil) or ctx ends or the window system fails (error). The window is
// gone before an accepted selection is returned, so it cannot appear in the
// capture. Select must be called from one goroutine at a time.
func (s *Selector) Select(ctx context.Context, outputs []ports.Output) (ports.Selection, bool, error) {
	if err := ctx.Err(); err != nil {
		return ports.Selection{}, false, err
	}
	m, err := newModel(outputs, time.Now())
	if err != nil {
		return ports.Selection{}, false, err
	}
	sheet, removeSheet, err := writeStyleSheet()
	if err != nil {
		return ports.Selection{}, false, err
	}
	defer removeSheet()

	// The child context ends Run once the user decides; the parent stays
	// authoritative for real cancellation.
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	m.cancel = cancel
	runErr := nefergui.Run(child, m, view,
		nefergui.Title("NeferCap"),
		nefergui.Size(640, m.windowHeight()),
		nefergui.Styles(sheet),
	)
	return m.result(ctx, runErr)
}

// result maps Run's outcome. A Canceled error caused by our own accept or
// dismiss is not a failure, but any other error (including one joined with it,
// such as a surface Close failure) is reported and rejects the selection. When
// the parent context ended, its error is always reported and nothing is
// accepted.
func (m *model) result(parent context.Context, runErr error) (ports.Selection, bool, error) {
	if runErr != nil {
		if perr := parent.Err(); perr != nil {
			return ports.Selection{}, false, errors.Join(perr, dropCanceled(runErr))
		}
		if m.accepted || m.closed {
			runErr = dropCanceled(runErr)
		}
		if runErr != nil {
			return ports.Selection{}, false, runErr
		}
	}
	if m.accepted {
		return m.sel, true, nil
	}
	return ports.Selection{}, false, nil
}

// dropCanceled removes bare context.Canceled leaves from err and joined errors.
func dropCanceled(err error) error {
	if err == nil || err == context.Canceled {
		return nil
	}
	j, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return err
	}
	var keep []error
	for _, e := range j.Unwrap() {
		if e = dropCanceled(e); e != nil {
			keep = append(keep, e)
		}
	}
	if len(keep) == 1 {
		return keep[0]
	}
	return errors.Join(keep...)
}

// writeStyleSheet stores the embedded stylesheet in a private temporary file
// because NeferGUI loads styles by path. It is read once when Run starts, so
// removal after Run returns is enough; a removal failure leaves only a small
// file in the temporary directory and is not worth failing a capture over.
func writeStyleSheet() (path string, remove func(), err error) {
	f, err := os.CreateTemp("", "nefercap-gui-*.css")
	if err != nil {
		return "", nil, fmt.Errorf("gui: stylesheet: %w", err)
	}
	path = f.Name()
	remove = func() { _ = os.Remove(path) }
	if _, err := f.WriteString(styleSheet); err != nil {
		err = errors.Join(err, f.Close())
		remove()
		return "", nil, fmt.Errorf("gui: stylesheet: %w", err)
	}
	if err := f.Close(); err != nil {
		remove()
		return "", nil, fmt.Errorf("gui: stylesheet: %w", err)
	}
	return path, remove, nil
}
