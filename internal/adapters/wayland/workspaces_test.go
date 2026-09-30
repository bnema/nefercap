package wayland

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bnema/nefercap/internal/adapters/wayland/testserver"
	"github.com/bnema/nefercap/internal/ports"
)

func wsConfig(ws ...testserver.WorkspaceSpec) testserver.Config {
	return testserver.Config{
		Outputs:       []testserver.OutputSpec{{Name: "TEST-1", Width: 8, Height: 4, Scale: 1}},
		HasWorkspaces: true, Workspaces: ws,
	}
}

func listWorkspaces(t *testing.T, s *Source) []ports.Workspace {
	t.Helper()
	ws, err := s.Workspaces(context.Background())
	require.NoError(t, err)
	return ws
}

func TestWorkspacesAbsent(t *testing.T) {
	s, _ := newSource(t, testserver.Config{})
	require.Nil(t, listWorkspaces(t, s))
	require.False(t, s.Capabilities().Workspaces)
}

func TestWorkspacesListing(t *testing.T) {
	s, _ := newSource(t, wsConfig(
		testserver.WorkspaceSpec{ID: "name:dev", Name: "dev", Output: "TEST-1", Active: true},
		testserver.WorkspaceSpec{ID: "7f3a-12", Name: "2", Output: "TEST-1"},
		testserver.WorkspaceSpec{ID: "7f3a-13", Name: "3", Output: "TEST-1", Hidden: true},
	))
	out := firstOutput(t, s)
	ws := listWorkspaces(t, s)
	require.Equal(t, []ports.Workspace{
		{ID: 1, OutputID: out.ID, Name: "dev", Region: ports.Region{Width: 8, Height: 4}, Active: true},
		{ID: 2, OutputID: out.ID, Name: "2", Region: ports.Region{Width: 8, Height: 4}},
		{ID: 3, OutputID: out.ID, Name: "3", Region: ports.Region{Width: 8, Height: 4}},
	}, ws, "id strings are interned into Source-local numbers, in announcement order")
	require.Equal(t, ports.Capabilities{Workspaces: true}, s.Capabilities(), "no hidden workspaces without the extension")
}

func TestWorkspaceIdentityOpaqueAndTemporary(t *testing.T) {
	s, _ := newSource(t, wsConfig(
		testserver.WorkspaceSpec{ID: "name:main", Name: "main", Output: "TEST-1", Active: true},
		testserver.WorkspaceSpec{Name: "tmp1", Output: "TEST-1"},
		testserver.WorkspaceSpec{Name: "tmp2", Output: "TEST-1"},
	))
	ws := listWorkspaces(t, s)
	require.Len(t, ws, 3)
	seen := map[uint64]bool{}
	for _, w := range ws {
		require.NotZero(t, w.ID)
		require.False(t, seen[w.ID], "identities are unique")
		seen[w.ID] = true
	}
	again := listWorkspaces(t, s)
	require.Equal(t, ws, again, "identities are stable for the connection")
}

func TestWorkspaceStateChangesAndRemoval(t *testing.T) {
	s, srv := newSource(t, wsConfig(
		testserver.WorkspaceSpec{ID: "name:a", Name: "a", Output: "TEST-1", Active: true},
		testserver.WorkspaceSpec{ID: "7f3a-2", Name: "b", Output: "TEST-1"},
	))
	srv.SetActive("a", false)
	srv.SetActive("b", true)
	ws := listWorkspaces(t, s)
	require.False(t, ws[0].Active)
	require.True(t, ws[1].Active)
	srv.RemoveWorkspace("a")
	ws = listWorkspaces(t, s)
	require.Len(t, ws, 1)
	require.Equal(t, "b", ws[0].Name)
}

func TestWorkspaceCaptureActiveOnlyWithoutExtension(t *testing.T) {
	s, srv := newSource(t, wsConfig(
		testserver.WorkspaceSpec{ID: "name:a", Name: "a", Output: "TEST-1", Active: true},
		testserver.WorkspaceSpec{ID: "7f3a-2", Name: "b", Output: "TEST-1"},
	))
	out := firstOutput(t, s)
	f, err := s.Capture(context.Background(), ports.Target{OutputID: out.ID, WorkspaceID: 1})
	require.NoError(t, err)
	require.Equal(t, [2]int{8, 4}, [2]int{f.Width, f.Height})
	require.Equal(t, "output", srv.Stats().LastSource, "the active workspace is captured as its output")
	_, err = s.Capture(context.Background(), ports.Target{OutputID: out.ID, WorkspaceID: 2})
	require.ErrorIs(t, err, ports.ErrWorkspaceUnavailable, "a hidden workspace is never captured as the output")
	_, err = s.Capture(context.Background(), ports.Target{OutputID: out.ID, WorkspaceID: 99})
	require.ErrorIs(t, err, ports.ErrWorkspaceUnavailable)
	_, err = s.Capture(context.Background(), ports.Target{OutputID: out.ID, WorkspaceID: 1, Region: ports.Region{Width: 1, Height: 1}})
	require.ErrorIs(t, err, ports.ErrInvalidRegion)
}

