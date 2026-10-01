package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"

	"github.com/bnema/nefercap/internal/adapters/wayland"
	"github.com/bnema/nefercap/internal/core"
	portsmocks "github.com/bnema/nefercap/internal/mocks/ports"
	"github.com/bnema/nefercap/internal/ports"
)

// newTestLifecycle builds the coordinator over real contexts, as production does.
func newTestLifecycle(t *testing.T) (*lifecycle, context.Context, context.Context, context.CancelFunc) {
	t.Helper()
	parent, cancelParent := context.WithCancel(context.Background())
	t.Cleanup(cancelParent)
	recordCtx, cancelRecord := context.WithCancelCause(parent)
	t.Cleanup(func() { cancelRecord(nil) })
	hudCtx, cancelHUD := context.WithCancel(parent)
	t.Cleanup(cancelHUD)
	return newLifecycle(parent, cancelRecord, cancelHUD), recordCtx, hudCtx, cancelParent
}

func TestUserStopIsNotAFailure(t *testing.T) {
	life, recordCtx, hudCtx, _ := newTestLifecycle(t)
	life.userStop()
	if context.Cause(recordCtx) != errUserStop || hudCtx.Err() == nil {
		t.Fatalf("stop did not end recording and HUD: %v", context.Cause(recordCtx))
	}
	// The HUD teardown provokes a detach; it is an echo, not a failure.
	life.detached(errors.New("layer detached"))
	if err := life.failed(); err != nil {
		t.Fatalf("echo recorded as failure: %v", err)
	}
	// Stopped before the HUD was ready: the HUD only reports its cancellation.
	err := classify(ending{hudErr: context.Canceled, failure: life.failed(), parentErr: nil, userStop: context.Cause(recordCtx) == errUserStop})
	if err != nil {
		t.Fatalf("stop before ready: %v", err)
	}
}

func TestDetachAfterReadyKeepsRealError(t *testing.T) {
	life, recordCtx, hudCtx, _ := newTestLifecycle(t)
	detach := fmt.Errorf("compositor: %w", wayland.ErrExclusionUnavailable)
	life.detached(detach)
	if recordCtx.Err() == nil || hudCtx.Err() == nil {
		t.Fatal("detach did not stop recording and HUD")
	}
	// The core observes only the cancellation the detach caused.
	err := classify(ending{runErr: fmt.Errorf("capture first frame: %w", context.Canceled), failure: life.failed(), userStop: context.Cause(recordCtx) == errUserStop})
	if !errors.Is(err, wayland.ErrExclusionUnavailable) {
		t.Fatalf("detach error lost: %v", err)
	}
	// A second detach does not replace the first cause.
	life.detached(errors.New("later"))
	if cause := context.Cause(recordCtx); !errors.Is(cause, wayland.ErrExclusionUnavailable) {
		t.Fatalf("first cause replaced: %v", cause)
	}
}

func TestDetachDuringOwnShutdownIsIgnored(t *testing.T) {
	life, recordCtx, _, _ := newTestLifecycle(t)
	life.beginShutdown()
	life.detached(errors.New("late detach"))
	if err := life.failed(); err != nil || recordCtx.Err() != nil {
		t.Fatalf("late detach acted on: %v %v", err, recordCtx.Err())
	}
}

func TestDetachAfterParentCancelIsIgnored(t *testing.T) {
	life, _, _, cancelParent := newTestLifecycle(t)
	cancelParent()
	life.detached(errors.New("echo"))
	if err := life.failed(); err != nil {
		t.Fatalf("echo of parent cancel recorded: %v", err)
	}
}

func TestHUDEndedBeforeReady(t *testing.T) {
	// Closed with nothing real: a failure naming the missing authorization.
	life, _, _, _ := newTestLifecycle(t)
	life.hudEnded(nil, true)
	if err := classify(ending{failure: life.failed()}); !errors.Is(err, errHUDClosedEarly) {
		t.Fatalf("silent early close: %v", err)
	}
	// A real startup error is returned as itself, not masked by a generic one.
	life, _, _, _ = newTestLifecycle(t)
	real := errors.New("authorize refused")
	rest := life.hudEnded(real, true)
	err := classify(ending{hudErr: rest, failure: life.failed()})
	if !errors.Is(err, real) || errors.Is(err, errHUDClosedEarly) {
		t.Fatalf("startup error masked: %v", err)
	}
}

