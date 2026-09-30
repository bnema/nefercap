package wayland

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bnema/wlturbo"
	"github.com/bnema/wlturbo/protocol/core"
	"github.com/stretchr/testify/require"

	"github.com/bnema/nefercap/internal/ports"
)

const (
	testToken  = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testToken2 = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

func privateCfg(p privateConfig) compositorConfig {
	p.enabled = true
	return compositorConfig{
		outputs: []outputSpec{{name: "DP-1", width: 16, height: 8, scale: 1, version: 4}},
		private: p,
	}
}

func TestSessionUnsupportedWithoutGlobal(t *testing.T) {
	s, _ := newSource(t, compositorConfig{})
	ctx := context.Background()
	_, err := s.BeginSession(ctx, ports.Target{OutputID: 1}, true)
	require.ErrorIs(t, err, ErrSessionUnsupported)
	_, err = s.Workspaces(ctx)
	require.ErrorIs(t, err, ErrSessionUnsupported)
	require.ErrorIs(t, s.PingSession(ctx), ErrSessionUnavailable)
	require.NoError(t, s.EndSession(ctx))
	// Standard capture is unaffected and the Source stays usable.
	_, err = s.Capture(ctx, ports.Target{OutputID: 1})
	require.NoError(t, err)
}

func TestBeginSessionOutputAndRegion(t *testing.T) {
	s, comp := newSource(t, privateCfg(privateConfig{}))
	ctx := context.Background()
	out := firstOutput(t, s)

	st, err := s.BeginSession(ctx, ports.Target{OutputID: out.ID, Region: ports.Region{X: 2, Y: 1, Width: 4, Height: 3}}, true)
	require.NoError(t, err)
	require.True(t, st.Active)
	require.Equal(t, testToken, st.Token)
	require.EqualValues(t, 1, st.Revision)
	require.Equal(t, ports.Target{OutputID: out.ID, Region: ports.Region{X: 2, Y: 1, Width: 4, Height: 3}}, st.Target)
	require.Equal(t, ports.Region{X: 2, Y: 1, Width: 4, Height: 3}, st.Region)
	require.Equal(t, [4]int32{2, 1, 4, 3}, comp.snap().requested)
	require.EqualValues(t, 1, comp.snap().record)

	_, err = s.BeginSession(ctx, ports.Target{OutputID: out.ID}, false)
	require.ErrorIs(t, err, ErrSessionActive)

	require.NoError(t, s.EndSession(ctx))
	require.Equal(t, 1, comp.stat(func(c *compositor) int { return c.privDestroyed }))
	require.NoError(t, s.EndSession(ctx)) // idempotent

	st, err = s.BeginSession(ctx, ports.Target{OutputID: out.ID}, false) // full output
	require.NoError(t, err)
	require.Equal(t, ports.Region{X: 0, Y: 0, Width: 16, Height: 8}, st.Target.Region)
	require.EqualValues(t, 0, comp.snap().record)
}

func TestBeginSessionReturnsFirstStateThenResumes(t *testing.T) {
	s, _ := newSource(t, privateCfg(privateConfig{inactive: true}))
	ctx := context.Background()
	st, err := s.BeginSession(ctx, ports.Target{OutputID: 1}, true)
	require.NoError(t, err)
	require.False(t, st.Active, "the first state was paused")
	require.EqualValues(t, 1, st.Revision)
	_, err = s.Capture(ctx, ports.Target{OutputID: 1}) // the resume is read here
	require.NoError(t, err)
}

func TestBeginSessionRefused(t *testing.T) {
	s, _ := newSource(t, privateCfg(privateConfig{refuse: true}))
	ctx := context.Background()
	_, err := s.BeginSession(ctx, ports.Target{OutputID: firstOutput(t, s).ID}, true)
	require.ErrorIs(t, err, ErrSessionUnavailable)
	var stopped *SessionStoppedError
	require.ErrorAs(t, err, &stopped)
	require.Equal(t, StopBusy, stopped.Reason)
	require.ErrorIs(t, s.PingSession(ctx), ErrSessionUnavailable)
	_, err = s.Capture(ctx, ports.Target{OutputID: 1}) // the connection survived
	require.NoError(t, err)
}

func TestBeginSessionInvalidTargets(t *testing.T) {
	s, comp := newSource(t, privateCfg(privateConfig{}))
	ctx := context.Background()
	_, err := s.BeginSession(ctx, ports.Target{OutputID: 99}, true)
	require.ErrorIs(t, err, ports.ErrOutputNotFound)
	_, err = s.BeginSession(ctx, ports.Target{OutputID: 1, Region: ports.Region{Width: -1, Height: 1}}, true)
	require.ErrorIs(t, err, ports.ErrInvalidRegion)
	_, err = s.BeginSession(ctx, ports.Target{WorkspaceID: 42}, true)
	require.ErrorIs(t, err, ErrWorkspaceNotFound)
	require.Zero(t, comp.stat(func(c *compositor) int { return c.privSessions }))
}

func TestWorkspacesFromAdvertisements(t *testing.T) {
	cfg := privateCfg(privateConfig{workspaces: []workspaceSpec{
		{id: 1<<32 | 7, output: "DP-1", name: "main", region: [4]int32{0, 0, 16, 8}, active: true},
		{id: 5, output: "DP-1", name: "other", region: [4]int32{0, 0, 16, 8}},
		{id: 6, output: "GONE", name: "orphan", region: [4]int32{0, 0, 16, 8}},
		{id: 8, output: "DP-1", name: "bad", region: [4]int32{0, 0, 0, 8}},
	}})
	s, _ := newSource(t, cfg)
	list, err := s.Workspaces(context.Background())
	require.NoError(t, err)
	require.Equal(t, []ports.Workspace{
		{ID: 5, OutputID: 1, Name: "other", Region: ports.Region{Width: 16, Height: 8}},
		{ID: 1<<32 | 7, OutputID: 1, Name: "main", Region: ports.Region{Width: 16, Height: 8}, Active: true},
	}, list)
}

func TestWorkspaceSessionAndLiveRegion(t *testing.T) {
	const id = 1<<32 | 7
	cfg := privateCfg(privateConfig{workspaces: []workspaceSpec{
		{id: id, output: "DP-1", name: "main", region: [4]int32{0, 0, 16, 8}, active: true},
		{id: 9, output: "DP-1", name: "off", region: [4]int32{0, 0, 16, 8}},
	}})
	s, comp := newSource(t, cfg)
	ctx := context.Background()

	st, err := s.BeginSession(ctx, ports.Target{WorkspaceID: id}, true)
	require.NoError(t, err)
	require.Equal(t, ports.Target{OutputID: 1, WorkspaceID: id}, st.Target, "a workspace target has a zero region")
	require.Equal(t, ports.Region{Width: 16, Height: 8}, st.Region)

	// The chain the application runs: Begin, then Capture(state.Target).
	f, err := s.Capture(ctx, st.Target)
	require.NoError(t, err)
	require.Equal(t, [2]int{16, 8}, [2]int{f.Width, f.Height})
	require.EqualValues(t, id, comp.snap().workspace)

	f, err = s.Capture(ctx, ports.Target{WorkspaceID: id}) // OutputID may be omitted
	require.NoError(t, err)
	require.Equal(t, [2]int{16, 8}, [2]int{f.Width, f.Height})
	require.Equal(t, [4]int32{0, 0, 16, 8}, comp.snap().lastRegion)
	require.Equal(t, [4]int32{}, comp.snap().requested, "a workspace session requests no region")

	// The compositor moves the workspace: the next frame follows the live region.
	comp.pushState([4]int32{4, 2, 8, 4}, id)
	f, err = s.Capture(ctx, st.Target) // the same target, still valid
	require.NoError(t, err)
	require.Equal(t, [2]int{8, 4}, [2]int{f.Width, f.Height})
	require.Equal(t, [4]int32{4, 2, 8, 4}, comp.snap().lastRegion)

	// A workspace other than the session's needs a private render path.
	_, err = s.Capture(ctx, ports.Target{WorkspaceID: 9})
	require.ErrorIs(t, err, ErrSessionUnsupported)
	_, err = s.Capture(ctx, ports.Target{WorkspaceID: id, Region: ports.Region{X: 1, Y: 1, Width: 1, Height: 1}})
	require.ErrorIs(t, err, ports.ErrInvalidRegion)
}

func TestWorkspaceCaptureNeedsSession(t *testing.T) {
	s, _ := newSource(t, privateCfg(privateConfig{workspaces: []workspaceSpec{
		{id: 3, output: "DP-1", region: [4]int32{0, 0, 16, 8}, active: true},
	}}))
	_, err := s.Capture(context.Background(), ports.Target{WorkspaceID: 3})
	require.ErrorIs(t, err, ErrSessionUnsupported)
}

// The compositor serves a workspace that is not on screen: the session is
// requested by identity with a zero region, whatever the inventory says.
func TestBeginHiddenWorkspace(t *testing.T) {
	s, comp := newSource(t, privateCfg(privateConfig{workspaces: []workspaceSpec{
		{id: 3, output: "DP-1", region: [4]int32{0, 0, 16, 8}},
	}}))
	ctx := context.Background()
	st, err := s.BeginSession(ctx, ports.Target{WorkspaceID: 3}, true)
	require.NoError(t, err)
	require.Equal(t, ports.Target{OutputID: 1, WorkspaceID: 3}, st.Target)
	require.EqualValues(t, 3, comp.snap().workspace)
	require.Equal(t, [4]int32{}, comp.snap().requested)
	_, err = s.Capture(ctx, st.Target)
	require.NoError(t, err)
}

// The session stays pinned to its output and workspace whatever the inventory
// announces or removes afterwards.
func TestSessionPinnedAgainstInventoryChanges(t *testing.T) {
	cfg := privateCfg(privateConfig{workspaces: []workspaceSpec{
		{id: 3, output: "DP-1", region: [4]int32{0, 0, 16, 8}, active: true},
	}})
	cfg.outputs = append(cfg.outputs, outputSpec{name: "DP-2", width: 32, height: 16, scale: 1, version: 4})
	s, comp := newSource(t, cfg)
	ctx := context.Background()
	st, err := s.BeginSession(ctx, ports.Target{WorkspaceID: 3}, true)
	require.NoError(t, err)
	require.EqualValues(t, 1, st.Target.OutputID)

	comp.announceWorkspace(workspaceSpec{id: 3, output: "DP-2", region: [4]int32{0, 0, 32, 16}, active: true})
	comp.removeWorkspace(3)
	f, err := s.Capture(ctx, st.Target)
	require.NoError(t, err, "a removed workspace is not consulted")
	require.Equal(t, [2]int{16, 8}, [2]int{f.Width, f.Height}, "still the pinned output")
	f, err = s.Capture(ctx, ports.Target{OutputID: 1, WorkspaceID: 3})
	require.NoError(t, err)
	_, err = s.Capture(ctx, ports.Target{OutputID: 2, WorkspaceID: 3})
	require.ErrorIs(t, err, ports.ErrOutputNotFound)

	// The removal is followed by the stop: the last grant is never used after.
	comp.stopSession(uint32(StopWorkspaceGone))
	_, err = s.Capture(ctx, st.Target)
	require.ErrorIs(t, err, ErrSessionUnavailable)
	var stopped *SessionStoppedError
	require.ErrorAs(t, err, &stopped)
	require.Equal(t, StopWorkspaceGone, stopped.Reason)
}

func TestPausedSessionIsInactive(t *testing.T) {
	s, comp := newSource(t, privateCfg(privateConfig{pausedStart: true}))
	ctx := context.Background()
	st, err := s.BeginSession(ctx, ports.Target{OutputID: 1}, true)
	require.NoError(t, err)
	require.False(t, st.Active)
	captures := comp.stat(func(c *compositor) int { return c.captures })
	_, err = s.Capture(ctx, ports.Target{OutputID: 1})
	require.ErrorIs(t, err, ErrSessionInactive)
	require.Equal(t, captures, comp.stat(func(c *compositor) int { return c.captures }), "no capture while paused")

	comp.pushState([4]int32{0, 0, 16, 8}, 0) // resumes
	_, err = s.Capture(ctx, ports.Target{OutputID: 1})
	require.NoError(t, err)
}

func TestCaptureRetriesCompositorFailureOnce(t *testing.T) {
	cfg := privateCfg(privateConfig{})
	cfg.failCopies = 1
	s, comp := newSource(t, cfg)
	ctx := context.Background()
	_, err := s.BeginSession(ctx, ports.Target{OutputID: 1}, true)
	require.NoError(t, err)
	_, err = s.Capture(ctx, ports.Target{OutputID: 1})
	require.NoError(t, err)
	require.Equal(t, 2, comp.stat(func(c *compositor) int { return c.copies }))

	comp.mu.Lock()
	comp.cfg.failCopies = 5
	comp.mu.Unlock()
	before := comp.stat(func(c *compositor) int { return c.copies })
	_, err = s.Capture(ctx, ports.Target{OutputID: 1})
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrSessionUnavailable)
	require.Equal(t, before+2, comp.stat(func(c *compositor) int { return c.copies }), "one retry, no more")
}

