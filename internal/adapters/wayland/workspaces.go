package wayland

import (
	"cmp"
	"context"
	"slices"
	"strconv"
	"unicode"
	"unicode/utf8"

	extworkspace "github.com/bnema/wlturbo/protocol/workspace"

	"github.com/bnema/nefercap/internal/ports"
)

// ext-workspace-v1 client. Only the state needed to list workspaces and to
// name one as a capture source is kept; nothing is ever requested from the
// compositor (no activate, no commit).

const (
	workspaceVersion = 1
	maxWorkspaces    = 256 // bound of the inventory
	maxNameLen       = 256 // bytes, for workspace names
	maxGroupOutputs  = 64

	maxInterned = 4 * maxWorkspaces // id strings remembered across removals
)

// workspaceState is the ext-workspace inventory of a Source.
type workspaceState struct {
	mgr      *extworkspace.ExtWorkspaceManager // nil when the compositor has no ext-workspace
	handles  map[uint32]*workspace             // by proxy ID
	byID     map[uint64]*workspace             // by Source-local identity
	groups   map[uint32]*workspaceGroup
	next     uint64            // last identity handed out; identities start at 1
	interned map[string]uint64 // id string to identity, bounded by maxInterned
	overflow bool              // the compositor announced more objects than maxWorkspaces
	shown    uint64            // workspace last verified as displayed, without the extension
}

// watchWorkspaces subscribes to the creation events of the manager.
func (s *Source) watchWorkspaces(m *extworkspace.ExtWorkspaceManager) {
	m.OnWorkspaceGroup(func(proxy *extworkspace.ExtWorkspaceGroupHandle) {
		// wlturbo registered the proxy already, which absorbs its events.
		if len(s.ws.groups) >= maxWorkspaces {
			s.ws.overflow = true
			return
		}
		g := &workspaceGroup{proxy: proxy, outputs: make(map[uint32]bool)}
		s.ws.groups[proxy.ID()] = g
		s.watchGroup(g)
	})
	m.OnWorkspace(func(proxy *extworkspace.ExtWorkspaceHandle) {
		if len(s.ws.handles) >= maxWorkspaces {
			s.ws.overflow = true
			return
		}
		w := &workspace{proxy: proxy}
		s.ws.handles[proxy.ID()] = w
		s.watchWorkspace(w)
	})
	m.OnDone(s.ws.assignIDs)
}

type workspaceGroup struct {
	proxy   *extworkspace.ExtWorkspaceGroupHandle
	outputs map[uint32]bool // wl_output proxy IDs
}

func (s *Source) watchGroup(g *workspaceGroup) {
	g.proxy.OnOutputEnter(func(id uint32) {
		if len(g.outputs) < maxGroupOutputs {
			g.outputs[id] = true
		}
	})
	g.proxy.OnOutputLeave(func(id uint32) { delete(g.outputs, id) })
	g.proxy.OnWorkspaceEnter(func(id uint32) {
		if w := s.ws.handles[id]; w != nil {
			w.group = g
		}
	})
	g.proxy.OnWorkspaceLeave(func(id uint32) {
		if w := s.ws.handles[id]; w != nil && w.group == g {
			w.group = nil
		}
	})
	g.proxy.OnRemoved(func() {
		delete(s.ws.groups, g.proxy.ID())
		for _, w := range s.ws.handles {
			if w.group == g {
				w.group = nil
			}
		}
		_ = g.proxy.Destroy()
	})
}

// workspace is one ext_workspace_handle_v1. Its proxy is the argument of
// create_workspace_source.
type workspace struct {
	proxy   *extworkspace.ExtWorkspaceHandle
	idStr   string // the id event: empty for a temporary workspace
	name    string
	state   uint32
	group   *workspaceGroup
	id      uint64 // Source-local identity, assigned at the first done
	removed bool
	frame   *workspaceFrame // NeferWL's extension only; nil until asked for
}

// watchWorkspace subscribes to the events of w. Coordinates are not used:
// nothing here depends on the grid.
func (s *Source) watchWorkspace(w *workspace) {
	w.proxy.OnId(func(id string) {
		if w.idStr == "" {
			w.idStr = id
		}
	})
	w.proxy.OnName(func(n string) {
		if validName(n) {
			w.name = n
		}
	})
	w.proxy.OnState(func(state uint32) { w.state = state })
	w.proxy.OnRemoved(func() {
		w.removed = true
		s.releaseWorkspaceFrame(w)
		delete(s.ws.handles, w.proxy.ID())
		if w.id != 0 && s.ws.byID[w.id] == w {
			delete(s.ws.byID, w.id)
		}
		_ = w.proxy.Destroy()
	})
}

// assignIDs gives every workspace announced since the last done its
// identity. The id event is an opaque string: it is interned into a number
// local to this Source, from one counter, so identities cannot collide and the
// string is never parsed. The same string keeps its identity while the Source
// lives (up to maxInterned strings). A workspace without an id, or whose id
// is already held by a live workspace, is temporary: it gets a fresh number
// that is not remembered.
func (ws *workspaceState) assignIDs() {
	fresh := make([]*workspace, 0, len(ws.handles))
	for _, w := range ws.handles {
		if w.id == 0 && !w.removed {
			fresh = append(fresh, w)
		}
	}
	// Map order is random: numbers follow the announcement order.
	slices.SortFunc(fresh, func(a, b *workspace) int { return cmp.Compare(a.proxy.ID(), b.proxy.ID()) })
	for _, w := range fresh {
		w.id = ws.intern(w.idStr)
		ws.byID[w.id] = w
	}
}

