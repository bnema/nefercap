package selection

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/bnema/nefercap/internal/adapters/uierrors"
	"github.com/bnema/nefercap/internal/core"
	"github.com/bnema/nefercap/internal/ports"
)

var errOverlayClosed = errors.New("selection: overlay closed by the compositor")

// session is the only state the per-output overlays share. Each overlay owns
// its model and window on its own goroutine; they talk to each other only
// through this first-decision-wins record and the shared cancel, never through
// window or picker state.
type session struct {
	cancel context.CancelFunc

	// toggles counts Tab presses on any overlay; wakes redraw every overlay
	// after one. wakes is set before the overlays start and never changes.
	toggles atomic.Uint32
	wakes   []chan struct{}

	mu       sync.Mutex
	decided  bool
	accepted bool
	result   core.PickResult
	mode     ports.Mode // the mode the confirming overlay showed
	errs     []error
}

func newSession(cancel context.CancelFunc) *session { return &session{cancel: cancel} }

// toggle flips the capture mode for every overlay and asks each to redraw.
// After the first decision it does nothing, so a late Tab on another overlay
// cannot change what was confirmed.
func (s *session) toggle() {
	s.mu.Lock()
	decided := s.decided
	if !decided {
		s.toggles.Add(1)
	}
	s.mu.Unlock()
	if decided {
		return
	}
	for _, w := range s.wakes {
		select {
		case w <- struct{}{}:
		default:
		}
	}
}

// accept records the first decision, with the mode its overlay showed, and
// ends every overlay.
func (s *session) accept(r core.PickResult, mode ports.Mode) {
	s.mu.Lock()
	if !s.decided {
		s.decided, s.accepted, s.result, s.mode = true, true, r, mode
	}
	s.mu.Unlock()
	s.cancel()
}

// abort records the first decision as "no selection" and ends every overlay.
func (s *session) abort() {
	s.mu.Lock()
	s.decided = true
	s.mu.Unlock()
	s.cancel()
}

// fail records a real error, which rejects the selection, and ends every overlay.
func (s *session) fail(err error) {
	s.mu.Lock()
	s.decided = true
	s.errs = append(s.errs, err)
	s.mu.Unlock()
	s.cancel()
}

func (s *session) isDecided() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.decided
}

// runEnded classifies one overlay's Run result with uierrors.Ended. The state
// it depends on (decided, parent error) is read once, so a context that ends
// meanwhile cannot flip the answer. An overlay that ended with no decision and
// a live parent was removed by the compositor, which is a failure, and any real
// error reported with it is kept. Failures name the output.
func (s *session) runEnded(parent context.Context, output string, runErr error) {
	perr := parent.Err()
	explained := perr != nil || s.isDecided()
	if err := uierrors.Ended(runErr, explained, perr, errOverlayClosed); err != nil {
		s.fail(fmt.Errorf("selection: output %s: %w", output, err))
	}
}

// outcome is read after every overlay has returned. The caller's context wins;
// then real errors; then the decision.
func (s *session) outcome(parent context.Context) (core.PickResult, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if perr := parent.Err(); perr != nil {
		return core.PickResult{}, false, errors.Join(append([]error{perr}, s.errs...)...)
	}
	if len(s.errs) > 0 {
		return core.PickResult{}, false, errors.Join(s.errs...)
	}
	return s.result, s.accepted, nil
}

// acceptedMode is the mode recorded by accept.
func (s *session) acceptedMode() ports.Mode {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mode
}