func TestOutputCaptureUnderSession(t *testing.T) {
	s, comp := newSource(t, privateCfg(privateConfig{}))
	ctx := context.Background()
	_, err := s.BeginSession(ctx, ports.Target{OutputID: 1}, true)
	require.NoError(t, err)
	f, err := s.Capture(ctx, ports.Target{OutputID: 1})
	require.NoError(t, err)
	require.Equal(t, [2]int{16, 8}, [2]int{f.Width, f.Height})
	require.Equal(t, 1, comp.stat(func(c *compositor) int { return c.captures }))
}

func TestPingSession(t *testing.T) {
	s, comp := newSource(t, privateCfg(privateConfig{}))
	ctx := context.Background()
	require.ErrorIs(t, s.PingSession(ctx), ErrSessionUnavailable) // none active
	_, err := s.BeginSession(ctx, ports.Target{OutputID: 1}, true)
	require.NoError(t, err)
	require.NoError(t, s.PingSession(ctx))
	require.Equal(t, 1, comp.stat(func(c *compositor) int { return c.privPings }))
}

func TestCapturePingsOnlyWhenDue(t *testing.T) {
	s, comp := newSource(t, privateCfg(privateConfig{}))
	ctx := context.Background()
	_, err := s.BeginSession(ctx, ports.Target{OutputID: 1}, true)
	require.NoError(t, err)

	for range 3 {
		_, err = s.Capture(ctx, ports.Target{OutputID: 1})
		require.NoError(t, err)
	}
	require.Zero(t, comp.stat(func(c *compositor) int { return c.privPings }), "no ping inside the interval")

	s.pingEvery = 0
	for range 3 {
		_, err = s.Capture(ctx, ports.Target{OutputID: 1})
		require.NoError(t, err)
	}
	require.NoError(t, s.PingSession(ctx)) // round trip: every earlier ping was received
	require.Equal(t, 4, comp.stat(func(c *compositor) int { return c.privPings }))
}

