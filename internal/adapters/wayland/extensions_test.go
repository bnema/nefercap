package wayland

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/bnema/go-wayland-bindings/client/wayland"
	"github.com/bnema/wlturbo"
	"github.com/stretchr/testify/require"

	"github.com/bnema/nefercap/internal/adapters/wayland/testserver"
	"github.com/bnema/nefercap/internal/logging"
	"github.com/bnema/nefercap/internal/ports"
)

func extConfig() testserver.Config {
	return testserver.Config{
		Outputs:       []testserver.OutputSpec{{Name: "BIG", Width: 64, Height: 48, Scale: 1}},
		NeferwlSource: true, NeferwlExclusion: true,
	}
}

func TestCapabilitiesWithExtension(t *testing.T) {
	s, _ := newSource(t, extConfig())
	require.Equal(t, ports.Capabilities{Exclusion: true}, s.Capabilities())
}

func TestRegionUsesExtensionSource(t *testing.T) {
	s, srv := newSource(t, extConfig())
	out := firstOutput(t, s)
	f, err := s.Capture(context.Background(), ports.Target{OutputID: out.ID, Region: ports.Region{X: 10, Y: 5, Width: 20, Height: 8}})
	require.NoError(t, err)
	require.NoError(t, f.Validate())
	require.Equal(t, [3]int{20, 8, 80}, [3]int{f.Width, f.Height, f.Stride}, "the compositor sends only the region: no crop")
	st := srv.Stats()
	require.Equal(t, "region", st.LastSource)
	require.Equal(t, [4]int32{10, 5, 20, 8}, st.LastRegion)
	want := testserver.Pixel(10, 5, 1)
	require.Equal(t, want[:], f.Row(0)[:4])
	// the same region reuses the session; another one replaces it
	_, err = s.Capture(context.Background(), ports.Target{OutputID: out.ID, Region: ports.Region{X: 10, Y: 5, Width: 20, Height: 8}})
	require.NoError(t, err)
	require.Equal(t, 1, srv.Stats().Sessions)
	_, err = s.Capture(context.Background(), ports.Target{OutputID: out.ID, Region: ports.Region{X: 0, Y: 0, Width: 4, Height: 4}})
	require.NoError(t, err)
	st = srv.Stats()
	require.Equal(t, 2, st.Sessions)
	require.Equal(t, 1, st.DestroyedSessions)
}

func TestBeginExclusionNeedsExtension(t *testing.T) {
	s, _ := newSource(t, testserver.Config{})
	_, err := s.BeginExclusion(context.Background(), ports.Target{OutputID: firstOutput(t, s).ID})
	require.ErrorIs(t, err, ErrExclusionUnsupported)
}

func TestBeginExclusionAndCaptureKeepSession(t *testing.T) {
	s, srv := newSource(t, extConfig())
	target := ports.Target{OutputID: firstOutput(t, s).ID}
	token, err := s.BeginExclusion(context.Background(), target)
	require.NoError(t, err)
	require.Equal(t, testserver.Token, token)
	for range 3 {
		_, err = s.Capture(context.Background(), target)
		require.NoError(t, err)
	}
	st := srv.Stats()
	require.Equal(t, 1, st.Sessions, "capture reuses the session that owns the exclusion")
	require.Equal(t, 1, st.Exclusions)
	_, err = s.BeginExclusion(context.Background(), target)
	require.Error(t, err, "one exclusion per session")
}

func TestExclusionRefused(t *testing.T) {
	cfg := extConfig()
	cfg.ExclusionFail = 1 // busy
	s, _ := newSource(t, cfg)
	_, err := s.BeginExclusion(context.Background(), ports.Target{OutputID: firstOutput(t, s).ID})
	require.ErrorIs(t, err, ErrExclusionUnavailable)
}

// Any refusal that is neither busy nor session_stopped is a plain
// ErrExclusionUnavailable carrying the reason.
func TestExclusionOtherReasonIsUnavailable(t *testing.T) {
	cfg := extConfig()
	cfg.ExclusionFail = 4 // reason 3
	s, _ := newSource(t, cfg)
	_, err := s.BeginExclusion(context.Background(), ports.Target{OutputID: firstOutput(t, s).ID})
	require.ErrorIs(t, err, ErrExclusionUnavailable)
	require.NotErrorIs(t, err, ErrExclusionBusy)
	require.NotErrorIs(t, err, ports.ErrCaptureStopped)
	require.ErrorContains(t, err, "reason 3")
}