func TestHUDEndedIgnoredOnceStopping(t *testing.T) {
	life, _, _, _ := newTestLifecycle(t)
	life.userStop()
	life.hudEnded(context.Canceled, false)
	if err := life.failed(); err != nil {
		t.Fatalf("stopped HUD counted as failure: %v", err)
	}
}

func TestParentCancelBeforeReadyWithNilHUD(t *testing.T) {
	life, _, _, cancelParent := newTestLifecycle(t)
	cancelParent()
	// The HUD ended cleanly (nil) because the parent cancelled it.
	life.hudEnded(nil, true)
	err := classify(ending{failure: life.failed(), parentErr: context.Canceled})
	if err != context.Canceled {
		t.Fatalf("want bare context.Canceled, got %v", err)
	}
}

func TestStopOnSignalStopsOnce(t *testing.T) {
	life, recordCtx, _, _ := newTestLifecycle(t)
	signal := make(chan struct{}, 1)
	watchCtx, stopWatch := context.WithCancel(context.Background())
	defer stopWatch()
	done := stopOnSignal(watchCtx, signal, life.userStop)
	signal <- struct{}{}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("signal watcher did not finish")
	}
	if context.Cause(recordCtx) != errUserStop {
		t.Fatalf("cause: %v", context.Cause(recordCtx))
	}
}

func TestClassify(t *testing.T) {
	boom := errors.New("boom")
	cancelled := fmt.Errorf("capture first frame: %w", context.Canceled)
	tests := []struct {
		name string
		in   ending
		want error
		is   error
	}{
		{name: "user stop before first frame", in: ending{runErr: cancelled, userStop: true}},
		{name: "user stop after frames", in: ending{userStop: true}},
		{name: "parent cancel returns bare error", in: ending{runErr: cancelled, parentErr: context.Canceled}, want: context.Canceled},
		{name: "cancel plus encoder failure", in: ending{runErr: errors.Join(cancelled, fmt.Errorf("finalize video: %w", boom)), userStop: true}, is: boom},
		{name: "HUD failure with user stop", in: ending{runErr: cancelled, hudErr: boom, userStop: true}, is: boom},
		{name: "recorded failure beats parent cancel", in: ending{runErr: cancelled, failure: boom, parentErr: context.Canceled}, is: boom},
		{name: "both failures kept", in: ending{runErr: boom, failure: errHUDClosed}, is: errHUDClosed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := classify(tt.in)
			switch {
			case tt.is != nil:
				if !errors.Is(err, tt.is) {
					t.Fatalf("got %v, want it to wrap %v", err, tt.is)
				}
			case err != tt.want:
				t.Fatalf("got %v, want %v", err, tt.want)
			}
		})
	}
}

func TestExclusionError(t *testing.T) {
	live := context.Background()
	done, cancel := context.WithCancel(context.Background())
	cancel()
	if err := exclusionError(done, fmt.Errorf("begin: %w", context.Canceled)); err != context.Canceled {
		t.Fatalf("cancellation not bare: %v", err)
	}
	real := errors.New("protocol failure")
	if err := exclusionError(done, errors.Join(context.Canceled, real)); !errors.Is(err, real) || err == context.Canceled {
		t.Fatalf("real failure masked by cancel: %v", err)
	}
	// A cancel error while the caller is live is not the caller's cancel.
	if err := exclusionError(live, context.Canceled); err == context.Canceled {
		t.Fatal("foreign cancel treated as caller's")
	}
	if err := exclusionError(live, real); !errors.Is(err, real) || err == real {
		t.Fatalf("phase missing: %v", err)
	}
}

func TestAuthorized(t *testing.T) {
	ready := make(chan struct{}, 1)
	if authorized(ready) {
		t.Fatal("authorized before ready")
	}
	ready <- struct{}{}
	if !authorized(ready) {
		t.Fatal("ready not seen")
	}
}