func TestStopDuringSessionFailsCaptureAndPing(t *testing.T) {
	s, comp := newSource(t, privateCfg(privateConfig{}))
	ctx := context.Background()
	_, err := s.BeginSession(ctx, ports.Target{OutputID: 1}, true)
	require.NoError(t, err)
	_, err = s.Capture(ctx, ports.Target{OutputID: 1})
	require.NoError(t, err)

	comp.stopSession(3)
	// Dispatching the stop happens inside the operation that reads it.
	require.ErrorIs(t, s.PingSession(ctx), ErrSessionUnavailable)
	captures := comp.stat(func(c *compositor) int { return c.captures })
	_, err = s.Capture(ctx, ports.Target{OutputID: 1})
	require.ErrorIs(t, err, ErrSessionUnavailable)
	require.Equal(t, captures, comp.stat(func(c *compositor) int { return c.captures }), "no frame is requested after the stop")
	require.NoError(t, s.EndSession(ctx))
	// A new session can follow.
	_, err = s.BeginSession(ctx, ports.Target{OutputID: 1}, true)
	require.NoError(t, err)
}

func TestStoppedSessionStaysFailClosedUntilEnded(t *testing.T) {
	s, comp := newSource(t, privateCfg(privateConfig{}))
	ctx := context.Background()
	_, err := s.BeginSession(ctx, ports.Target{OutputID: 1}, true)
	require.NoError(t, err)
	comp.stopSession(uint32(StopWorkspaceGone))
	// The stop is read inside the first operation; a frame copied meanwhile is
	// discarded. Nothing is requested afterwards, and never a plain capture.
	_, err = s.Capture(ctx, ports.Target{OutputID: 1})
	require.ErrorIs(t, err, ErrSessionUnavailable)
	captures := comp.stat(func(c *compositor) int { return c.captures })
	for range 3 {
		_, err = s.Capture(ctx, ports.Target{OutputID: 1})
		require.ErrorIs(t, err, ErrSessionUnavailable)
	}
	var stopped *SessionStoppedError
	require.ErrorAs(t, err, &stopped)
	require.Equal(t, StopWorkspaceGone, stopped.Reason)
	require.Equal(t, captures, comp.stat(func(c *compositor) int { return c.captures }))
	_, err = s.BeginSession(ctx, ports.Target{OutputID: 1}, true)
	require.ErrorIs(t, err, ErrSessionActive, "the owner must end the stopped session first")
	require.NoError(t, s.EndSession(ctx))
	require.Equal(t, 1, comp.stat(func(c *compositor) int { return c.privDestroyed }))
	_, err = s.Capture(ctx, ports.Target{OutputID: 1}) // no session: plain capture again
	require.NoError(t, err)
}

