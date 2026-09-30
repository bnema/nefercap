package wayland

import (
	"context"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bnema/nefercap/internal/ports"
)

func newSource(t *testing.T, cfg compositorConfig) (*Source, *compositor) {
	t.Helper()
	comp := startCompositor(t, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := New(ctx, comp.path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s, comp
}

func firstOutput(t *testing.T, s *Source) ports.Output {
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
	s, _ := newSource(t, compositorConfig{outputs: []outputSpec{
		{name: "DP-1", width: 8, height: 4, scale: 2, version: 4},
		{name: "ignored", width: 6, height: 2, scale: 1, version: 3},
	}})
	outs, err := s.Outputs(context.Background())
	require.NoError(t, err)
	require.Equal(t, []ports.Output{
		{ID: 1, Name: "DP-1", Width: 8, Height: 4, Scale: 2},
		{ID: 2, Name: "output-2", Width: 6, Height: 2, Scale: 1}, // no wl_output.name before version 4
	}, outs)
}

func TestOutputRemovedBetweenCalls(t *testing.T) {
	s, comp := newSource(t, compositorConfig{outputs: []outputSpec{
		{name: "A", width: 4, height: 4, scale: 1, version: 4},
		{name: "B", width: 4, height: 4, scale: 1, version: 4},
	}})
	comp.removeOutput(1)
	outs, err := s.Outputs(context.Background())
	require.NoError(t, err)
	require.Len(t, outs, 1)
	require.Equal(t, "B", outs[0].Name)
	_, err = s.Capture(context.Background(), ports.Target{OutputID: 1})
	require.ErrorIs(t, err, ports.ErrOutputNotFound)
	// Not finding an output is not a connection failure.
	_, err = s.Capture(context.Background(), ports.Target{OutputID: 2})
	require.NoError(t, err)
}

func TestOutputRemovedDuringCapture(t *testing.T) {
	s, _ := newSource(t, compositorConfig{removeOnCapture: true})
	out := firstOutput(t, s)
	_, err := s.Capture(context.Background(), ports.Target{OutputID: out.ID})
	require.ErrorIs(t, err, ports.ErrOutputNotFound)
	outs, err := s.Outputs(context.Background())
	require.NoError(t, err)
	require.Empty(t, outs)
}

func TestCaptureFullOutput(t *testing.T) {
	s, comp := newSource(t, compositorConfig{})
	out := firstOutput(t, s)
	f, err := s.Capture(context.Background(), ports.Target{OutputID: out.ID})
	require.NoError(t, err)
	require.NoError(t, f.Validate())
	require.Equal(t, 8, f.Width)
	require.Equal(t, 4, f.Height)
	require.Equal(t, 32, f.Stride)
	require.Equal(t, ports.XRGB8888, f.Format)
	require.False(t, f.YInvert)
	for y := 0; y < f.Height; y++ {
		row := f.Row(y)
		for x := 0; x < f.Width; x++ {
			want := pixel(x, y, 1)
			require.Equal(t, want[:], row[x*4:x*4+4], "pixel %d,%d", x, y)
		}
	}
	require.Zero(t, comp.stat(func(c *compositor) int { return int(c.lastRegion[2]) }), "full output must not send a region")
}

func TestCaptureStrideFormatAndYInvert(t *testing.T) {
	s, _ := newSource(t, compositorConfig{
		announce: []announce{{format: 0, width: 5, height: 3, stride: 5*4 + 12}},
		flags:    1,
	})
	out := firstOutput(t, s)
	f, err := s.Capture(context.Background(), ports.Target{OutputID: out.ID})
	require.NoError(t, err)
	require.NoError(t, f.Validate())
	require.Equal(t, ports.ARGB8888, f.Format)
	require.Equal(t, 32, f.Stride)
	require.Equal(t, 5, f.Width)
	require.Equal(t, 3, f.Height)
	require.True(t, f.YInvert)
	require.Len(t, f.Pixels, 32*3)
	// Row(0) is the top row, stored last when y-inverted.
	want := pixel(0, 2, 1)
	require.Equal(t, want[:], f.Row(0)[:4])
	require.Len(t, f.Row(0), 20)
	// Padding is not part of a row but is preserved in storage.
	require.Equal(t, byte(0xEE), f.Pixels[20])
}

func TestCaptureRegion(t *testing.T) {
	s, comp := newSource(t, compositorConfig{})
	out := firstOutput(t, s)
	f, err := s.Capture(context.Background(), ports.Target{OutputID: out.ID, Region: ports.Region{X: 1, Y: 2, Width: 3, Height: 1}})
	require.NoError(t, err)
	require.Equal(t, 3, f.Width)
	require.Equal(t, 1, f.Height)
	comp.mu.Lock()
	defer comp.mu.Unlock()
	require.True(t, comp.lastRegionSet)
	require.Equal(t, [4]int32{1, 2, 3, 1}, comp.lastRegion)
}

func TestCaptureInvalidRegion(t *testing.T) {
	s, comp := newSource(t, compositorConfig{})
	out := firstOutput(t, s)
	for _, r := range []ports.Region{
		{X: -1, Y: 0, Width: 1, Height: 1},
		{X: 0, Y: -1, Width: 1, Height: 1},
		{X: 0, Y: 0, Width: 0, Height: 1},
		{X: 0, Y: 0, Width: 1, Height: -3},
		{X: 0, Y: 0, Width: ports.MaxDimension + 1, Height: 1},
		{X: 1 << 40, Y: 0, Width: 1, Height: 1},
	} {
		_, err := s.Capture(context.Background(), ports.Target{OutputID: out.ID, Region: r})
		require.ErrorIs(t, err, ports.ErrInvalidRegion, "%+v", r)
	}
	require.Zero(t, comp.stat(func(c *compositor) int { return c.captures }))
	_, err := s.Capture(context.Background(), ports.Target{OutputID: out.ID})
	require.NoError(t, err, "invalid regions must not end the session")
}

func TestCaptureUnknownOutput(t *testing.T) {
	s, _ := newSource(t, compositorConfig{})
	_, err := s.Capture(context.Background(), ports.Target{OutputID: 99})
	require.ErrorIs(t, err, ports.ErrOutputNotFound)
	_, err = s.Capture(context.Background(), ports.Target{OutputID: 1})
	require.NoError(t, err)
}

func TestCaptureRejectsBadBuffers(t *testing.T) {
	const xrgb = 1
	cases := map[string]announce{
		"zero width":       {xrgb, 0, 4, 16},
		"zero height":      {xrgb, 4, 0, 16},
		"wide":             {xrgb, ports.MaxDimension + 1, 1, (ports.MaxDimension + 1) * 4},
		"tall":             {xrgb, 1, ports.MaxDimension + 1, 4},
		"short stride":     {xrgb, 4, 4, 12},
		"unaligned stride": {xrgb, 4, 4, 18},
		"over 256 MiB":     {xrgb, 16384, 16384, 16384 * 4},
		"huge stride":      {xrgb, 4, 4, 1 << 31},
		"stride overflow":  {xrgb, 4, 2, 0xffffffff - 3},
	}
	for name, a := range cases {
		t.Run(name, func(t *testing.T) {
			s, comp := newSource(t, compositorConfig{announce: []announce{a}})
			out := firstOutput(t, s)
			_, err := s.Capture(context.Background(), ports.Target{OutputID: out.ID})
			require.Error(t, err)
			require.Zero(t, comp.stat(func(c *compositor) int { return c.pools }), "nothing may be allocated before validation")
			require.Zero(t, comp.stat(func(c *compositor) int { return c.copies }))
			_, err = s.Outputs(context.Background())
			require.NoError(t, err, "a rejected announcement leaves the connection usable")
		})
	}
}

func TestCaptureUnsupportedFormat(t *testing.T) {
	for _, format := range []uint32{2, 0x34325258, 0x34324752, 0xffffffff} { // C8, XR24-like fourcc, RG24, garbage
		s, comp := newSource(t, compositorConfig{announce: []announce{{format, 4, 4, 16}}})
		out := firstOutput(t, s)
		_, err := s.Capture(context.Background(), ports.Target{OutputID: out.ID})
		require.ErrorIs(t, err, ports.ErrUnsupportedFormat, "format %#x", format)
		require.Zero(t, comp.stat(func(c *compositor) int { return c.pools }))
	}
}

func TestCapturePicksSupportedAnnouncement(t *testing.T) {
	s, _ := newSource(t, compositorConfig{announce: []announce{{0x34325258, 4, 4, 16}, {1, 3, 2, 12}}})
	out := firstOutput(t, s)
	f, err := s.Capture(context.Background(), ports.Target{OutputID: out.ID})
	require.NoError(t, err)
	require.Equal(t, 3, f.Width)
	require.Equal(t, ports.XRGB8888, f.Format)
}

func TestCaptureWithoutBufferDone(t *testing.T) {
	s, _ := newSource(t, compositorConfig{screencopyVersion: 1})
	out := firstOutput(t, s)
	f, err := s.Capture(context.Background(), ports.Target{OutputID: out.ID})
	require.NoError(t, err)
	require.NoError(t, f.Validate())
}

func TestMissingScreencopy(t *testing.T) {
	comp := startCompositor(t, compositorConfig{noScreencopy: true})
	_, err := New(context.Background(), comp.path)
	require.Error(t, err)
}

func TestReuseAndGeometryChange(t *testing.T) {
	s, comp := newSource(t, compositorConfig{})
	out := firstOutput(t, s)
	target := ports.Target{OutputID: out.ID}
	f1, err := s.Capture(context.Background(), target)
	require.NoError(t, err)
	first := &f1.Pixels[0]
	for i := 2; i <= 5; i++ {
		f, err := s.Capture(context.Background(), target)
		require.NoError(t, err)
		require.Same(t, first, &f.Pixels[0], "storage must be reused")
		want := pixel(3, 1, uint32(i))
		require.Equal(t, want[:], f.Row(1)[12:16], "capture %d must show fresh content", i)
	}
	require.Equal(t, 1, comp.stat(func(c *compositor) int { return c.pools }))
	require.Equal(t, 1, comp.stat(func(c *compositor) int { return c.buffers }))
	require.Equal(t, 1, comp.stat(func(c *compositor) int { return c.fdsReceived }))

	f, err := s.Capture(context.Background(), ports.Target{OutputID: out.ID, Region: ports.Region{Width: 2, Height: 2}})
	require.NoError(t, err)
	require.Equal(t, 2, f.Width)
	require.Equal(t, 2, comp.stat(func(c *compositor) int { return c.pools }))
	require.Equal(t, 1, comp.stat(func(c *compositor) int { return c.destroyedBuffers }))
}

func TestNoDescriptorLeak(t *testing.T) {
	s, comp := newSource(t, compositorConfig{})
	out := firstOutput(t, s)
	target := ports.Target{OutputID: out.ID}
	_, err := s.Capture(context.Background(), target)
	require.NoError(t, err)
	steady := fdCount(t)
	for i := 0; i < 30; i++ {
		// Alternate geometry: every switch creates a pool and must not keep its descriptor.
		region := ports.Region{Width: 2 + i%2, Height: 2}
		_, err := s.Capture(context.Background(), ports.Target{OutputID: out.ID, Region: region})
		require.NoError(t, err)
		_, err = s.Capture(context.Background(), target)
		require.NoError(t, err)
	}
	require.Equal(t, 61, comp.stat(func(c *compositor) int { return c.fdsReceived }))
	require.Equal(t, steady, fdCount(t))

	require.NoError(t, s.Close())
	require.Eventually(t, func() bool { return fdCount(t) <= steady-2 }, 2*time.Second, 5*time.Millisecond,
		"connection descriptors must be closed on Close")
}

func TestCancelInterruptsBlockedCapture(t *testing.T) {
	s, comp := newSource(t, compositorConfig{hold: true})
	out := firstOutput(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := s.Capture(ctx, ports.Target{OutputID: out.ID})
		errc <- err
	}()
	<-comp.copyEvent
	cancel()
	select {
	case err := <-errc:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(3 * time.Second):
		t.Fatal("capture not interrupted")
	}
	_, err := s.Outputs(context.Background())
	require.ErrorIs(t, err, ports.ErrClosed, "cancellation is not recoverable")
	_, err = s.Capture(context.Background(), ports.Target{OutputID: out.ID})
	require.ErrorIs(t, err, ports.ErrClosed)
	require.NoError(t, s.Close())
}

func TestCloseInterruptsBlockedCapture(t *testing.T) {
	s, comp := newSource(t, compositorConfig{hold: true})
	out := firstOutput(t, s)
	errc := make(chan error, 1)
	go func() {
		_, err := s.Capture(context.Background(), ports.Target{OutputID: out.ID})
		errc <- err
	}()
	<-comp.copyEvent
	require.NoError(t, s.Close())
	select {
	case err := <-errc:
		require.ErrorIs(t, err, ports.ErrClosed)
	case <-time.After(3 * time.Second):
		t.Fatal("capture not interrupted")
	}
	require.NoError(t, s.Close())
	_, err := s.Outputs(context.Background())
	require.ErrorIs(t, err, ports.ErrClosed)
}

func TestCancelledContextBeforeCall(t *testing.T) {
	s, _ := newSource(t, compositorConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.Outputs(ctx)
	require.ErrorIs(t, err, context.Canceled)
	_, err = s.Outputs(context.Background())
	require.ErrorIs(t, err, ports.ErrClosed)
}

func TestCompositorDisconnect(t *testing.T) {
	s, comp := newSource(t, compositorConfig{hold: true})
	out := firstOutput(t, s)
	errc := make(chan error, 1)
	go func() {
		_, err := s.Capture(context.Background(), ports.Target{OutputID: out.ID})
		errc <- err
	}()
	<-comp.copyEvent
	comp.mu.Lock()
	comp.conn.Close()
	comp.mu.Unlock()
	select {
	case err := <-errc:
		require.Error(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("capture not interrupted")
	}
	_, err := s.Capture(context.Background(), ports.Target{OutputID: out.ID})
	require.ErrorIs(t, err, ports.ErrClosed)
}

func TestConstructorContextIsScopedToConstructor(t *testing.T) {
	comp := startCompositor(t, compositorConfig{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	s, err := New(ctx, comp.path)
	require.NoError(t, err)
	defer s.Close()
	cancel()
	time.Sleep(20 * time.Millisecond)
	out := firstOutput(t, s)
	_, err = s.Capture(context.Background(), ports.Target{OutputID: out.ID})
	require.NoError(t, err, "cancelling the constructor context must not close the source")
}

func TestConstructorTimeout(t *testing.T) {
	comp := startCompositor(t, compositorConfig{silent: true})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := New(ctx, comp.path)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(start), 2*time.Second)
}

func TestConstructorConnectFailure(t *testing.T) {
	_, err := New(context.Background(), t.TempDir()+"/missing")
	require.Error(t, err)
	t.Setenv("XDG_RUNTIME_DIR", "")
	_, err = New(context.Background(), "relative")
	require.Error(t, err)
}

func TestConcurrentCallsAreSerialized(t *testing.T) {
	s, _ := newSource(t, compositorConfig{})
	out := firstOutput(t, s)
	errc := make(chan error, 8)
	for range 8 {
		go func() {
			_, err := s.Capture(context.Background(), ports.Target{OutputID: out.ID})
			if err == nil {
				_, err = s.Outputs(context.Background())
			}
			errc <- err
		}()
	}
	for range 8 {
		require.NoError(t, <-errc)
	}
}

// Steady state cost of one capture over the real transport. The measurement
// includes the in-process protocol peer, so it is an upper bound for the
// Source alone; the budget is set from measured values (see the benchmark).
func TestCaptureAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("race instrumentation changes allocation counts")
	}
	s, _ := newSource(t, compositorConfig{outputs: []outputSpec{{name: "X", width: 64, height: 64, scale: 1, version: 4}}})
	out := firstOutput(t, s)
	ctx := context.Background()
	target := ports.Target{OutputID: out.ID}
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
	t.Logf("allocations per capture (client and in-process peer): %g", avg)
	const budget = 14 // measured 12 (client and in-process peer), with headroom
	if avg > budget {
		t.Fatalf("allocations per capture %g exceed budget %d", avg, budget)
	}
	cancelable, cancel := context.WithCancel(ctx)
	defer cancel()
	avgCancelable := testing.AllocsPerRun(200, func() {
		if _, err := s.Capture(cancelable, target); err != nil {
			panic(err)
		}
	})
	t.Logf("allocations per capture with cancellable context: %g", avgCancelable)
	if avgCancelable > budget {
		t.Fatalf("cancellable capture allocations %g exceed budget", avgCancelable)
	}
}

func BenchmarkCapture(b *testing.B) {
	comp := startCompositor(b, compositorConfig{outputs: []outputSpec{{name: "BENCH", width: 1920, height: 1080, scale: 1, version: 4}}})
	s, err := New(context.Background(), comp.path)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	outs, err := s.Outputs(context.Background())
	if err != nil || len(outs) == 0 {
		b.Fatal(err)
	}
	target := ports.Target{OutputID: outs[0].ID}
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

func TestCaptureWaitsForReady(t *testing.T) {
	s, comp := newSource(t, compositorConfig{hold: true})
	out := firstOutput(t, s)
	type result struct {
		f   ports.Frame
		err error
	}
	res := make(chan result, 1)
	go func() {
		f, err := s.Capture(context.Background(), ports.Target{OutputID: out.ID})
		res <- result{f, err}
	}()
	<-comp.copyEvent
	select {
	case <-res:
		t.Fatal("capture returned before the compositor completed the copy")
	case <-time.After(50 * time.Millisecond):
	}
	comp.completePending()
	r := <-res
	require.NoError(t, r.err)
	want := pixel(1, 1, 1)
	require.Equal(t, want[:], r.f.Row(1)[4:8])
}

// Opt-in: NEFERCAP_WAYLAND_SOCKET names the socket of a disposable headless
// compositor that supports wlr-screencopy. Never point it at a real session.
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
	require.NotEmpty(t, out.Name)
	for range 3 {
		f, err := s.Capture(context.Background(), ports.Target{OutputID: out.ID})
		require.NoError(t, err)
		require.NoError(t, f.Validate())
	}
	f, err := s.Capture(context.Background(), ports.Target{OutputID: out.ID, Region: ports.Region{Width: 16, Height: 8}})
	require.NoError(t, err)
	require.Equal(t, 16, f.Width)
	require.Equal(t, 8, f.Height)
}

func TestIdleCancellationDoesNotKillSource(t *testing.T) {
	s, _ := newSource(t, compositorConfig{})
	out := firstOutput(t, s)
	target := ports.Target{OutputID: out.ID}
	ctx, cancel := context.WithCancel(context.Background())
	_, err := s.Capture(ctx, target)
	require.NoError(t, err)
	_, err = s.Capture(ctx, target) // same context: the watcher stays registered
	require.NoError(t, err)
	cancel()
	_, err = s.Capture(context.Background(), target)
	require.NoError(t, err, "a context cancelled between calls must not end the source")
	_, err = s.Outputs(ctx)
	require.ErrorIs(t, err, context.Canceled, "the next call given the cancelled context observes it")
	_, err = s.Capture(context.Background(), target)
	require.ErrorIs(t, err, ports.ErrClosed)
}

func TestStaleContextDoesNotAffectLaterCall(t *testing.T) {
	s, comp := newSource(t, compositorConfig{hold: true})
	out := firstOutput(t, s)
	target := ports.Target{OutputID: out.ID}
	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()

	runCapture := func(ctx context.Context) chan error {
		errc := make(chan error, 1)
		go func() {
			_, err := s.Capture(ctx, target)
			errc <- err
		}()
		<-comp.copyEvent
		return errc
	}
	errc := runCapture(ctxA)
	comp.completePending()
	require.NoError(t, <-errc)

	errc = runCapture(ctxB) // blocked under B while A's old registration is replaced
	cancelA()
	select {
	case err := <-errc:
		t.Fatalf("cancelling a previous call's context ended a later call: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	comp.completePending()
	require.NoError(t, <-errc)

	errc = runCapture(ctxB)
	cancelB()
	select {
	case err := <-errc:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(3 * time.Second):
		t.Fatal("blocked capture not interrupted")
	}
	s.opMu.Lock()
	released := s.buf.data == nil && s.watchStop == nil
	s.opMu.Unlock()
	require.True(t, released, "terminal source must release its mapping and watcher")
	_, err := s.Outputs(context.Background())
	require.ErrorIs(t, err, ports.ErrClosed)
}

func TestContextSwitchRace(t *testing.T) {
	s, _ := newSource(t, compositorConfig{})
	out := firstOutput(t, s)
	target := ports.Target{OutputID: out.ID}
	for i := 0; i < 200; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		_, err := s.Capture(ctx, target)
		require.NoError(t, err)
		go cancel() // races with the next call, which uses another context
		_, err = s.Capture(context.Background(), target)
		require.NoError(t, err)
		cancel()
	}
}

func TestNoShmOffer(t *testing.T) {
	s, comp := newSource(t, compositorConfig{noShmOffer: true})
	out := firstOutput(t, s)
	_, err := s.Capture(context.Background(), ports.Target{OutputID: out.ID})
	require.ErrorIs(t, err, ports.ErrUnsupportedFormat)
	require.ErrorContains(t, err, "no wl_shm buffer")
	require.Zero(t, comp.stat(func(c *compositor) int { return c.pools }))
	_, err = s.Outputs(context.Background())
	require.NoError(t, err)
}

func TestMalformedOutputModeAndScale(t *testing.T) {
	s, _ := newSource(t, compositorConfig{outputs: []outputSpec{
		{name: "BAD", width: 0xffffffff, height: 1 << 30, scale: -4, version: 4},
		{name: "HUGE", width: 100, height: 50, scale: 1000, version: 4},
	}})
	outs, err := s.Outputs(context.Background())
	require.NoError(t, err)
	require.Equal(t, []ports.Output{
		{ID: 1, Name: "BAD", Scale: 1},
		{ID: 2, Name: "HUGE", Width: 100, Height: 50, Scale: maxOutputScale},
	}, outs)
}

func TestDuplicateGlobalIgnored(t *testing.T) {
	s, _ := newSource(t, compositorConfig{duplicateGlobal: true})
	outs, err := s.Outputs(context.Background())
	require.NoError(t, err)
	require.Len(t, outs, 1)
	require.Equal(t, 1, s.bound)
}

func TestReadyWithoutCopyIsTerminal(t *testing.T) {
	s, _ := newSource(t, compositorConfig{readyEarly: true})
	out := firstOutput(t, s)
	_, err := s.Capture(context.Background(), ports.Target{OutputID: out.ID})
	require.Error(t, err)
	_, err = s.Outputs(context.Background())
	require.ErrorIs(t, err, ports.ErrClosed)
}

func TestCompositorFailureIsRecoverable(t *testing.T) {
	t.Run("before copy", func(t *testing.T) {
		s, comp := newSource(t, compositorConfig{failCapture: true})
		out := firstOutput(t, s)
		_, err := s.Capture(context.Background(), ports.Target{OutputID: out.ID})
		require.Error(t, err)
		comp.mu.Lock()
		comp.cfg.failCapture = false
		comp.mu.Unlock()
		_, err = s.Capture(context.Background(), ports.Target{OutputID: out.ID})
		require.NoError(t, err)
	})
	t.Run("after copy", func(t *testing.T) {
		s, _ := newSource(t, compositorConfig{failCopies: 1})
		out := firstOutput(t, s)
		_, err := s.Capture(context.Background(), ports.Target{OutputID: out.ID})
		require.Error(t, err)
		require.NotErrorIs(t, err, ports.ErrClosed)
		f, err := s.Capture(context.Background(), ports.Target{OutputID: out.ID})
		require.NoError(t, err)
		require.NoError(t, f.Validate())
	})
}
