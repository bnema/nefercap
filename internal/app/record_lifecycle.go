package app

import (
	"context"
	"errors"
	"os"
	"sync/atomic"

	"github.com/bnema/nefercap/internal/adapters/uierrors"
)

var (
	// errUserStop is the cancellation cause of a stop requested by the user
	// (control command or HUD button). It is not a failure.
	errUserStop = errors.New("recording stopped by user")
	// errHUDClosed and errHUDClosedEarly report a HUD that ended although
	// nobody asked it to.
	errHUDClosed      = errors.New("recording controls closed unexpectedly")
	errHUDClosedEarly = errors.New("recording controls closed before authorization")
)

// recordingOutcome says what a recording left behind, independently of the
// error: only a Saved recording has a path worth printing.
type recordingOutcome struct{ Saved bool }

// lifecycle is the state shared by the recording owner, the stop watchers and
// the compositor's detach callback. Everything here is safe for concurrent use
// and non-blocking. It never touches the Source: the owner goroutine alone
// runs capture, so borrowed pixels and protocol state stay single-owner.
//
// The recording context carries the first cause that ended it: errUserStop for
// a requested stop, or the first real failure. A failure is also kept in a
// one-slot channel so it survives a parent cancellation that won the context.
type lifecycle struct {
	parent       context.Context
	cancelRecord context.CancelCauseFunc
	cancelHUD    context.CancelFunc
	shuttingDown atomic.Bool
	failure      chan error
}

func newLifecycle(parent context.Context, cancelRecord context.CancelCauseFunc, cancelHUD context.CancelFunc) *lifecycle {
	return &lifecycle{parent: parent, cancelRecord: cancelRecord, cancelHUD: cancelHUD, failure: make(chan error, 1)}
}

// userStop ends the recording and its HUD without a failure. The flag is set
// first so the detach the HUD teardown provokes is recognised as an echo.
func (l *lifecycle) userStop() {
	l.shuttingDown.Store(true)
	l.cancelRecord(errUserStop)
	l.cancelHUD()
}

// beginShutdown marks the owner's own teardown; call it before stopping the HUD.
func (l *lifecycle) beginShutdown() { l.shuttingDown.Store(true) }

// fail records the first real failure, and ends recording and HUD with it as
// the cause. It reports whether err was the first.
func (l *lifecycle) fail(err error) bool {
	select {
	case l.failure <- err:
	default:
		return false
	}
	l.cancelRecord(err)
	l.cancelHUD()
	return true
}

// detached is the compositor's layer-detach callback. A detach that arrives
// before shutdown begins is real: the exclusion is gone and recording must
// stop. One that follows our own shutdown or the parent's cancellation is the
// echo of closing the HUD and is ignored.
func (l *lifecycle) detached(err error) {
	if l.shuttingDown.Load() || l.parent.Err() != nil {
		return
	}
	if err == nil {
		err = errHUDClosed
	}
	l.fail(err)
}

// hudEnded handles a HUD that returned. If nobody asked it to stop, that is a
// failure: its own real error, or a closed error when it ended quietly. It
// returns the part of err that classify must still consider.
func (l *lifecycle) hudEnded(err error, early bool) error {
	if l.shuttingDown.Load() || l.parent.Err() != nil {
		return err
	}
	own := err != nil && !uierrors.IsCancellation(err)
	reason := err
	if !own {
		reason = errHUDClosed
		if early {
			reason = errHUDClosedEarly
		}
	}
	if !l.fail(reason) || !own {
		return err
	}
	return nil
}

// failed returns the first recorded failure, if any. Call it once the
// recording context is done, so the slot has been written.
func (l *lifecycle) failed() error {
	select {
	case err := <-l.failure:
		return err
	default:
		return nil
	}
}

// stopOnSignal turns one HUD button press into a user stop. Its lifetime is
// bounded by ctx and the completion channel is joined by the caller.
func stopOnSignal(ctx context.Context, signal <-chan struct{}, stop context.CancelFunc) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
		case <-signal:
			stop()
		}
	}()
	return done
}

// ending gathers everything that explains how one recording ended.
type ending struct {
	runErr    error // core.Run, nil when it did not run
	hudErr    error // HUD result after it was stopped
	failure   error // first real failure recorded by the lifecycle
	parentErr error // parent context error at the end
	userStop  bool
}

// classify maps an ending to the error of the invocation. Real failures win
// over any cancellation, including ones joined with a cancellation; a bare
// user stop is success; a bare parent cancellation returns the parent's error.
func classify(e ending) error {
	var real []error
	if e.failure != nil {
		real = append(real, e.failure)
	}
	for _, err := range []error{e.runErr, e.hudErr} {
		if err != nil && !uierrors.IsCancellation(err) {
			real = append(real, err)
		}
	}
	switch len(real) {
	case 0:
	case 1:
		return real[0]
	default:
		return errors.Join(real...)
	}
	switch {
	case e.userStop:
		return nil
	case e.parentErr != nil:
		return e.parentErr
	}
	return e.runErr
}

// savedFile reports whether path is a non-empty regular file. Call it only
// after the core reported success: the writer reserves the path exclusively
// and removes it when aborting, so a file there is this recording.
func savedFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}
