package uierrors

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDrop(t *testing.T) {
	boom := errors.New("boom")
	other := errors.New("other")
	c := context.Canceled

	assert.NoError(t, Drop(nil, c))
	assert.NoError(t, Drop(c, c))
	assert.NoError(t, Drop(errors.Join(c, c), c))
	assert.Same(t, boom, Drop(errors.Join(c, boom), c), "a lone survivor is returned unwrapped")
	assert.Same(t, boom, Drop(boom, c))

	both := Drop(errors.Join(c, boom, other), c)
	assert.ErrorIs(t, both, boom)
	assert.ErrorIs(t, both, other)
	assert.NotErrorIs(t, both, c)

	nested := Drop(errors.Join(errors.Join(c, boom), c), c)
	assert.Same(t, boom, nested)
}

func TestDropKeepsWrappedTargets(t *testing.T) {
	wrapped := fmt.Errorf("surface close: %w", context.Canceled)
	got := Drop(errors.Join(context.Canceled, wrapped), context.Canceled)
	assert.Same(t, wrapped, got, "wrapping makes it a real failure")
}

func TestDropMultipleTargets(t *testing.T) {
	assert.NoError(t, Drop(errors.Join(context.Canceled, context.DeadlineExceeded), context.Canceled, context.DeadlineExceeded))
}

var errClosed = errors.New("closed")

// liveParent returns a parent context and its cancel, to classify with real
// context state rather than hand-built errors.
func liveParent() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

func TestEndedCleanAfterOwnDecision(t *testing.T) {
	parent, cancel := liveParent()
	defer cancel()
	// We decided and canceled the child: Run reports a bare Canceled.
	child, cancelChild := context.WithCancel(parent)
	cancelChild()
	assert.NoError(t, Ended(child.Err(), true, parent.Err(), errClosed))
	assert.NoError(t, Ended(nil, true, parent.Err(), errClosed))
}

func TestEndedCleanAfterCallerCancel(t *testing.T) {
	parent, cancel := liveParent()
	cancel()
	// The caller's context is done, which explains the end by itself.
	perr := parent.Err()
	require.Error(t, perr)
	assert.NoError(t, Ended(context.Canceled, true, perr, errClosed))
	assert.NoError(t, Ended(perr, true, perr, errClosed))
}

func TestEndedKeepsRealFailureJoinedWithCancel(t *testing.T) {
	boom := errors.New("boom")
	parent, cancel := liveParent()
	cancel()
	for _, explained := range []bool{true, false} {
		err := Ended(errors.Join(context.Canceled, boom), explained, parent.Err(), errClosed)
		assert.ErrorIs(t, err, boom, "explained=%v", explained)
	}
	wrapped := fmt.Errorf("surface close: %w", context.Canceled)
	err := Ended(errors.Join(context.Canceled, wrapped), true, nil, errClosed)
	assert.Same(t, wrapped, err, "a wrapped Canceled is a real failure")
}

func TestEndedUnexpectedCancelIsClosed(t *testing.T) {
	boom := errors.New("boom")
	// Nobody decided and the parent is live, yet Run ended.
	assert.ErrorIs(t, Ended(context.Canceled, false, nil, errClosed), errClosed)
	assert.ErrorIs(t, Ended(nil, false, nil, errClosed), errClosed)
	assert.NotErrorIs(t, Ended(context.Canceled, false, nil, errClosed), context.Canceled)

	// A real failure is reported as itself, never relabeled as a close.
	real := Ended(errors.Join(context.Canceled, boom), false, nil, errClosed)
	assert.Same(t, boom, real)
	assert.NotErrorIs(t, real, errClosed)
	assert.NotErrorIs(t, real, context.Canceled)
	assert.Same(t, boom, Ended(boom, false, nil, errClosed))
}

func TestIsCancellation(t *testing.T) {
	boom := errors.New("boom")
	c := context.Canceled
	assert.False(t, IsCancellation(nil))
	assert.True(t, IsCancellation(c))
	assert.True(t, IsCancellation(fmt.Errorf("capture first frame: %w", c)))
	assert.True(t, IsCancellation(errors.Join(c, fmt.Errorf("x: %w", c))))
	assert.False(t, IsCancellation(boom))
	assert.False(t, IsCancellation(errors.Join(c, boom)))
	assert.False(t, IsCancellation(fmt.Errorf("%w: %w", c, boom)))
	assert.False(t, IsCancellation(context.DeadlineExceeded))
	assert.False(t, IsCancellation(fmt.Errorf("finalize: %w", context.DeadlineExceeded)))
}
