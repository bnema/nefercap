package core_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/bnema/nefercap/internal/core"
	portsmocks "github.com/bnema/nefercap/internal/mocks/ports"
	"github.com/bnema/nefercap/internal/ports"
)

var (
	frame  = ports.Frame{Pixels: make([]byte, 16), Width: 2, Height: 2, Stride: 8, Format: XRGB}
	target = ports.Target{OutputID: 7, Region: ports.Region{X: 1, Y: 2, Width: 3, Height: 4}}
)

const XRGB = ports.XRGB8888

type harness struct {
	src *portsmocks.MockSource
	png *portsmocks.MockScreenshotWriter
	vid *portsmocks.MockVideoWriter
	svc *core.Service
}

func newHarness(t *testing.T) harness {
	t.Helper()
	h := harness{
		src: portsmocks.NewMockSource(t),
		png: portsmocks.NewMockScreenshotWriter(t),
		vid: portsmocks.NewMockVideoWriter(t),
	}
	h.svc = core.New(h.src, h.png, h.vid)
	return h
}

func shot() ports.Selection {
	return ports.Selection{Mode: ports.Screenshot, Target: target, Path: "a.png"}
}

func rec(d time.Duration) ports.Selection {
	return ports.Selection{
		Mode: ports.Record, Target: target, Path: "a.mkv", Duration: d,
		Video: ports.VideoSettings{FPS: 120},
	}
}

func TestInvalidSelectionsMakeNoCalls(t *testing.T) {
	const maxI32 = 1<<31 - 1
	cases := map[string]func(*ports.Selection){
		"empty mode":        func(s *ports.Selection) { s.Mode = "" },
		"unknown mode":      func(s *ports.Selection) { s.Mode = "gif" },
		"zero output":       func(s *ports.Selection) { s.Target.OutputID = 0 },
		"negative x":        func(s *ports.Selection) { s.Target.Region = ports.Region{X: -1, Width: 1, Height: 1} },
		"negative y":        func(s *ports.Selection) { s.Target.Region = ports.Region{Y: -1, Width: 1, Height: 1} },
		"zero width":        func(s *ports.Selection) { s.Target.Region = ports.Region{X: 1, Height: 1} },
		"zero height":       func(s *ports.Selection) { s.Target.Region = ports.Region{Width: 1} },
		"negative width":    func(s *ports.Selection) { s.Target.Region = ports.Region{Width: -1, Height: 1} },
		"x overflow":        func(s *ports.Selection) { s.Target.Region = ports.Region{X: maxI32, Width: 1, Height: 1} },
		"width overflow":    func(s *ports.Selection) { s.Target.Region = ports.Region{Width: maxI32 + 1, Height: 1} },
		"empty path":        func(s *ports.Selection) { s.Path = "" },
		"negative duration": func(s *ports.Selection) { s.Duration = -time.Second },
	}
	for name, mutate := range cases {
		for _, base := range []ports.Selection{shot(), rec(time.Second)} {
			t.Run(string(base.Mode)+"/"+name, func(t *testing.T) {
				h := newHarness(t)
				sel := base
				mutate(&sel)
				err := h.svc.Run(context.Background(), sel)
				require.ErrorIs(t, err, core.ErrInvalidSelection)
			})
		}
	}

	videos := map[string]ports.VideoSettings{
		"fps zero":       {},
		"fps negative":   {FPS: -1},
		"fps too high":   {FPS: 121},
		"width only":     {FPS: 30, Width: 640},
		"height only":    {FPS: 30, Height: 480},
		"odd width":      {FPS: 30, Width: 641, Height: 480},
		"odd height":     {FPS: 30, Width: 640, Height: 481},
		"negative size":  {FPS: 30, Width: -2, Height: -2},
		"oversized":      {FPS: 30, Width: 16386, Height: 2},
		"frame too big":  {FPS: 30, Width: 16384, Height: 16384},
		"oversized high": {FPS: 30, Width: 2, Height: 16386},
	}
	for name, v := range videos {
		t.Run("record/"+name, func(t *testing.T) {
			h := newHarness(t)
			sel := rec(0)
			sel.Video = v
			require.ErrorIs(t, h.svc.Run(context.Background(), sel), core.ErrInvalidSelection)
		})
	}
}

func TestRegionErrorIsInvalidRegion(t *testing.T) {
	h := newHarness(t)
	sel := shot()
	sel.Target.Region = ports.Region{X: -1, Width: 1, Height: 1}
	require.ErrorIs(t, h.svc.Run(context.Background(), sel), ports.ErrInvalidRegion)
}