func TestWorkspaceCaptureUsesExtensionSource(t *testing.T) {
	cfg := wsConfig(
		testserver.WorkspaceSpec{ID: "name:a", Name: "a", Output: "TEST-1", Active: true},
		testserver.WorkspaceSpec{ID: "7f3a-2", Name: "b", Output: "TEST-1", Hidden: true},
	)
	cfg.NeferwlSource = true
	s, srv := newSource(t, cfg)
	require.Equal(t, ports.Capabilities{Workspaces: true}, s.Capabilities())
	out := firstOutput(t, s)
	f, err := s.Capture(context.Background(), ports.Target{OutputID: out.ID, WorkspaceID: 2})
	require.NoError(t, err, "a hidden workspace is captured through the extension")
	require.NoError(t, f.Validate())
	require.Equal(t, "workspace", srv.Stats().LastSource)
	// it follows the workspace: still captured after the active one changes
	srv.SetActive("a", false)
	srv.SetActive("b", true)
	_, err = s.Capture(context.Background(), ports.Target{OutputID: out.ID, WorkspaceID: 2})
	require.NoError(t, err)
	require.Equal(t, 1, srv.Stats().Sessions, "the same session serves the workspace")
}

// With NeferWL's extension a workspace lists its own frame, not the output.
func TestWorkspaceRegionIsTheFrameFromTheExtension(t *testing.T) {
	cfg := wsConfig(
		testserver.WorkspaceSpec{ID: "name:a", Name: "a", Output: "TEST-1", Active: true, Frame: [4]int32{2, 1, 4, 2}},
		testserver.WorkspaceSpec{ID: "7f3a-2", Name: "b", Output: "TEST-1", Hidden: true, Frame: [4]int32{0, 0, 8, 4}},
		testserver.WorkspaceSpec{ID: "7f3a-3", Name: "c", Output: "TEST-1", Frame: [4]int32{6, 3, 9, 9}},
		testserver.WorkspaceSpec{ID: "7f3a-4", Name: "d", Output: "TEST-1"},
	)
	cfg.NeferwlSource = true
	s, srv := newSource(t, cfg)
	regions := func() map[string]ports.Region {
		m := map[string]ports.Region{}
		for _, w := range listWorkspaces(t, s) {
			m[w.Name] = w.Region
		}
		return m
	}
	require.Equal(t, map[string]ports.Region{
		"a": {X: 2, Y: 1, Width: 4, Height: 2},
		"b": {Width: 8, Height: 4},
		"c": {X: 6, Y: 3, Width: 2, Height: 1}, // clipped to the output
		"d": {Width: 8, Height: 4},             // no frame sent: the whole output
	}, regions())
	require.Equal(t, 4, srv.Stats().WorkspaceFrames)
	// Listing again asks for nothing more, and a change is followed.
	srv.SetFrame("a", [4]int32{1, 1, 3, 3})
	require.Equal(t, ports.Region{X: 1, Y: 1, Width: 3, Height: 3}, regions()["a"])
	require.Equal(t, 4, srv.Stats().WorkspaceFrames)
	// A removed workspace releases its frame object.
	srv.RemoveWorkspace("a")
	require.NotContains(t, regions(), "a")
	require.Eventually(t, func() bool { return srv.Stats().DestroyedWorkspaceFrames == 1 }, 2*time.Second, time.Millisecond)
}

// An off-screen frame, or one that is nowhere on the output, falls back to the
// whole output rather than offering an empty workspace.
func TestWorkspaceRegionFallsBackToTheOutput(t *testing.T) {
	cfg := wsConfig(testserver.WorkspaceSpec{ID: "name:a", Name: "a", Output: "TEST-1", Active: true, Frame: [4]int32{20, 20, 4, 4}})
	cfg.NeferwlSource = true
	s, _ := newSource(t, cfg)
	require.Equal(t, ports.Region{Width: 8, Height: 4}, listWorkspaces(t, s)[0].Region)
}