func TestStopArrivingWithFrameDiscardsFrame(t *testing.T) {
	s, _ := newSource(t, privateCfg(privateConfig{stopOnPing: true}))
	ctx := context.Background()
	_, err := s.BeginSession(ctx, ports.Target{OutputID: 1}, true)
	require.NoError(t, err)
	s.pingEvery = 0
	_, err = s.Capture(ctx, ports.Target{OutputID: 1})
	require.ErrorIs(t, err, ErrSessionUnavailable)
	require.NoError(t, s.EndSession(ctx))
}

func TestRevisionGoingBackIsProtocolViolation(t *testing.T) {
	s, _ := newSource(t, privateCfg(privateConfig{badRevision: true, inactive: true}))
	ctx := context.Background()
	_, err := s.BeginSession(ctx, ports.Target{OutputID: 1}, true)
	require.NoError(t, err)
	require.ErrorIs(t, s.PingSession(ctx), ErrSessionUnavailable)
	_, err = s.Capture(ctx, ports.Target{OutputID: 1})
	require.ErrorIs(t, err, ErrSessionUnavailable)
}

func TestZeroRevisionIsProtocolViolation(t *testing.T) {
	s, _ := newSource(t, privateCfg(privateConfig{zeroRevision: true}))
	ctx := context.Background()
	_, err := s.BeginSession(ctx, ports.Target{OutputID: 1}, true)
	require.ErrorIs(t, err, ErrSessionUnavailable)
}

