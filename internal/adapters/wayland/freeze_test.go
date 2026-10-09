package wayland

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bnema/nefercap/internal/adapters/wayland/testserver"
	"github.com/bnema/nefercap/internal/ports"
)

// The test compositor paints the frame sequence into every pixel, so a
// frozen capture keeps the sequence of the Freeze while live ones advance.
func TestFreezeServesTheFrozenScreen(t *testing.T) {
	ctx := context.Background()
	s, srv := newSource(t, testserver.Config{Outputs: bigOutput()})
	out := firstOutput(t, s)
	frozen, err := s.Freeze(ctx, []uint32{out.ID})
	require.NoError(t, err)
	f := frozen[out.ID]
	require.NoError(t, f.Validate())
	require.Equal(t, [3]int{64, 48, 64 * 4}, [3]int{f.Width, f.Height, f.Stride})
	captures := srv.Stats().Captures

	whole, err := s.Capture(ctx, ports.Target{OutputID: out.ID})
	require.NoError(t, err)
	want := testserver.Pixel(5, 3, 1)
	require.Equal(t, want[:], whole.Row(3)[5*4:6*4])

	region, err := s.Capture(ctx, ports.Target{OutputID: out.ID, Region: ports.Region{X: 10, Y: 5, Width: 20, Height: 8}})
	require.NoError(t, err)
	require.Equal(t, [2]int{20, 8}, [2]int{region.Width, region.Height})
	want = testserver.Pixel(10, 5, 1)
	require.Equal(t, want[:], region.Row(0)[:4])
	require.Equal(t, captures, srv.Stats().Captures, "frozen captures read no new frame")

	s.Thaw()
	live, err := s.Capture(ctx, ports.Target{OutputID: out.ID})
	require.NoError(t, err)
	want = testserver.Pixel(5, 3, 2)
	require.Equal(t, want[:], live.Row(3)[5*4:6*4], "Thaw captures the live screen again")
}

// Freeze ends the live session and frees the shared buffers: the compositor
// stops streaming and only the frozen copy stays in memory.
func TestFreezeReleasesLiveResources(t *testing.T) {
	s, srv := newSource(t, testserver.Config{Outputs: bigOutput()})
	_, err := s.Freeze(context.Background(), []uint32{firstOutput(t, s).ID})
	require.NoError(t, err)
	require.NoError(t, s.roundtrip())
	st := srv.Stats()
	require.Equal(t, st.Sessions, st.DestroyedSessions)
	require.Equal(t, st.Buffers, st.DestroyedBuffers)
	require.Nil(t, s.bufs[0].data)
}

// A rotated output cannot be frozen: it stays live instead of failing the
// selector, and its capture reports the transform as before.
func TestFreezeLeavesRotatedOutputsLive(t *testing.T) {
	s, _ := newSource(t, testserver.Config{Transform: 1})
	out := firstOutput(t, s)
	frozen, err := s.Freeze(context.Background(), []uint32{out.ID})
	require.NoError(t, err)
	require.Empty(t, frozen)
	_, err = s.Capture(context.Background(), ports.Target{OutputID: out.ID})
	require.ErrorIs(t, err, ports.ErrUnsupportedTransform)
}

// Thaw keeps the copies of the listed outputs; the others capture live.
func TestThawKeepsListedOutputs(t *testing.T) {
	ctx := context.Background()
	s, _ := newSource(t, testserver.Config{Outputs: []testserver.OutputSpec{
		{Name: "A-1", Width: 8, Height: 4, Scale: 1}, {Name: "B-2", Width: 8, Height: 4, Scale: 1},
	}})
	outs, err := s.Outputs(ctx)
	require.NoError(t, err)
	_, err = s.Freeze(ctx, []uint32{outs[0].ID, outs[1].ID}) // frames 1 and 2
	require.NoError(t, err)
	s.Thaw(outs[0].ID)
	kept, err := s.Capture(ctx, ports.Target{OutputID: outs[0].ID})
	require.NoError(t, err)
	want := testserver.Pixel(0, 0, 1)
	require.Equal(t, want[:], kept.Row(0)[:4])
	live, err := s.Capture(ctx, ports.Target{OutputID: outs[1].ID})
	require.NoError(t, err)
	want = testserver.Pixel(0, 0, 3)
	require.Equal(t, want[:], live.Row(0)[:4])
}