func TestWorkspaceRegionIsTheOutputWithoutTheExtension(t *testing.T) {
	s, srv := newSource(t, wsConfig(testserver.WorkspaceSpec{ID: "name:a", Name: "a", Output: "TEST-1", Active: true, Frame: [4]int32{2, 1, 4, 2}}))
	require.Equal(t, ports.Region{Width: 8, Height: 4}, listWorkspaces(t, s)[0].Region)
	require.Zero(t, srv.Stats().WorkspaceFrames)
}

func TestWorkspaceLimit(t *testing.T) {
	var specs []testserver.WorkspaceSpec
	for i := range maxWorkspaces + 4 {
		specs = append(specs, testserver.WorkspaceSpec{ID: strconv.Itoa(i + 1), Name: "w", Output: "TEST-1"})
	}
	s, _ := newSource(t, wsConfig(specs...))
	require.Nil(t, listWorkspaces(t, s), "an inventory beyond the bound is rejected, not truncated")
}

// The id event is an opaque string: it is interned, never parsed.
func TestWorkspaceIdentityIsInterned(t *testing.T) {
	ws := &workspaceState{interned: map[string]uint64{}, byID: map[uint64]*workspace{}}
	a, b := ws.intern("7f3a-5"), ws.intern("name:5")
	require.NotEqual(t, a, b, "both id forms are just strings")
	require.Equal(t, a, ws.intern("7f3a-5"), "the same string keeps its identity")
	require.Equal(t, uint64(1), a, "identities come from one counter")
	require.Equal(t, uint64(2), b)
	require.NotContains(t, []uint64{a, b}, ws.intern("5"), "a bare decimal is another string too")
	ws.byID[a] = &workspace{}
	dup := ws.intern("7f3a-5")
	require.NotEqual(t, a, dup, "an id held by a live workspace is not shared")
	require.Equal(t, uint64(4), dup)
	t1, t2 := ws.intern(""), ws.intern("")
	require.NotEqual(t, t1, t2, "workspaces without an id are temporary")
	delete(ws.byID, a)
	require.Equal(t, a, ws.intern("7f3a-5"), "a returning workspace gets its identity back")
	require.False(t, validName("a\nb"))
	require.False(t, validName("\u202e"))
	require.True(t, validName("Bureau 2"))
}

func TestWorkspaceInternTableIsBounded(t *testing.T) {
	ws := &workspaceState{interned: map[string]uint64{}, byID: map[uint64]*workspace{}}
	for i := range maxInterned + 10 {
		ws.intern("p-" + strconv.Itoa(i))
	}
	require.Len(t, ws.interned, maxInterned, "the string table is bounded")
}

// Hotplug: an output that goes away leaves no stale group entry, and no
// workspace is listed on it.
func TestOutputHotplugPurgesWorkspaceAndXdgState(t *testing.T) {
	cfg := testserver.Config{
		Outputs: []testserver.OutputSpec{
			{Name: "A", Width: 8, Height: 4, Scale: 1, LogicalWidth: 8, LogicalHeight: 4},
			{Name: "B", Width: 16, Height: 8, Scale: 1, LogicalWidth: 16, LogicalHeight: 8},
		},
		HasWorkspaces: true,
		Workspaces:    []testserver.WorkspaceSpec{{ID: "name:a", Name: "a", Output: "A", Active: true}},
	}
	s, srv := newSource(t, cfg)
	ws := listWorkspaces(t, s)
	require.Len(t, ws, 2, "one entry per output of the group")
	srv.RemoveOutput("B")
	ws = listWorkspaces(t, s)
	require.Len(t, ws, 1)
	require.Equal(t, "A", outputNameOf(t, s, ws[0].OutputID))
	for _, g := range s.ws.groups {
		require.Len(t, g.outputs, 1, "the removed output proxy is purged from the group")
	}
	require.Equal(t, 1, srv.Stats().DestroyedXdgOutputs, "the zxdg_output_v1 of the removed output is destroyed")
}

func outputNameOf(t *testing.T, s *Source, id uint32) string {
	t.Helper()
	outs, err := s.Outputs(context.Background())
	require.NoError(t, err)
	for _, o := range outs {
		if o.ID == id {
			return o.Name
		}
	}
	return ""
}