func TestMalformedTokenRefusesSession(t *testing.T) {
	for _, tok := range []string{"short", testToken[:63] + "G", testToken + "0"} {
		s, _ := newSource(t, privateCfg(privateConfig{token: tok}))
		_, err := s.BeginSession(context.Background(), ports.Target{OutputID: 1}, true)
		require.ErrorIs(t, err, ErrSessionUnavailable, tok)
	}
}

func TestTokenChangeIsProtocolViolation(t *testing.T) {
	s, _ := newSource(t, privateCfg(privateConfig{swapToken: true}))
	ctx := context.Background()
	_, err := s.BeginSession(ctx, ports.Target{OutputID: 1}, true)
	require.NoError(t, err)
	require.ErrorIs(t, s.PingSession(ctx), ErrSessionUnavailable)
	_, err = s.Capture(ctx, ports.Target{OutputID: 1})
	require.ErrorIs(t, err, ErrSessionUnavailable)
}

func TestSessionStateHidesToken(t *testing.T) {
	st := SessionState{Token: testToken, Active: true}
	for _, s := range []string{fmt.Sprint(st), fmt.Sprintf("%v", st), fmt.Sprintf("%+v", st), fmt.Sprintf("%#v", st)} {
		require.NotContains(t, s, testToken)
	}
}

func TestSessionOperationsAfterClose(t *testing.T) {
	s, _ := newSource(t, privateCfg(privateConfig{}))
	require.NoError(t, s.Close())
	ctx := context.Background()
	_, err := s.BeginSession(ctx, ports.Target{OutputID: 1}, true)
	require.ErrorIs(t, err, ports.ErrClosed)
	require.ErrorIs(t, s.PingSession(ctx), ports.ErrClosed)
	require.ErrorIs(t, s.EndSession(ctx), ports.ErrClosed)
	_, err = s.Workspaces(ctx)
	require.ErrorIs(t, err, ports.ErrClosed)
}

func TestBeginSessionCancelled(t *testing.T) {
	s, _ := newSource(t, privateCfg(privateConfig{}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.BeginSession(ctx, ports.Target{OutputID: 1}, true)
	require.ErrorIs(t, err, context.Canceled)
}

func secondConnection(t *testing.T, comp *compositor) (*wlturbo.Display, *core.Surface) {
	t.Helper()
	conn, err := net.Dial("unix", comp.path)
	require.NoError(t, err)
	display, err := wlturbo.ConnectFromConn(conn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = display.Close() })
	require.NoError(t, display.Roundtrip())
	wlComp := core.NewCompositor(display.Context())
	if _, err = display.Registry().BindNegotiated("wl_compositor", 4, wlComp); err != nil {
		return display, core.NewSurface(display.Context()) // peer without wl_compositor
	}
	surface, err := wlComp.CreateSurface()
	require.NoError(t, err)
	return display, surface
}

func TestAuthorizeLayerWaitsForAttached(t *testing.T) {
	comp := startCompositor(t, privateCfg(privateConfig{}))
	display, surface := secondConnection(t, comp)
	require.NoError(t, AuthorizeLayer(context.Background(), display, surface, testToken, nil))
	require.Equal(t, []string{testToken}, comp.privAttachedCopy())
}

func TestAuthorizeLayerAttachFailed(t *testing.T) {
	for reason, want := range map[int]AttachFailure{
		1: AttachUnknownToken, 2: AttachUnauthorized, 3: AttachTooManyLayers,
		4: AttachAlreadyAttached, 5: AttachSessionEnded, 6: AttachLayerDestroyed,
	} {
		comp := startCompositor(t, privateCfg(privateConfig{attachFail: reason}))
		display, surface := secondConnection(t, comp)
		err := AuthorizeLayer(context.Background(), display, surface, testToken, nil)
		require.ErrorIs(t, err, ErrAttachFailed)
		var failed *AttachFailedError
		require.ErrorAs(t, err, &failed)
		require.Equal(t, want, failed.Reason)
		require.Equal(t, want == AttachUnknownToken || want == AttachSessionEnded, errors.Is(err, ErrSessionUnavailable))
		require.Equal(t, want == AttachUnauthorized, errors.Is(err, ErrUnauthorized))
		require.NotContains(t, err.Error(), testToken)
	}
}

// Two attachments on one connection are told apart by their own objects: a
// detach of the first, arriving while the second authorizes, reaches only the
// first callback.
func TestAuthorizeLayerDetachedCallback(t *testing.T) {
	comp := startCompositor(t, privateCfg(privateConfig{}))
	display, first := secondConnection(t, comp)
	var got atomic.Pointer[LayerDetachedError]
	calls := make(chan struct{}, 4)
	require.NoError(t, AuthorizeLayer(context.Background(), display, first, testToken, func(err error) {
		var d *LayerDetachedError
		if errors.As(err, &d) {
			got.Store(d)
		}
		calls <- struct{}{}
	}))
	require.Nil(t, got.Load(), "no detach before it happens")

	comp.detachLayer(0, uint32(DetachSessionEnded))
	second := core.NewSurface(display.Context())
	otherCalls := 0
	require.NoError(t, AuthorizeLayer(context.Background(), display, second, testToken, func(error) { otherCalls++ }))
	// The reader goroutine of the GUI delivers the callback; here the test
	// drives the connection until it arrives.
	for len(calls) == 0 {
		require.NoError(t, display.Roundtrip())
	}
	d := got.Load()
	require.NotNil(t, d)
	require.Equal(t, DetachSessionEnded, d.Reason)
	require.ErrorIs(t, d, ErrSessionUnavailable)
	require.Zero(t, otherCalls)
	require.Len(t, calls, 1)
	require.NotContains(t, d.Error(), testToken)

	// The second layer's own detach reaches only its own callback.
	comp.detachLayer(1, uint32(DetachLayerDestroyed))
	require.NoError(t, display.Roundtrip())
	require.Equal(t, 1, otherCalls)
	require.Len(t, calls, 1)
}

func TestAuthorizeLayerContextCancelled(t *testing.T) {
	comp := startCompositor(t, privateCfg(privateConfig{attachSilent: true}))
	display, surface := secondConnection(t, comp)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- AuthorizeLayer(ctx, display, surface, testToken, nil) }()
	require.Eventually(t, func() bool { return len(comp.privAttachedCopy()) == 1 }, 5*time.Second, time.Millisecond)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	require.True(t, display.Closed())

	// A context that is already cancelled sends nothing.
	comp2 := startCompositor(t, privateCfg(privateConfig{}))
	display2, surface2 := secondConnection(t, comp2)
	require.ErrorIs(t, AuthorizeLayer(ctx, display2, surface2, testToken, nil), context.Canceled)
	require.Empty(t, comp2.privAttachedCopy())
}