func (ws *workspaceState) intern(idStr string) uint64 {
	if idStr != "" {
		if id, ok := ws.interned[idStr]; ok {
			if ws.byID[id] == nil {
				return id
			}
		} else if len(ws.interned) < maxInterned {
			ws.next++
			ws.interned[idStr] = ws.next
			return ws.next
		}
	}
	ws.next++
	return ws.next
}

// validName accepts a bounded, valid UTF-8, single-line name without control
// or bidirectional formatting characters.
func validName(n string) bool {
	if len(n) > maxNameLen || !utf8.ValidString(n) {
		return false
	}
	for _, r := range n {
		switch {
		case unicode.IsControl(r), r == 0x2028, r == 0x2029, r == 0x061C, r == 0x200E, r == 0x200F,
			r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069:
			return false
		}
	}
	return true
}

func outputName(o *output) string {
	if o.name == "" { // wl_output older than version 4 has no name
		return "output-" + strconv.FormatUint(uint64(o.id), 10)
	}
	return o.name
}

// forgetOutput drops a removed wl_output proxy from every group.
func (ws *workspaceState) forgetOutput(proxyID uint32) {
	for _, g := range ws.groups {
		delete(g.outputs, proxyID)
	}
}

// bindWorkspaces binds ext-workspace-v1 when the compositor offers it.
func (s *Source) bindWorkspaces() error {
	s.ws.handles = make(map[uint32]*workspace)
	s.ws.byID = make(map[uint64]*workspace)
	s.ws.groups = make(map[uint32]*workspaceGroup)
	s.ws.interned = make(map[string]uint64)
	m := extworkspace.NewExtWorkspaceManager(s.wl)
	ok, err := s.bindOptional(extworkspace.ExtWorkspaceManagerInterface, workspaceVersion, m)
	if err != nil || !ok {
		return err
	}
	s.watchWorkspaces(m)
	s.ws.mgr = m
	return nil
}

// Workspaces lists the workspaces ext-workspace-v1 advertises, one entry per
// output of the workspace's group, ordered by identity. It returns nil when
// the compositor has no ext-workspace.
func (s *Source) Workspaces(ctx context.Context) ([]ports.Workspace, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if err := s.begin(ctx); err != nil {
		return nil, err
	}
	err := s.settle()
	if err == nil {
		// The frame of a workspace comes from NeferWL's extension: one
		// object per workspace, answered at creation.
		var asked bool
		if asked, err = s.requestFrames(); asked && err == nil {
			err = s.roundtrip()
		}
	}
	var list []ports.Workspace
	if err == nil {
		list = s.workspaceList()
	}
	if err = s.finish(ctx, err); err != nil {
		return nil, err
	}
	return list, nil
}

func (s *Source) workspaceList() []ports.Workspace {
	if s.ws.mgr == nil || s.ws.overflow {
		return nil
	}
	var list []ports.Workspace
	for _, w := range s.ws.byID {
		if w.removed || w.group == nil {
			continue
		}
		for proxyID := range w.group.outputs {
			o := s.byProxy[proxyID]
			if o == nil {
				continue
			}
			list = append(list, ports.Workspace{
				ID: w.id, OutputID: o.id, Name: w.name,
				Region: workspaceRegion(w, o),
				Active: w.state&extworkspace.STATE_ACTIVE != 0,
			})
		}
	}
	slices.SortFunc(list, func(a, b ports.Workspace) int {
		return cmp.Or(cmp.Compare(a.ID, b.ID), cmp.Compare(a.OutputID, b.OutputID))
	})
	return list
}

// workspaceRegion is where the workspace is on o, in output-local logical
// pixels: its frame from NeferWL's extension, clipped to the output, else the
// whole output (ext-workspace carries no geometry).
func workspaceRegion(w *workspace, o *output) ports.Region {
	lw, lh := o.logicalSize()
	whole := ports.Region{Width: lw, Height: lh}
	r, ok := w.frame.region()
	if !ok {
		return whole
	}
	x0, y0 := max(r.X, 0), max(r.Y, 0)
	x1, y1 := min(r.X+r.Width, lw), min(r.Y+r.Height, lh)
	if x1 <= x0 || y1 <= y0 {
		return whole
	}
	return ports.Region{X: x0, Y: y0, Width: x1 - x0, Height: y1 - y0}
}

// workspaceKey resolves a workspace target. With NeferWL's extension the
// session follows the workspace itself, shown or not. Without it the
// workspace is captured as its output, and only while it is displayed: that is
// checked when the workspace is first targeted, so the recording then follows
// the output whatever it shows, and never starts on hidden content.
func (s *Source) workspaceKey(t ports.Target, out *output) (sessionKey, ports.Region, error) {
	if t.Region != (ports.Region{}) {
		return sessionKey{}, ports.Region{}, ports.ErrInvalidRegion
	}
	w := s.ws.byID[t.WorkspaceID]
	if w == nil || w.removed {
		return sessionKey{}, ports.Region{}, ports.ErrWorkspaceUnavailable
	}
	if s.ext.source != nil {
		return sessionKey{output: out.id, workspace: w.id}, ports.Region{}, nil
	}
	if s.ws.shown != w.id {
		if err := s.roundtrip(); err != nil {
			return sessionKey{}, ports.Region{}, err
		}
		if w.removed || w.state&extworkspace.STATE_ACTIVE == 0 || w.group == nil || !w.group.outputs[out.proxy.ID()] {
			return sessionKey{}, ports.Region{}, ports.ErrWorkspaceUnavailable
		}
		s.ws.shown = w.id
	}
	return sessionKey{output: out.id}, ports.Region{}, nil
}