func TestExclusionSessionStoppedStaysStopped(t *testing.T) {
	s, srv := newSource(t, extConfig())
	target := ports.Target{OutputID: firstOutput(t, s).ID}
	_, err := s.BeginExclusion(context.Background(), target)
	require.NoError(t, err)
	srv.StopSessions()
	for range 2 {
		_, err = s.Capture(context.Background(), target)
		require.ErrorIs(t, err, ports.ErrCaptureStopped, "a recording never continues on a session without its exclusion")
	}
	require.Equal(t, 1, srv.Stats().Sessions)
}

func TestExclusionEndsWithTargetChange(t *testing.T) {
	s, srv := newSource(t, extConfig())
	out := firstOutput(t, s)
	_, err := s.BeginExclusion(context.Background(), ports.Target{OutputID: out.ID})
	require.NoError(t, err)
	_, err = s.Capture(context.Background(), ports.Target{OutputID: out.ID, Region: ports.Region{Width: 4, Height: 4}})
	require.NoError(t, err)
	require.Equal(t, 1, srv.Stats().DestroyedExclusions)
}

// hudConnection is a second connection, like the HUD's, with a surface and a
// goroutine that reads its events, as neferclient's reader does.
func hudConnection(t *testing.T, srv *testserver.Server) (*wlturbo.Display, *core.Surface) {
	t.Helper()
	conn, err := net.Dial("unix", srv.Path)
	require.NoError(t, err)
	display, err := wlturbo.ConnectFromConn(conn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = display.Close() })
	require.NoError(t, display.Roundtrip())
	comp := core.NewCompositor(display.Context())
	_, err = display.Registry().BindNegotiated("wl_compositor", 4, comp)
	require.NoError(t, err)
	surface, err := comp.CreateSurface()
	require.NoError(t, err)
	read := make(chan struct{})
	go func() {
		defer close(read)
		for display.Dispatch() == nil {
		}
	}()
	t.Cleanup(func() { _ = display.Close(); <-read })
	return display, surface
}

func TestAuthorizeLayerWaitsForAttached(t *testing.T) {
	_, srv := newSource(t, extConfig())
	display, surface := hudConnection(t, srv)
	require.NoError(t, AuthorizeLayer(context.Background(), display, surface, testserver.Token, nil))
	require.Equal(t, []string{testserver.Token}, srv.Stats().Attached)
}

func TestAuthorizeLayerAttachFailed(t *testing.T) {
	for reason, want := range map[uint32]AttachFailure{
		1: AttachUnknownToken, 2: AttachUnauthorized, 3: AttachTooManyLayers,
		4: AttachAlreadyAttached, 5: AttachExclusionEnded, 6: AttachLayerDestroyed,
	} {
		cfg := extConfig()
		cfg.AttachFail = reason
		srv := testserver.Start(t, cfg)
		display, surface := hudConnection(t, srv)
		err := AuthorizeLayer(context.Background(), display, surface, testserver.Token, nil)
		require.ErrorIs(t, err, ErrAttachFailed)
		var failed *AttachFailedError
		require.ErrorAs(t, err, &failed)
		require.Equal(t, want, failed.Reason)
		require.Equal(t, want == AttachUnknownToken || want == AttachExclusionEnded, errors.Is(err, ErrExclusionUnavailable))
		require.Equal(t, want == AttachUnauthorized, errors.Is(err, ErrUnauthorized))
		require.NotContains(t, err.Error(), testserver.Token)
	}
}

func TestAuthorizeLayerDetachedCallback(t *testing.T) {
	_, srv := newSource(t, extConfig())
	display, surface := hudConnection(t, srv)
	var got atomic.Pointer[LayerDetachedError]
	calls := make(chan struct{}, 4)
	require.NoError(t, AuthorizeLayer(context.Background(), display, surface, testserver.Token, func(err error) {
		var d *LayerDetachedError
		if errors.As(err, &d) {
			got.Store(d)
		}
		calls <- struct{}{}
	}))
	require.Nil(t, got.Load())
	srv.Detach(0, uint32(DetachExclusionEnded))
	select {
	case <-calls:
	case <-time.After(5 * time.Second):
		t.Fatal("onDetached was not called")
	}
	d := got.Load()
	require.NotNil(t, d)
	require.ErrorIs(t, d, ErrExclusionUnavailable)
	require.NotContains(t, d.Error(), testserver.Token)
}