func TestAuthorizeLayerCancelAfterSuccessKeepsConnection(t *testing.T) {
	comp := startCompositor(t, privateCfg(privateConfig{}))
	display, surface := secondConnection(t, comp)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, AuthorizeLayer(ctx, display, surface, testToken, nil))
	cancel() // the watcher is gone: the GUI owns the connection now
	require.NoError(t, display.Roundtrip(), "the server still answers")
	require.False(t, display.Closed())
}

// The deadline lands around the delayed answer. Whatever wins, the result and
// the connection agree: success leaves it open, a failure closed it.
func TestAuthorizeLayerDeadlineRacingTheAnswer(t *testing.T) {
	const delay = 2 * time.Millisecond
	var ok, failed int
	for range 100 {
		comp := startCompositor(t, privateCfg(privateConfig{attachDelay: delay}))
		display, surface := secondConnection(t, comp)
		err := authorizeLayer(context.Background(), display, surface, testToken, nil, delay)
		if err == nil {
			ok++
			require.False(t, display.Closed())
			continue
		}
		failed++
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.True(t, display.Closed())
	}
	t.Logf("success %d, deadline %d", ok, failed)
}

func TestAuthorizeLayerTimesOutSilently(t *testing.T) {
	comp := startCompositor(t, privateCfg(privateConfig{attachSilent: true}))
	display, surface := secondConnection(t, comp)
	err := authorizeLayer(context.Background(), display, surface, testToken, nil, 100*time.Millisecond)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.True(t, display.Closed())
}

func TestAuthorizeLayerReportsProtocolError(t *testing.T) {
	comp := startCompositor(t, privateCfg(privateConfig{attachError: true}))
	display, surface := secondConnection(t, comp)
	err := AuthorizeLayer(context.Background(), display, surface, testToken, nil)
	require.ErrorIs(t, err, wlturbo.ErrDisplayError)
	var derr *wlturbo.DisplayError
	require.ErrorAs(t, err, &derr)
	require.EqualValues(t, 2, derr.Code)
}

func TestAuthorizeLayerRejectsBadInput(t *testing.T) {
	require.Error(t, AuthorizeLayer(context.Background(), nil, nil, testToken, nil))
	comp := startCompositor(t, privateCfg(privateConfig{}))
	display, surface := secondConnection(t, comp)
	for _, tok := range []string{"", "has space", "new\nline", testToken[:63], testToken + "0", testToken[:63] + "G", testToken[:63] + "A"} {
		require.Error(t, AuthorizeLayer(context.Background(), display, surface, tok, nil), "%q", tok)
	}
	require.Empty(t, comp.privAttachedCopy())
}

