package wayland

import (
	"context"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bnema/nefercap/internal/adapters/wayland/testserver"
	"github.com/bnema/nefercap/internal/ports"
)

func newSource(t testing.TB, cfg testserver.Config) (*Source, *testserver.Server) {
	t.Helper()
	srv := testserver.Start(t, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := New(ctx, srv.Path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s, srv
}

func firstOutput(t testing.TB, s *Source) ports.Output {
	t.Helper()
	outs, err := s.Outputs(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, outs)
	return outs[0]
}

func fdCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	require.NoError(t, err)
	return len(entries)
}

func TestOutputs(t *testing.T) {
	s, _ := newSource(t, testserver.Config{Outputs: []testserver.OutputSpec{
		{Name: "A-1", Width: 8, Height: 4, Scale: 1}, {Name: "B-2", Width: 16, Height: 8, Scale: 2},
	}})
	outs, err := s.Outputs(context.Background())
	require.NoError(t, err)
	require.Len(t, outs, 2)
	require.Equal(t, "A-1", outs[0].Name)
	require.Equal(t, 16, outs[1].Width)
	require.Equal(t, 2, outs[1].Scale)
}

func TestCapabilitiesBaseline(t *testing.T) {
	s, _ := newSource(t, testserver.Config{})
	require.Equal(t, ports.Capabilities{}, s.Capabilities())
}

func TestCaptureFullOutput(t *testing.T) {
	s, srv := newSource(t, testserver.Config{})
	out := firstOutput(t, s)
	f, err := s.Capture(context.Background(), ports.Target{OutputID: out.ID})
	require.NoError(t, err)
	require.NoError(t, f.Validate())
	require.Equal(t, [3]int{8, 4, 32}, [3]int{f.Width, f.Height, f.Stride})
	require.Equal(t, ports.XRGB8888, f.Format)
	want := testserver.Pixel(3, 2, 1)
	require.Equal(t, want[:], f.Row(2)[12:16])
	require.Equal(t, "output", srv.Stats().LastSource)
}

func TestCapturePicksSupportedFormat(t *testing.T) {
	s, _ := newSource(t, testserver.Config{Formats: []uint32{0x34325258 /* XR24 big */, testserver.ARGB8888}})
	f, err := s.Capture(context.Background(), ports.Target{OutputID: firstOutput(t, s).ID})
	require.NoError(t, err)
	require.Equal(t, ports.ARGB8888, f.Format)
}

func TestCaptureUnsupportedFormat(t *testing.T) {
	s, _ := newSource(t, testserver.Config{Formats: []uint32{0x34325258}})
	_, err := s.Capture(context.Background(), ports.Target{OutputID: firstOutput(t, s).ID})
	require.ErrorIs(t, err, ports.ErrUnsupportedFormat)
	_, err = s.Capture(context.Background(), ports.Target{OutputID: firstOutput(t, s).ID})
	require.ErrorIs(t, err, ports.ErrUnsupportedFormat, "a negotiation failure leaves the Source usable")
}

func TestCaptureNoShmFormat(t *testing.T) {
	s, _ := newSource(t, testserver.Config{NoShmFormat: true})
	_, err := s.Capture(context.Background(), ports.Target{OutputID: firstOutput(t, s).ID})
	require.ErrorIs(t, err, ports.ErrUnsupportedFormat)
}

func TestCaptureUnknownOutputAndInvalidRegion(t *testing.T) {
	s, _ := newSource(t, testserver.Config{})
	_, err := s.Capture(context.Background(), ports.Target{OutputID: 999})
	require.ErrorIs(t, err, ports.ErrOutputNotFound)
	out := firstOutput(t, s)
	for _, r := range []ports.Region{{X: -1, Width: 1, Height: 1}, {Width: 0, Height: 1, X: 1}, {Width: 1, Height: -1}, {Width: ports.MaxDimension + 1, Height: 1}} {
		_, err = s.Capture(context.Background(), ports.Target{OutputID: out.ID, Region: r})
		require.ErrorIs(t, err, ports.ErrInvalidRegion, "%+v", r)
	}
}

func TestMissingCopyCapture(t *testing.T) {
	srv := testserver.Start(t, testserver.Config{NoCopyCapture: true})
	_, err := New(context.Background(), srv.Path)
	require.ErrorContains(t, err, "ext-image-copy-capture")
}

func TestOutputRemovedDuringCapture(t *testing.T) {
	s, srv := newSource(t, testserver.Config{Hold: true})
	out := firstOutput(t, s)
	errc := make(chan error, 1)
	go func() { _, err := s.Capture(context.Background(), ports.Target{OutputID: out.ID}); errc <- err }()
	require.Eventually(t, func() bool { return srv.Held() == 1 }, 5*time.Second, time.Millisecond)
	srv.RemoveOutput("TEST-1")
	srv.Release()
	require.ErrorIs(t, <-errc, ports.ErrOutputNotFound)
	_, err := s.Capture(context.Background(), ports.Target{OutputID: out.ID})
	require.ErrorIs(t, err, ports.ErrOutputNotFound)
}

func TestSessionStoppedEndsCapture(t *testing.T) {
	s, srv := newSource(t, testserver.Config{})
	target := ports.Target{OutputID: firstOutput(t, s).ID}
	_, err := s.Capture(context.Background(), target)
	require.NoError(t, err)
	srv.StopSessions()
	_, err = s.Capture(context.Background(), target)
	require.ErrorIs(t, err, ports.ErrCaptureStopped)
	_, err = s.Capture(context.Background(), target)
	require.NoError(t, err, "a stopped session is replaced by the next capture")
}

func TestFailedStoppedReasonEndsCapture(t *testing.T) {
	s, srv := newSource(t, testserver.Config{})
	target := ports.Target{OutputID: firstOutput(t, s).ID}
	srv.Fail(1, 2)
	_, err := s.Capture(context.Background(), target)
	require.ErrorIs(t, err, ports.ErrCaptureStopped)
}

func TestFailedUnknownRetries(t *testing.T) {
	s, srv := newSource(t, testserver.Config{})
	target := ports.Target{OutputID: firstOutput(t, s).ID}
	srv.Fail(2, 0)
	_, err := s.Capture(context.Background(), target)
	require.NoError(t, err, "recoverable failures are retried")
	s, srv = newSource(t, testserver.Config{})
	srv.Fail(maxFrameAttempts, 0)
	_, err = s.Capture(context.Background(), target)
	require.ErrorIs(t, err, errCompositorFailedCapture, "retries without a last frame are bounded")
	srv.Fail(0, 0)
	_, err = s.Capture(context.Background(), target)
	require.NoError(t, err, "and the Source stays usable")
}

// With a last frame, a transient unknown failure repeats it, like a pending
// capture does; the failures are counted across calls and stay bounded.
func TestFailedUnknownRepeatsLastFrame(t *testing.T) {
	s, srv := newSource(t, testserver.Config{})
	target := ports.Target{OutputID: firstOutput(t, s).ID}
	first, err := s.Capture(context.Background(), target)
	require.NoError(t, err)
	want := append([]byte(nil), first.Pixels...)
	srv.Fail(1, 0)
	repeated, err := s.Capture(context.Background(), target)
	require.NoError(t, err, "a transient unknown failure repeats the last frame")
	require.Equal(t, want, repeated.Pixels)
	fresh, err := s.Capture(context.Background(), target)
	require.NoError(t, err, "recording continues")
	require.NotEqual(t, want, fresh.Pixels, "with a new frame")

	srv.Fail(maxFrameAttempts, 0)
	// The capture started after the previous frame was answered before Fail.
	_, err = s.Capture(context.Background(), target)
	require.NoError(t, err)
	for range maxFrameAttempts - 1 {
		_, err = s.Capture(context.Background(), target)
		require.NoError(t, err)
	}
	_, err = s.Capture(context.Background(), target)
	require.ErrorIs(t, err, errCompositorFailedCapture, "the failure count is kept across calls")
	srv.Fail(0, 0)
	_, err = s.Capture(context.Background(), target)
	require.NoError(t, err, "and the Source stays usable")
}

// A session the compositor stops before its first frame is a refusal: the
// error says so, and it is not retried with a second session.
func TestSessionStoppedBeforeFirstFrameIsRefusal(t *testing.T) {
	s, srv := newSource(t, testserver.Config{StopAtCreate: true})
	target := ports.Target{OutputID: firstOutput(t, s).ID}
	_, err := s.Capture(context.Background(), target)
	require.ErrorIs(t, err, ports.ErrCaptureStopped)
	require.ErrorContains(t, err, "compositor refused capture")
	require.ErrorContains(t, err, "/etc/neferwl/capture-allow")
	require.Equal(t, 1, srv.Stats().Sessions, "no second session within the call")
}

// A stop after frames were served is a plain stop, not a refusal.
func TestSessionStoppedAfterFrameIsNotRefusal(t *testing.T) {
	s, srv := newSource(t, testserver.Config{})
	target := ports.Target{OutputID: firstOutput(t, s).ID}
	_, err := s.Capture(context.Background(), target)
	require.NoError(t, err)
	srv.StopSessions()
	_, err = s.Capture(context.Background(), target)
	require.ErrorIs(t, err, ports.ErrCaptureStopped)
	require.NotContains(t, err.Error(), "refused")
}

func TestFailedBufferConstraintsRenegotiates(t *testing.T) {
	s, srv := newSource(t, testserver.Config{})
	out := firstOutput(t, s)
	target := ports.Target{OutputID: out.ID}
	_, err := s.Capture(context.Background(), target)
	require.NoError(t, err)
	// The constraints change while no frame is in flight; the next frame is
	// created with a fresh buffer of the new size.
	srv.Resize("TEST-1", 16, 8)
	var f ports.Frame
	require.Eventually(t, func() bool {
		var err error
		f, err = s.Capture(context.Background(), target)
		require.NoError(t, err)
		return f.Width == 16
	}, 5*time.Second, time.Millisecond)
	require.Equal(t, [2]int{16, 8}, [2]int{f.Width, f.Height})
}

// After a layout change the last frame no longer fits: a failed(unknown)
// cannot repeat it. The frame is retried, bounded, and never the stale one.
func TestFailedUnknownAfterLayoutChangeRetriesBounded(t *testing.T) {
	s, srv := newSource(t, testserver.Config{})
	target := ports.Target{OutputID: firstOutput(t, s).ID}
	_, err := s.Capture(context.Background(), target)
	require.NoError(t, err)
	srv.Resize("TEST-1", 16, 8)
	_ = firstOutput(t, s) // a roundtrip: the session's new constraints are read
	srv.Fail(2, 0)
	f, err := s.Capture(context.Background(), target)
	require.NoError(t, err, "recoverable failures are retried")
	require.Equal(t, [2]int{16, 8}, [2]int{f.Width, f.Height}, "never the repeated 8x4 frame")

	srv.Resize("TEST-1", 32, 16)
	_ = firstOutput(t, s)
	srv.Fail(maxFrameAttempts, 0)
	_, err = s.Capture(context.Background(), target)
	require.ErrorIs(t, err, errCompositorFailedCapture, "the retries are bounded")
	srv.Fail(0, 0)
	f, err = s.Capture(context.Background(), target)
	require.NoError(t, err, "and the Source stays usable")
	require.Equal(t, [2]int{32, 16}, [2]int{f.Width, f.Height})
}

func TestConstraintsChangeWhileFrameInFlight(t *testing.T) {
	s, srv := newSource(t, testserver.Config{Hold: true})
	target := ports.Target{OutputID: firstOutput(t, s).ID}
	type result struct {
		f   ports.Frame
		err error
	}
	res := make(chan result, 1)
	go func() { f, err := s.Capture(context.Background(), target); res <- result{f, err} }()
	require.Eventually(t, func() bool { return srv.Held() == 1 }, 5*time.Second, time.Millisecond)
	srv.Resize("TEST-1", 16, 8) // the held 8x4 frame now mismatches: failed(buffer_constraints)
	srv.Release()
	require.Eventually(t, func() bool { return srv.Held() == 1 }, 5*time.Second, time.Millisecond, "the retry uses the new size")
	srv.Release()
	r := <-res
	require.NoError(t, r.err)
	require.Equal(t, [2]int{16, 8}, [2]int{r.f.Width, r.f.Height})
}

func TestReuseAndBuffers(t *testing.T) {
	s, srv := newSource(t, testserver.Config{})
	target := ports.Target{OutputID: firstOutput(t, s).ID}
	for range 5 {
		_, err := s.Capture(context.Background(), target)
		require.NoError(t, err)
	}
	st := srv.Stats()
	require.Equal(t, 2, st.Buffers, "two shm buffers alternate: the last frame stays readable while the next is written")
	require.Equal(t, 1, st.Sessions, "one session serves consecutive frames")
	require.GreaterOrEqual(t, st.Ready, 5)
	srv.Resize("TEST-1", 16, 8)
	var f ports.Frame
	require.Eventually(t, func() bool { // a resize is seen by the next calls, never mid-poll
		var err error
		f, err = s.Capture(context.Background(), target)
		require.NoError(t, err)
		return f.Width == 16
	}, 5*time.Second, time.Millisecond)
	st = srv.Stats()
	require.GreaterOrEqual(t, st.Buffers, 3)
	require.GreaterOrEqual(t, st.DestroyedBuffers, 1, "a buffer of the old size is destroyed")
}

func TestOneFrameInFlight(t *testing.T) {
	// The server raises duplicate_frame when a frame is created before the
	// previous one was destroyed, so successful back to back captures prove it.
	s, _ := newSource(t, testserver.Config{})
	target := ports.Target{OutputID: firstOutput(t, s).ID}
	for range 20 {
		_, err := s.Capture(context.Background(), target)
		require.NoError(t, err)
	}
}

func TestNoDescriptorLeak(t *testing.T) {
	s, _ := newSource(t, testserver.Config{})
	target := ports.Target{OutputID: firstOutput(t, s).ID}
	_, err := s.Capture(context.Background(), target)
	require.NoError(t, err)
	before := fdCount(t)
	for range 50 {
		_, err = s.Capture(context.Background(), target)
		require.NoError(t, err)
	}
	require.LessOrEqual(t, fdCount(t), before+1)
}

func TestCaptureWaitsForReady(t *testing.T) {
	s, srv := newSource(t, testserver.Config{Hold: true})
	target := ports.Target{OutputID: firstOutput(t, s).ID}
	done := make(chan error, 1)
	go func() { _, err := s.Capture(context.Background(), target); done <- err }()
	require.Eventually(t, func() bool { return srv.Held() == 1 }, 5*time.Second, time.Millisecond)
	select {
	case <-done:
		t.Fatal("capture returned before the compositor completed the copy")
	case <-time.After(50 * time.Millisecond):
	}
	srv.Release()
	require.NoError(t, <-done)
}

func TestCancelInterruptsBlockedCapture(t *testing.T) {
	s, srv := newSource(t, testserver.Config{Hold: true})
	target := ports.Target{OutputID: firstOutput(t, s).ID}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := s.Capture(ctx, target); done <- err }()
	require.Eventually(t, func() bool { return srv.Held() == 1 }, 5*time.Second, time.Millisecond)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	_, err := s.Capture(context.Background(), target)
	require.ErrorIs(t, err, ports.ErrClosed)
}