func TestCloseDropsFrozenFrames(t *testing.T) {
	s, _ := newSource(t, testserver.Config{})
	out := firstOutput(t, s)
	_, err := s.Freeze(context.Background(), []uint32{out.ID})
	require.NoError(t, err)
	require.NoError(t, s.Close())
	require.Nil(t, s.frozen)
	_, err = s.Capture(context.Background(), ports.Target{OutputID: out.ID})
	require.ErrorIs(t, err, ports.ErrClosed)
}

func TestFreezeWorkspaceIsItsRegionOfTheFrozenOutput(t *testing.T) {
	cfg := wsConfig(testserver.WorkspaceSpec{ID: "7f3a-2", Name: "b", Output: "TEST-1", Active: true})
	cfg.NeferwlSource = true
	s, srv := newSource(t, cfg)
	srv.SetFrame("b", [4]int32{2, 1, 4, 2})
	ws := listWorkspaces(t, s)
	require.Len(t, ws, 1)
	out := firstOutput(t, s)
	_, err := s.Freeze(context.Background(), []uint32{out.ID})
	require.NoError(t, err)
	f, err := s.Capture(context.Background(), ports.Target{OutputID: out.ID, WorkspaceID: ws[0].ID})
	require.NoError(t, err)
	require.Equal(t, [2]int{4, 2}, [2]int{f.Width, f.Height})
	want := testserver.Pixel(2, 1, 1)
	require.Equal(t, want[:], f.Row(0)[:4])
	require.Equal(t, "output", srv.Stats().LastSource, "a frozen workspace is cut from the output frame")

	_, err = s.Capture(context.Background(), ports.Target{OutputID: out.ID, WorkspaceID: 99})
	require.ErrorIs(t, err, ports.ErrWorkspaceUnavailable)
}

// The copy shows the workspace that was displayed: a hidden one is
// unavailable, as in a live capture without NeferWL's extension.
func TestFreezeHiddenWorkspaceIsUnavailable(t *testing.T) {
	s, _ := newSource(t, wsConfig(
		testserver.WorkspaceSpec{ID: "1", Name: "a", Output: "TEST-1", Active: true},
		testserver.WorkspaceSpec{ID: "2", Name: "b", Output: "TEST-1"},
	))
	ws := listWorkspaces(t, s)
	require.Len(t, ws, 2)
	out := firstOutput(t, s)
	_, err := s.Freeze(context.Background(), []uint32{out.ID})
	require.NoError(t, err)
	for _, w := range ws {
		_, err := s.Capture(context.Background(), ports.Target{OutputID: out.ID, WorkspaceID: w.ID})
		if w.Active {
			require.NoError(t, err)
		} else {
			require.ErrorIs(t, err, ports.ErrWorkspaceUnavailable)
		}
	}
}

func TestFreezeUnknownOutput(t *testing.T) {
	s, _ := newSource(t, testserver.Config{})
	_, err := s.Freeze(context.Background(), []uint32{12345})
	require.ErrorIs(t, err, ports.ErrOutputNotFound)

	out := firstOutput(t, s)
	_, err = s.Freeze(context.Background(), []uint32{out.ID})
	require.NoError(t, err)
	_, err = s.Capture(context.Background(), ports.Target{OutputID: 12345})
	require.ErrorIs(t, err, ports.ErrOutputNotFound)
}

func TestOwnedCopyDropsStridePadding(t *testing.T) {
	f := ports.Frame{Pixels: make([]byte, 2*16), Width: 3, Height: 2, Stride: 16, Format: ports.XRGB8888}
	for i := range f.Pixels {
		f.Pixels[i] = byte(i)
	}
	c := ownedCopy(f)
	require.NoError(t, c.Validate())
	require.Equal(t, 12, c.Stride)
	require.Equal(t, f.Row(1), c.Row(1))
	f.Pixels[16] = 0xee
	require.NotEqual(t, f.Row(1), c.Row(1), "the copy owns its storage")
}