func TestValidSelectionsAreAccepted(t *testing.T) {
	sizes := []ports.VideoSettings{{FPS: 1}, {FPS: 120, Width: 2, Height: 2}, {FPS: 30, Width: 16384, Height: 4096}}
	for _, v := range sizes {
		h := newHarness(t)
		h.src.EXPECT().Capture(mock.Anything, target).Return(frame, nil).Once()
		h.vid.EXPECT().Start(mock.Anything, frame, "a.mkv", v).Return(errors.New("stop")).Once()
		h.vid.EXPECT().Abort().Return(nil).Once()
		sel := rec(0)
		sel.Video = v
		require.ErrorContains(t, h.svc.Run(context.Background(), sel), "stop")
	}
	// Zero region means full output.
	h := newHarness(t)
	full := ports.Target{OutputID: 1}
	h.src.EXPECT().Capture(mock.Anything, full).Return(frame, nil).Once()
	h.png.EXPECT().Save(mock.Anything, frame, "a.png").Return(nil).Once()
	sel := shot()
	sel.Target = full
	require.NoError(t, h.svc.Run(context.Background(), sel))
}

func TestSelectionAllocations(t *testing.T) {
	sels := []ports.Selection{shot(), rec(time.Second)}
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // validation passes, then Run stops before any port call
	for _, sel := range sels {
		allocs := testing.AllocsPerRun(100, func() {
			if err := h.svc.Run(ctx, sel); !errors.Is(err, context.Canceled) {
				t.Fatalf("unexpected result %v", err)
			}
		})
		assert.Zero(t, allocs, sel.Mode)
	}
}

func TestScreenshot(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	c := h.src.EXPECT().Capture(ctx, target).Return(frame, nil).Once()
	h.png.EXPECT().Save(ctx, frame, "a.png").Return(nil).Once().NotBefore(c)
	require.NoError(t, h.svc.Run(ctx, shot()))
}