func TestCloseInterruptsBlockedCapture(t *testing.T) {
	s, srv := newSource(t, testserver.Config{Hold: true})
	target := ports.Target{OutputID: firstOutput(t, s).ID}
	done := make(chan error, 1)
	go func() { _, err := s.Capture(context.Background(), target); done <- err }()
	require.Eventually(t, func() bool { return srv.Held() == 1 }, 5*time.Second, time.Millisecond)
	require.NoError(t, s.Close())
	require.ErrorIs(t, <-done, ports.ErrClosed)
	require.NoError(t, s.Close(), "idempotent")
}

func TestCancelledContextBeforeCall(t *testing.T) {
	s, _ := newSource(t, testserver.Config{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.Capture(ctx, ports.Target{OutputID: firstOutput(t, s).ID})
	require.ErrorIs(t, err, context.Canceled)
}

func TestConstructorContextIsScopedToConstructor(t *testing.T) {
	srv := testserver.Start(t, testserver.Config{})
	ctx, cancel := context.WithCancel(context.Background())
	s, err := New(ctx, srv.Path)
	require.NoError(t, err)
	defer s.Close()
	cancel()
	_, err = s.Capture(context.Background(), ports.Target{OutputID: firstOutput(t, s).ID})
	require.NoError(t, err)
}

func TestConstructorConnectFailure(t *testing.T) {
	_, err := New(context.Background(), t.TempDir()+"/none")
	require.Error(t, err)
}

func TestIdleCancellationDoesNotKillSource(t *testing.T) {
	s, _ := newSource(t, testserver.Config{})
	target := ports.Target{OutputID: firstOutput(t, s).ID}
	ctx, cancel := context.WithCancel(context.Background())
	_, err := s.Capture(ctx, target)
	require.NoError(t, err)
	_, err = s.Capture(ctx, target) // same context: the watcher stays registered
	require.NoError(t, err)
	cancel()
	_, err = s.Capture(context.Background(), target)
	require.NoError(t, err, "a cancelled context affects only calls given that context")
}

func TestConcurrentCallsAreSerialized(t *testing.T) {
	s, _ := newSource(t, testserver.Config{})
	target := ports.Target{OutputID: firstOutput(t, s).ID}
	errc := make(chan error, 8)
	for range 8 {
		go func() { _, err := s.Capture(context.Background(), target); errc <- err }()
	}
	for range 8 {
		require.NoError(t, <-errc)
	}
}

// allocBudget is measured (see the log of TestCaptureAllocations), with
// headroom.
const allocBudget = 20

// Steady state cost of one capture over the real transport. The measurement
// includes the in-process libwayland server, so it is an upper bound for the
// Source alone; the budget is set from measured values (see the benchmark).
func TestCaptureAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("race instrumentation changes allocation counts")
	}
	s, _ := newSource(t, testserver.Config{Outputs: []testserver.OutputSpec{{Name: "X", Width: 64, Height: 64, Scale: 1}}})
	target := ports.Target{OutputID: firstOutput(t, s).ID}
	ctx := context.Background()
	for range 3 { // warm pools and buffers
		_, err := s.Capture(ctx, target)
		require.NoError(t, err)
	}
	runtime.GC()
	avg := testing.AllocsPerRun(200, func() {
		if _, err := s.Capture(ctx, target); err != nil {
			panic(err)
		}
	})
	t.Logf("allocations per capture (client and in-process server): %g", avg)
	require.LessOrEqual(t, avg, float64(allocBudget))
}