func TestAuthorizeLayerProtocolViolationClosesDisplay(t *testing.T) {
	_, srv := newSource(t, extConfig())
	display, surface := hudConnection(t, srv)
	got := make(chan error, 1)
	require.NoError(t, AuthorizeLayer(context.Background(), display, surface, testserver.Token, func(err error) { got <- err }))
	srv.SendRaw(0, 0, 0) // a second attached
	require.ErrorIs(t, <-got, ErrExclusionUnavailable)
	require.True(t, display.Closed(), "the surface could be visible in captures: the connection ends")
}

func TestAuthorizeLayerContextAndTimeout(t *testing.T) {
	cfg := extConfig()
	cfg.AttachSilent = true
	srv := testserver.Start(t, cfg)
	display, surface := hudConnection(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- AuthorizeLayer(ctx, display, surface, testserver.Token, nil) }()
	require.Eventually(t, func() bool { return len(srv.Stats().Attached) == 1 }, 5*time.Second, time.Millisecond)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	require.True(t, display.Closed())

	display2, surface2 := hudConnection(t, srv)
	err := authorizeLayer(context.Background(), display2, surface2, testserver.Token, nil, 50*time.Millisecond)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.True(t, display2.Closed())
}

func TestAuthorizeLayerRejectsBadInput(t *testing.T) {
	require.Error(t, AuthorizeLayer(context.Background(), nil, nil, testserver.Token, nil))
	_, srv := newSource(t, extConfig())
	display, surface := hudConnection(t, srv)
	require.Error(t, AuthorizeLayer(context.Background(), display, (*core.Surface)(nil), testserver.Token, nil), "typed nil surface")
	for _, tok := range []string{"", "has space", testserver.Token[:63], testserver.Token + "0", testserver.Token[:63] + "G", testserver.Token[:63] + "A"} {
		require.Error(t, AuthorizeLayer(context.Background(), display, surface, tok, nil), "%q", tok)
	}
	require.Empty(t, srv.Stats().Attached)
}

func TestAuthorizeLayerWithoutGlobal(t *testing.T) {
	srv := testserver.Start(t, testserver.Config{})
	display, surface := hudConnection(t, srv)
	require.ErrorIs(t, AuthorizeLayer(context.Background(), display, surface, testserver.Token, nil), ErrExclusionUnsupported)
}

// A refused exclusion leaves none behind: the session is an ordinary one and
// is not kept alive by the sticky-stopped rule.
func TestRefusedExclusionDoesNotPinStoppedSession(t *testing.T) {
	cfg := extConfig()
	cfg.ExclusionFail = 1
	s, srv := newSource(t, cfg)
	target := ports.Target{OutputID: firstOutput(t, s).ID}
	_, err := s.BeginExclusion(context.Background(), target)
	require.ErrorIs(t, err, ErrExclusionUnavailable)
	require.Equal(t, 1, srv.Stats().DestroyedExclusions, "the failed exclusion object is destroyed")
	srv.StopSessions()
	_, err = s.Capture(context.Background(), target)
	require.ErrorIs(t, err, ports.ErrCaptureStopped)
	_, err = s.Capture(context.Background(), target)
	require.NoError(t, err, "a stopped session without an exclusion is replaced")
}

// The token is a secret: nothing of the exclusion flow logs it.
func TestTokenNeverLogged(t *testing.T) {
	var logs bytes.Buffer
	ctx := logging.With(context.Background(), &logs, true)
	srv := testserver.Start(t, extConfig())
	s, err := New(ctx, srv.Path)
	require.NoError(t, err)
	defer s.Close()
	target := ports.Target{OutputID: firstOutput(t, s).ID}
	token, err := s.BeginExclusion(ctx, target)
	require.NoError(t, err)
	_, err = s.Capture(ctx, target)
	require.NoError(t, err)
	display, surface := hudConnection(t, srv)
	require.NoError(t, AuthorizeLayer(ctx, display, surface, token, nil))
	srv.StopSessions()
	_, _ = s.Capture(ctx, target)
	require.NotEmpty(t, logs.String(), "the source logs something")
	require.NotContains(t, logs.String(), token)
}