func TestScreenshotFailures(t *testing.T) {
	boom := errors.New("boom")
	t.Run("capture", func(t *testing.T) {
		h := newHarness(t)
		h.src.EXPECT().Capture(mock.Anything, target).Return(ports.Frame{}, boom).Once()
		require.ErrorIs(t, h.svc.Run(context.Background(), shot()), boom)
	})
	t.Run("invalid frame", func(t *testing.T) {
		h := newHarness(t)
		bad := frame
		bad.Stride = 4
		h.src.EXPECT().Capture(mock.Anything, target).Return(bad, nil).Once()
		require.Error(t, h.svc.Run(context.Background(), shot()))
	})
	t.Run("save", func(t *testing.T) {
		h := newHarness(t)
		h.src.EXPECT().Capture(mock.Anything, target).Return(frame, nil).Once()
		h.png.EXPECT().Save(mock.Anything, frame, "a.png").Return(errors.Join(boom, ports.ErrPathExists)).Once()
		err := h.svc.Run(context.Background(), shot())
		require.ErrorIs(t, err, boom)
		require.ErrorIs(t, err, ports.ErrPathExists)
	})
	t.Run("already cancelled", func(t *testing.T) {
		h := newHarness(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		require.ErrorIs(t, h.svc.Run(ctx, shot()), context.Canceled)
	})
	t.Run("cancelled during capture", func(t *testing.T) {
		h := newHarness(t)
		ctx, cancel := context.WithCancel(context.Background())
		h.src.EXPECT().Capture(ctx, target).RunAndReturn(func(ctx context.Context, _ ports.Target) (ports.Frame, error) {
			cancel()
			return ports.Frame{}, ctx.Err()
		}).Once()
		require.ErrorIs(t, h.svc.Run(ctx, shot()), context.Canceled)
	})
	t.Run("cancelled during save", func(t *testing.T) {
		h := newHarness(t)
		ctx, cancel := context.WithCancel(context.Background())
		h.src.EXPECT().Capture(ctx, target).Return(frame, nil).Once()
		h.png.EXPECT().Save(ctx, frame, "a.png").RunAndReturn(func(ctx context.Context, _ ports.Frame, _ string) error {
			cancel()
			return ctx.Err()
		}).Once()
		require.ErrorIs(t, h.svc.Run(ctx, shot()), context.Canceled)
	})
}

// recordOrder sets the mandatory prefix: Capture, Start, first Write.
func recordOrder(h harness, sel ports.Selection) (first *mock.Call) {
	c := h.src.EXPECT().Capture(mock.Anything, target).Return(frame, nil).Once()
	s := h.vid.EXPECT().Start(mock.Anything, frame, sel.Path, sel.Video).Return(nil).Once().NotBefore(c)
	return s
}

func TestRecordDuration(t *testing.T) {
	h := newHarness(t)
	sel := rec(60 * time.Millisecond)
	start := recordOrder(h, sel)
	var closed atomic.Bool
	var writes, inFlight atomic.Int32
	h.src.EXPECT().Capture(mock.Anything, target).RunAndReturn(func(context.Context, ports.Target) (ports.Frame, error) {
		require.False(t, closed.Load(), "capture after close")
		require.Equal(t, int32(1), inFlight.Add(1), "concurrent capture")
		defer inFlight.Add(-1)
		return frame, nil
	}).Maybe()
	w := h.vid.EXPECT().Write(mock.Anything, frame).RunAndReturn(func(context.Context, ports.Frame) error {
		require.False(t, closed.Load(), "write after close")
		require.Equal(t, int32(1), inFlight.Add(1), "concurrent write")
		defer inFlight.Add(-1)
		writes.Add(1)
		return nil
	}).NotBefore(start)
	w.Maybe()
	h.vid.EXPECT().Close().RunAndReturn(func() error {
		closed.Store(true)
		return nil
	}).Once()

	began := time.Now()
	require.NoError(t, h.svc.Run(context.Background(), sel))
	assert.GreaterOrEqual(t, time.Since(began), 60*time.Millisecond)
	assert.GreaterOrEqual(t, writes.Load(), int32(2))
	assert.True(t, closed.Load())
}

func TestRecordFirstWriteCompletesBeforeTick(t *testing.T) {
	// 1 fps: only the first frame is written in a short run.
	h := newHarness(t)
	sel := rec(30 * time.Millisecond)
	sel.Video.FPS = 1
	start := recordOrder(h, sel)
	w := h.vid.EXPECT().Write(mock.Anything, frame).Return(nil).Once().NotBefore(start)
	h.vid.EXPECT().Close().Return(nil).Once().NotBefore(w)
	require.NoError(t, h.svc.Run(context.Background(), sel))
}

func TestRecordCancelFinalizesGracefully(t *testing.T) {
	h := newHarness(t)
	sel := rec(0)
	ctx, cancel := context.WithCancel(context.Background())
	start := recordOrder(h, sel)
	h.src.EXPECT().Capture(mock.Anything, target).Return(frame, nil).Maybe()
	var n atomic.Int32
	w := h.vid.EXPECT().Write(mock.Anything, frame).RunAndReturn(func(context.Context, ports.Frame) error {
		if n.Add(1) == 3 {
			cancel()
		}
		return nil
	}).NotBefore(start)
	w.Maybe()
	h.vid.EXPECT().Close().Return(nil).Once()
	require.NoError(t, h.svc.Run(ctx, sel))
	assert.GreaterOrEqual(t, n.Load(), int32(3))
}

func TestRecordCancelInterruptsBlockedCapture(t *testing.T) {
	h := newHarness(t)
	sel := rec(0)
	ctx, cancel := context.WithCancel(context.Background())
	start := recordOrder(h, sel)
	w := h.vid.EXPECT().Write(mock.Anything, frame).Return(nil).Once().NotBefore(start)
	h.src.EXPECT().Capture(mock.Anything, target).RunAndReturn(func(ctx context.Context, _ ports.Target) (ports.Frame, error) {
		cancel()
		<-ctx.Done()
		return ports.Frame{}, ctx.Err()
	}).Once()
	h.vid.EXPECT().Close().Return(nil).Once().NotBefore(w)
	require.NoError(t, h.svc.Run(ctx, sel))
}

func TestRecordCancelDuringWrite(t *testing.T) {
	h := newHarness(t)
	sel := rec(0)
	ctx, cancel := context.WithCancel(context.Background())
	start := recordOrder(h, sel)
	w := h.vid.EXPECT().Write(mock.Anything, frame).RunAndReturn(func(ctx context.Context, _ ports.Frame) error {
		cancel()
		return ctx.Err()
	}).Once().NotBefore(start)
	h.vid.EXPECT().Close().Return(nil).Once().NotBefore(w)
	require.NoError(t, h.svc.Run(ctx, sel))
}

func TestRecordAlreadyCancelled(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, h.svc.Run(ctx, rec(0)), context.Canceled)
}