func BenchmarkCapture(b *testing.B) {
	s, _ := newSource(b, testserver.Config{Outputs: []testserver.OutputSpec{{Name: "BENCH", Width: 1920, Height: 1080, Scale: 1}}})
	target := ports.Target{OutputID: firstOutput(b, s).ID}
	ctx := context.Background()
	b.ReportAllocs()
	b.SetBytes(1920 * 1080 * 4)
	b.ResetTimer()
	for b.Loop() {
		if _, err := s.Capture(ctx, target); err != nil {
			b.Fatal(err)
		}
	}
}

// Opt-in: NEFERCAP_WAYLAND_SOCKET names the socket of a disposable headless
// compositor that supports ext-image-copy-capture. Never point it at a real
// session.
func TestHeadlessCompositor(t *testing.T) {
	socket := os.Getenv("NEFERCAP_WAYLAND_SOCKET")
	if socket == "" {
		t.Skip("NEFERCAP_WAYLAND_SOCKET not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := New(ctx, socket)
	require.NoError(t, err)
	defer s.Close()
	out := firstOutput(t, s)
	for range 3 {
		f, err := s.Capture(context.Background(), ports.Target{OutputID: out.ID})
		require.NoError(t, err)
		require.NoError(t, f.Validate())
	}
	f, err := s.Capture(context.Background(), ports.Target{OutputID: out.ID, Region: ports.Region{Width: 16, Height: 8}})
	require.NoError(t, err)
	require.Equal(t, [2]int{16, 8}, [2]int{f.Width, f.Height})
}

// A compositor that waits for damage holds every frame after the first. The
// capture stays pending, at most one frame is in flight, and Capture returns
// the last frame at once instead of blocking a timed recording.
func TestStaticScreenRepeatsLastFrame(t *testing.T) {
	s, srv := newSource(t, testserver.Config{HoldAfterFirst: true})
	target := ports.Target{OutputID: firstOutput(t, s).ID}
	first, err := s.Capture(context.Background(), target)
	require.NoError(t, err)
	want := testserver.Pixel(2, 1, 1)
	require.Equal(t, want[:], first.Row(1)[8:12])
	for range 5 {
		began := time.Now()
		f, err := s.Capture(context.Background(), target)
		require.NoError(t, err)
		require.Less(t, time.Since(began), time.Second, "a pending capture does not block the caller")
		require.Equal(t, want[:], f.Row(1)[8:12], "the last frame is repeated")
	}
	require.Eventually(t, func() bool { return srv.Held() == 1 }, 5*time.Second, time.Millisecond)
	st := srv.Stats()
	require.Equal(t, 2, st.Frames, "one served frame and one pending: a single frame in flight")
	srv.Release() // the screen changed
	var f ports.Frame
	require.Eventually(t, func() bool {
		f, err = s.Capture(context.Background(), target)
		require.NoError(t, err)
		return f.Row(1)[10] == 2
	}, 5*time.Second, time.Millisecond, "the pending capture completes into the other buffer")
}

// A timed recording over a static screen keeps its schedule.
func TestTimedRecordingOverStaticScreen(t *testing.T) {
	s, _ := newSource(t, testserver.Config{HoldAfterFirst: true})
	target := ports.Target{OutputID: firstOutput(t, s).ID}
	_, err := s.Capture(context.Background(), target)
	require.NoError(t, err)
	began := time.Now()
	for range 20 { // a 30 fps recording would wait 20 × 33 ms: each call must be far shorter
		_, err = s.Capture(context.Background(), target)
		require.NoError(t, err)
	}
	require.Less(t, time.Since(began), 500*time.Millisecond)
}

// NeferWL sends no new constraint batch after failed(buffer_constraints): the
// frame is retried with the current constraints after a roundtrip.
func TestFailedBufferConstraintsRetriesWithoutNewBatch(t *testing.T) {
	s, srv := newSource(t, testserver.Config{})
	target := ports.Target{OutputID: firstOutput(t, s).ID}
	srv.Fail(1, 1)
	_, err := s.Capture(context.Background(), target)
	require.NoError(t, err)
	srv.Fail(maxFrameAttempts, 1)
	_, err = s.Capture(context.Background(), target)
	require.ErrorIs(t, err, errCompositorFailedCapture, "the retries are bounded")
	srv.Fail(0, 0)
	_, err = s.Capture(context.Background(), target)
	require.NoError(t, err)
}

func TestNonNormalTransformIsRejected(t *testing.T) {
	for _, region := range []ports.Region{{}, {X: 1, Y: 1, Width: 2, Height: 2}} {
		s, _ := newSource(t, testserver.Config{Transform: 3})
		_, err := s.Capture(context.Background(), ports.Target{OutputID: firstOutput(t, s).ID, Region: region})
		require.ErrorIs(t, err, ports.ErrUnsupportedTransform)
		require.ErrorContains(t, err, "output transform 3 is not supported")
	}
}

// A workspace source stops when its workspace goes away.
func TestWorkspaceRemovedIsUnavailable(t *testing.T) {
	cfg := wsConfig(testserver.WorkspaceSpec{ID: "7f3a-2", Name: "b", Output: "TEST-1"})
	cfg.NeferwlSource = true
	s, srv := newSource(t, cfg)
	target := ports.Target{OutputID: firstOutput(t, s).ID, WorkspaceID: 1}
	_, err := s.Capture(context.Background(), target)
	require.NoError(t, err)
	srv.RemoveWorkspace("b")
	_, err = s.Capture(context.Background(), target)
	require.ErrorIs(t, err, ports.ErrWorkspaceUnavailable)
}

// A workspace session that stops because its workspace was removed is a plain
// stop, not a refusal: the allowlist hint is for a live workspace.
func TestWorkspaceRemovedBeforeFirstFrameIsNotRefusal(t *testing.T) {
	cfg := wsConfig(testserver.WorkspaceSpec{ID: "7f3a-2", Name: "b", Output: "TEST-1"})
	cfg.NeferwlSource = true
	s, srv := newSource(t, cfg)
	target := ports.Target{OutputID: firstOutput(t, s).ID, WorkspaceID: 1}
	srv.RemoveWorkspace("b") // not read yet: the session is created, then stopped
	_, err := s.Capture(context.Background(), target)
	require.ErrorIs(t, err, ports.ErrWorkspaceUnavailable)
	require.ErrorIs(t, err, ports.ErrCaptureStopped)
	require.NotContains(t, err.Error(), "capture-allow")
	require.NotContains(t, err.Error(), "refused")
}

// A live workspace whose session is refused keeps the allowlist hint.
func TestWorkspaceSessionRefusedKeepsAllowlistHint(t *testing.T) {
	cfg := wsConfig(testserver.WorkspaceSpec{ID: "7f3a-2", Name: "b", Output: "TEST-1"})
	cfg.NeferwlSource, cfg.StopAtCreate = true, true
	s, _ := newSource(t, cfg)
	_, err := s.Capture(context.Background(), ports.Target{OutputID: firstOutput(t, s).ID, WorkspaceID: 1})
	require.ErrorIs(t, err, ports.ErrCaptureStopped)
	require.ErrorContains(t, err, "/etc/neferwl/capture-allow")
}

// A cancellable context costs no more than a background one.
func TestCaptureAllocationsCancellable(t *testing.T) {
	if raceEnabled {
		t.Skip("race instrumentation changes allocation counts")
	}
	s, _ := newSource(t, testserver.Config{Outputs: []testserver.OutputSpec{{Name: "X", Width: 64, Height: 64, Scale: 1}}})
	target := ports.Target{OutputID: firstOutput(t, s).ID}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for range 3 {
		_, err := s.Capture(ctx, target)
		require.NoError(t, err)
	}
	runtime.GC()
	avg := testing.AllocsPerRun(200, func() {
		if _, err := s.Capture(ctx, target); err != nil {
			panic(err)
		}
	})
	t.Logf("allocations per capture with a cancellable context: %g", avg)
	require.LessOrEqual(t, avg, float64(allocBudget))
}

// A constraint batch that repeats the same layout (a compositor may re-send
// its constraints) must not drop the last frame or the pending capture: on a
// static screen the next Capture would block for damage that never comes.
func TestSameGeometryConstraintBatchKeepsPendingFrame(t *testing.T) {
	s, srv := newSource(t, testserver.Config{HoldAfterFirst: true})
	target := ports.Target{OutputID: firstOutput(t, s).ID}
	// Every call is bounded, so a regression fails instead of hanging.
	capture := func() (ports.Frame, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		return s.Capture(ctx, target)
	}
	for range 3 {
		_, err := capture()
		require.NoError(t, err)
	}
	srv.Resize("TEST-1", 8, 4) // same size: a new batch, same layout
	f, err := capture()
	require.NoError(t, err, "the last frame is repeated, not waited for")
	require.Equal(t, [2]int{8, 4}, [2]int{f.Width, f.Height})
	require.Equal(t, 2, srv.Stats().Frames, "the pending capture was kept, not replaced")
}