// A HUD that was authorized and then ended quietly is a plain close, not an
// early close.
func TestHUDEndedAfterAuthorization(t *testing.T) {
	life, _, _, _ := newTestLifecycle(t)
	ready := make(chan struct{}, 1)
	ready <- struct{}{}
	life.hudEnded(nil, !authorized(ready))
	if err := classify(ending{failure: life.failed()}); !errors.Is(err, errHUDClosed) || errors.Is(err, errHUDClosedEarly) {
		t.Fatalf("got %v", err)
	}
}

func TestFinishCapture(t *testing.T) {
	detach := errors.New("layer detached")
	var out bytes.Buffer
	if err := finishCapture(&out, "/v/a.mp4", true, detach); err != detach || out.String() != "/v/a.mp4\n" {
		t.Fatalf("saved with failure: %v %q", err, out.String())
	}
	out.Reset()
	if err := finishCapture(&out, "/v/a.mp4", true, nil); err != nil || out.String() != "/v/a.mp4\n" {
		t.Fatalf("saved: %v %q", err, out.String())
	}
	out.Reset()
	if err := finishCapture(&out, "/v/a.mp4", false, context.Canceled); err != context.Canceled || out.Len() != 0 {
		t.Fatalf("unsaved: %v %q", err, out.String())
	}
	if err := finishCapture(&out, "/v/a.mp4", false, nil); err != nil || out.Len() != 0 {
		t.Fatalf("stopped early: %v %q", err, out.String())
	}
	// A read-only file is a real writer that always fails.
	readOnly, openErr := os.Open(os.DevNull)
	if openErr != nil {
		t.Fatal(openErr)
	}
	defer readOnly.Close()
	err := finishCapture(readOnly, "/v/a.mp4", true, detach)
	if !errors.Is(err, detach) || len(err.(interface{ Unwrap() []error }).Unwrap()) != 2 {
		t.Fatalf("joined error lost a part: %v", err)
	}
}

// The production core with generated ports: cancelling during the first
// capture aborts nothing and writes nothing, and the lifecycle calls it success.
func TestCancelDuringFirstFrameIsNotAFailure(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	target := ports.Target{OutputID: 1}
	source := portsmocks.NewMockSource(t)
	source.EXPECT().Capture(ctx, target).RunAndReturn(func(c context.Context, _ ports.Target) (ports.Frame, error) {
		cancel(errUserStop)
		return ports.Frame{}, c.Err()
	})
	video := portsmocks.NewMockVideoWriter(t)
	path := filepath.Join(t.TempDir(), "never.mp4")
	runErr := core.New(source, nil, video).Run(ctx, ports.Selection{Mode: ports.Record, Target: target, Path: path, Video: ports.VideoSettings{FPS: 30}})
	if runErr == nil {
		t.Fatal("core reported success without a frame")
	}
	if err := classify(ending{runErr: runErr, userStop: context.Cause(ctx) == errUserStop}); err != nil {
		t.Fatalf("first frame cancel is a failure: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected no recording file: %v", err)
	}
}

// A finalize failure that surfaces beside the cancellation must reach the user.
func TestEncoderFailureAfterCancelIsKept(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	frame := ports.Frame{Pixels: make([]byte, 4*2*2), Width: 2, Height: 2, Stride: 8, Format: ports.XRGB8888}
	target := ports.Target{OutputID: 1}
	source := portsmocks.NewMockSource(t)
	source.EXPECT().Capture(ctx, target).Return(frame, nil)
	closeErr := errors.New("encoder exited 1")
	video := portsmocks.NewMockVideoWriter(t)
	video.EXPECT().Start(ctx, frame, mock.Anything, mock.Anything).Return(nil)
	video.EXPECT().Write(ctx, frame).RunAndReturn(func(context.Context, ports.Frame) error {
		cancel(errUserStop)
		return nil
	})
	video.EXPECT().Close().Return(closeErr)
	runErr := core.New(source, nil, video).Run(ctx, ports.Selection{Mode: ports.Record, Target: target, Path: filepath.Join(t.TempDir(), "x.mp4"), Video: ports.VideoSettings{FPS: 30}})
	err := classify(ending{runErr: runErr, userStop: context.Cause(ctx) == errUserStop})
	if !errors.Is(err, closeErr) {
		t.Fatalf("encoder failure masked: %v", err)
	}
}