func TestRecordStartFailures(t *testing.T) {
	boom := errors.New("boom")
	abortErr := errors.New("abort failed")
	t.Run("first capture", func(t *testing.T) {
		h := newHarness(t)
		h.src.EXPECT().Capture(mock.Anything, target).Return(ports.Frame{}, boom).Once()
		require.ErrorIs(t, h.svc.Run(context.Background(), rec(0)), boom)
	})
	t.Run("invalid first frame", func(t *testing.T) {
		h := newHarness(t)
		h.src.EXPECT().Capture(mock.Anything, target).Return(ports.Frame{Width: 2}, nil).Once()
		require.Error(t, h.svc.Run(context.Background(), rec(0)))
	})
	t.Run("start with abort failure", func(t *testing.T) {
		h := newHarness(t)
		sel := rec(0)
		c := h.src.EXPECT().Capture(mock.Anything, target).Return(frame, nil).Once()
		s := h.vid.EXPECT().Start(mock.Anything, frame, sel.Path, sel.Video).Return(boom).Once().NotBefore(c)
		h.vid.EXPECT().Abort().Return(abortErr).Once().NotBefore(s)
		err := h.svc.Run(context.Background(), sel)
		require.ErrorIs(t, err, boom)
		require.ErrorIs(t, err, abortErr)
	})
	t.Run("path exists", func(t *testing.T) {
		h := newHarness(t)
		sel := rec(0)
		h.src.EXPECT().Capture(mock.Anything, target).Return(frame, nil).Once()
		h.vid.EXPECT().Start(mock.Anything, frame, sel.Path, sel.Video).Return(ports.ErrPathExists).Once()
		h.vid.EXPECT().Abort().Return(nil).Once()
		require.ErrorIs(t, h.svc.Run(context.Background(), sel), ports.ErrPathExists)
	})
}

func TestRecordRuntimeFailures(t *testing.T) {
	boom := errors.New("boom")
	closeErr := errors.New("close failed")

	t.Run("first write", func(t *testing.T) {
		h := newHarness(t)
		sel := rec(0)
		start := recordOrder(h, sel)
		w := h.vid.EXPECT().Write(mock.Anything, frame).Return(boom).Once().NotBefore(start)
		h.vid.EXPECT().Close().Return(closeErr).Once().NotBefore(w)
		err := h.svc.Run(context.Background(), sel)
		require.ErrorIs(t, err, boom)
		require.ErrorIs(t, err, closeErr)
	})
	t.Run("later capture", func(t *testing.T) {
		h := newHarness(t)
		sel := rec(0)
		start := recordOrder(h, sel)
		w := h.vid.EXPECT().Write(mock.Anything, frame).Return(nil).Once().NotBefore(start)
		h.src.EXPECT().Capture(mock.Anything, target).Return(ports.Frame{}, boom).Once()
		h.vid.EXPECT().Close().Return(nil).Once().NotBefore(w)
		err := h.svc.Run(context.Background(), sel)
		require.ErrorIs(t, err, boom)
		assert.NotErrorIs(t, err, closeErr)
	})
	t.Run("later invalid frame", func(t *testing.T) {
		h := newHarness(t)
		sel := rec(0)
		start := recordOrder(h, sel)
		w := h.vid.EXPECT().Write(mock.Anything, frame).Return(nil).Once().NotBefore(start)
		h.src.EXPECT().Capture(mock.Anything, target).Return(ports.Frame{}, nil).Once()
		h.vid.EXPECT().Close().Return(nil).Once().NotBefore(w)
		require.Error(t, h.svc.Run(context.Background(), sel))
	})
	t.Run("later capture and close", func(t *testing.T) {
		h := newHarness(t)
		sel := rec(0)
		start := recordOrder(h, sel)
		w := h.vid.EXPECT().Write(mock.Anything, frame).Return(nil).Once().NotBefore(start)
		h.src.EXPECT().Capture(mock.Anything, target).Return(ports.Frame{}, boom).Once()
		h.vid.EXPECT().Close().Return(closeErr).Once().NotBefore(w)
		err := h.svc.Run(context.Background(), sel)
		require.ErrorIs(t, err, boom)
		require.ErrorIs(t, err, closeErr)
	})
	t.Run("geometry change", func(t *testing.T) {
		h := newHarness(t)
		sel := rec(0)
		start := recordOrder(h, sel)
		first := h.vid.EXPECT().Write(mock.Anything, frame).Return(nil).Once().NotBefore(start)
		h.src.EXPECT().Capture(mock.Anything, target).Return(frame, nil).Once()
		h.vid.EXPECT().Write(mock.Anything, frame).Return(ports.ErrGeometryChanged).Once().NotBefore(first)
		h.vid.EXPECT().Close().Return(nil).Once()
		require.ErrorIs(t, h.svc.Run(context.Background(), sel), ports.ErrGeometryChanged)
	})
	t.Run("close only", func(t *testing.T) {
		h := newHarness(t)
		sel := rec(20 * time.Millisecond)
		start := recordOrder(h, sel)
		h.src.EXPECT().Capture(mock.Anything, target).Return(frame, nil).Maybe()
		h.vid.EXPECT().Write(mock.Anything, frame).Return(nil).NotBefore(start).Maybe()
		h.vid.EXPECT().Close().Return(closeErr).Once()
		require.ErrorIs(t, h.svc.Run(context.Background(), sel), closeErr)
	})
}