func TestAuthorizeLayerWithoutGlobal(t *testing.T) {
	comp := startCompositor(t, compositorConfig{})
	display, surface := secondConnection(t, comp)
	require.ErrorIs(t, AuthorizeLayer(context.Background(), display, surface, testToken, nil), ErrSessionUnsupported)
}

// TestSessionLiveHeadless runs against a real compositor when
// NEFERCAP_TEST_WAYLAND_PRIVATE names its socket. It skips otherwise, and when
// that compositor lacks the private protocol.
func TestSessionLiveHeadless(t *testing.T) {
	socket := os.Getenv("NEFERCAP_TEST_WAYLAND_PRIVATE")
	if socket == "" {
		t.Skip("NEFERCAP_TEST_WAYLAND_PRIVATE is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := New(ctx, socket)
	require.NoError(t, err)
	defer s.Close()
	outs, err := s.Outputs(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, outs)
	st, err := s.BeginSession(ctx, ports.Target{OutputID: outs[0].ID}, false)
	if err != nil {
		require.ErrorIs(t, err, ErrSessionUnsupported)
		t.Skip("compositor has no private capture protocol")
	}
	require.True(t, st.Active)
	require.NotEmpty(t, st.Token)
	require.NoError(t, s.PingSession(ctx))
	_, err = s.Capture(ctx, ports.Target{OutputID: outs[0].ID})
	require.NoError(t, err)
	require.NoError(t, s.EndSession(ctx))
}

func TestGrantValidation(t *testing.T) {
	region := ports.Region{X: 2, Y: 1, Width: 4, Height: 3}
	for name, grant := range map[string][4]int32{
		"zero width":     {2, 1, 0, 3},
		"negative width": {2, 1, -4, 3},
		"oversized":      {0, 0, 1 << 20, 3},
		"sum overflow":   {2147483647, 1, 2147483647, 3},
		"outside region": {0, 0, 4, 3},
		"larger":         {2, 1, 5, 3},
	} {
		s, _ := newSource(t, privateCfg(privateConfig{stateRegion: &grant}))
		_, err := s.BeginSession(context.Background(), ports.Target{OutputID: 1, Region: region}, true)
		require.ErrorIs(t, err, ErrSessionUnavailable, name)
	}
	// A grant inside the requested region is fine, and so is any bounded grant
	// of a full-output request (its logical size is not known exactly).
	inside := [4]int32{3, 1, 2, 2}
	s, _ := newSource(t, privateCfg(privateConfig{stateRegion: &inside}))
	st, err := s.BeginSession(context.Background(), ports.Target{OutputID: 1, Region: region}, true)
	require.NoError(t, err)
	require.Equal(t, ports.Region{X: 3, Y: 1, Width: 2, Height: 2}, st.Region)
	zero := [4]int32{0, 0, 0, 0}
	s, _ = newSource(t, privateCfg(privateConfig{stateRegion: &zero}))
	_, err = s.BeginSession(context.Background(), ports.Target{OutputID: 1}, true)
	require.ErrorIs(t, err, ErrSessionUnavailable, "a zero grant is not the full output")
}

func TestLaterStateForOtherWorkspaceFailsClosed(t *testing.T) {
	s, comp := newSource(t, privateCfg(privateConfig{workspaces: []workspaceSpec{
		{id: 3, output: "DP-1", region: [4]int32{0, 0, 16, 8}, active: true},
	}}))
	ctx := context.Background()
	_, err := s.BeginSession(ctx, ports.Target{WorkspaceID: 3}, true)
	require.NoError(t, err)
	comp.pushState([4]int32{0, 0, 16, 8}, 4)
	require.ErrorIs(t, s.PingSession(ctx), ErrSessionUnavailable)
	_, err = s.Capture(ctx, ports.Target{WorkspaceID: 3})
	require.ErrorIs(t, err, ErrSessionUnavailable)
}

func TestGeometryKeepsChanging(t *testing.T) {
	s, comp := newSource(t, privateCfg(privateConfig{moveEachCapture: true, workspaces: []workspaceSpec{
		{id: 3, output: "DP-1", region: [4]int32{0, 0, 16, 8}, active: true},
	}}))
	ctx := context.Background()
	st, err := s.BeginSession(ctx, ports.Target{WorkspaceID: 3}, true)
	require.NoError(t, err)
	f, err := s.Capture(ctx, st.Target)
	require.ErrorIs(t, err, ErrSessionGeometryChanging)
	require.Zero(t, f.Width, "no stale frame")
	require.Equal(t, maxRegionRetries+1, comp.stat(func(c *compositor) int { return c.captures }))
	require.NoError(t, s.EndSession(ctx))
}

func TestHostileMetadataDisablesPrivateCapability(t *testing.T) {
	long := strings.Repeat("a", maxNameLen+1)
	var many []workspaceSpec
	for i := range maxWorkspaces + 1 {
		many = append(many, workspaceSpec{id: uint64(i + 1), output: "DP-1", region: [4]int32{0, 0, 16, 8}})
	}
	for name, ws := range map[string][]workspaceSpec{
		"control":   {{id: 1, output: "DP-1", name: "a\nb", region: [4]int32{0, 0, 16, 8}}},
		"bidi":      {{id: 1, output: "DP-1", name: "a\u202eb", region: [4]int32{0, 0, 16, 8}}},
		"utf8":      {{id: 1, output: "DP-1", name: "a\xffb", region: [4]int32{0, 0, 16, 8}}},
		"long":      {{id: 1, output: "DP-1", name: long, region: [4]int32{0, 0, 16, 8}}},
		"empty out": {{id: 1, output: "", name: "x", region: [4]int32{0, 0, 16, 8}}},
		"too many":  many,
	} {
		s, _ := newSource(t, privateCfg(privateConfig{workspaces: ws}))
		ctx := context.Background()
		_, err := s.Workspaces(ctx)
		require.ErrorIs(t, err, ErrServerMetadata, name)
		require.ErrorIs(t, err, ErrSessionUnavailable, name)
		_, err = s.BeginSession(ctx, ports.Target{OutputID: 1}, true)
		require.ErrorIs(t, err, ErrServerMetadata, name)
		_, err = s.Capture(ctx, ports.Target{OutputID: 1}) // standard capture is untouched
		require.NoError(t, err, name)
	}
	// Legitimate unicode names are fine.
	s, _ := newSource(t, privateCfg(privateConfig{workspaces: []workspaceSpec{
		{id: 1, output: "DP-1", name: "espace de travail é ✓", region: [4]int32{0, 0, 16, 8}},
	}}))
	list, err := s.Workspaces(context.Background())
	require.NoError(t, err)
	require.Len(t, list, 1)
}

func TestSessionStateJSONOmitsToken(t *testing.T) {
	st := SessionState{Token: testToken, Revision: 3, Active: true}
	for _, v := range []any{st, &st, struct{ S SessionState }{st}, map[string]any{"session": st}} {
		b, err := json.Marshal(v)
		require.NoError(t, err)
		require.NotContains(t, string(b), testToken)
		require.NotContains(t, strings.ToLower(string(b)), "token")
	}
}

// layerScript runs AuthorizeLayer against a peer that answers with events.
func layerScript(t *testing.T, script [][2]uint32, onDetached func(error)) (*wlturbo.Display, error) {
	t.Helper()
	comp := startCompositor(t, privateCfg(privateConfig{attachScript: script}))
	display, surface := secondConnection(t, comp)
	return display, AuthorizeLayer(context.Background(), display, surface, testToken, onDetached)
}

func TestLayerDetachedBeforeAckIsViolation(t *testing.T) {
	display, err := layerScript(t, [][2]uint32{{2, 0}}, nil)
	require.ErrorIs(t, err, ErrSessionUnavailable)
	require.True(t, display.Closed())
}

// After a refusal the attachment object is destroyed: a late event for it is
// dropped, it is neither a violation nor a reason to close the connection.
func TestLateEventOnRetiredLayerIsDropped(t *testing.T) {
	display, err := layerScript(t, [][2]uint32{{1, 0}, {1, 0}}, nil)
	var failed *AttachFailedError
	require.ErrorAs(t, err, &failed)
	require.NoError(t, display.Roundtrip())
	require.False(t, display.Closed())
}

func TestLayerStateMachineViolationsAfterAck(t *testing.T) {
	for name, script := range map[string][][2]uint32{
		"attached twice":       {{0, 0}, {0, 0}},
		"failed after attach":  {{0, 0}, {1, 0}},
		"detached then attach": {{0, 0}, {2, 0}, {0, 0}},
	} {
		got := make(chan error, 4)
		display, err := layerScript(t, script, func(e error) { got <- e })
		require.NoError(t, err, name)
		for range 3 {
			_ = display.Roundtrip() // the reader of the GUI connection
		}
		if name == "detached then attach" {
			var d *LayerDetachedError
			require.ErrorAs(t, recv(t, got), &d, name)
			require.True(t, display.Closed(), "the late event closes the connection")
			require.Empty(t, got, "the callback ran once")
			continue
		}
		require.ErrorIs(t, recv(t, got), ErrSessionUnavailable, name)
		require.True(t, display.Closed(), name)
		require.Empty(t, got, "reported once")
	}
}

func TestAuthorizeLayerRejectsForeignSurface(t *testing.T) {
	comp := startCompositor(t, privateCfg(privateConfig{}))
	display, _ := secondConnection(t, comp)
	_, foreign := secondConnection(t, startCompositor(t, privateCfg(privateConfig{})))
	require.Error(t, AuthorizeLayer(context.Background(), display, foreign, testToken, nil))
	require.Empty(t, comp.privAttachedCopy())
}

func recv(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("no callback")
		return nil
	}
}
