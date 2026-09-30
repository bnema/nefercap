package wayland

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strconv"
	"unicode"
	"unicode/utf8"

	"github.com/bnema/wlturbo"

	"github.com/bnema/nefercap/internal/ports"
)

// ext-workspace-v1 client. Only the state needed to list workspaces and to
// name one as a capture source is kept; nothing is ever requested from the
// compositor (no activate, no commit).

const (
	workspaceIface   = "ext_workspace_manager_v1"
	workspaceVersion = 1
	maxWorkspaces    = 256 // bound of the inventory
	maxNameLen       = 256 // bytes, for workspace names
	maxGroupOutputs  = 64

	wsStateActive = 1

	reqGroupDestroy  = 1
	reqHandleDestroy = 0

	maxInterned = 4 * maxWorkspaces // id strings remembered across removals
)

// workspaceState is the ext-workspace inventory of a Source.
type workspaceState struct {
	mgr      *workspaceManager     // nil when the compositor has no ext-workspace
	handles  map[uint32]*workspace // by proxy ID
	byID     map[uint64]*workspace // by Source-local identity
	groups   map[uint32]*workspaceGroup
	next     uint64            // last identity handed out; identities start at 1
	interned map[string]uint64 // id string to identity, bounded by maxInterned
	overflow bool              // the compositor announced more objects than maxWorkspaces
	shown    uint64            // workspace last verified as displayed, without the extension
}

// workspaceManager receives the creation events. It is bound once.
type workspaceManager struct {
	wlturbo.BaseProxy
	s *Source
}

func (*workspaceManager) EventSignature(op uint16) (string, bool) {
	switch op {
	case 0, 1:
		return "new_id,", true // workspace_group, workspace
	case 2, 3:
		return "", true // done, finished
	}
	return "", false
}

func (m *workspaceManager) Dispatch(e *wlturbo.Event) {
	s := m.s
	switch e.Opcode {
	case 0:
		id := e.Uint32()
		g := &workspaceGroup{s: s, outputs: make(map[uint32]bool)}
		g.SetContext(s.wl)
		g.SetID(id)
		g.SetVersion(m.Version())
		s.wl.Register(g)
		if len(s.ws.groups) >= maxWorkspaces {
			s.ws.overflow = true
			return
		}
		s.ws.groups[id] = g
	case 1:
		id := e.Uint32()
		w := &workspace{s: s}
		w.SetContext(s.wl)
		w.SetID(id)
		w.SetVersion(m.Version())
		s.wl.Register(w)
		if len(s.ws.handles) >= maxWorkspaces {
			s.ws.overflow = true
			return
		}
		s.ws.handles[id] = w
	case 2:
		s.ws.assignIDs()
	}
}

type workspaceGroup struct {
	wlturbo.BaseProxy
	s       *Source
	outputs map[uint32]bool // wl_output proxy IDs
}

func (*workspaceGroup) EventSignature(op uint16) (string, bool) {
	switch op {
	case 0:
		return "uint,", true // capabilities
	case 1, 2, 3, 4:
		return "object,", true // output_enter, output_leave, workspace_enter, workspace_leave
	case 5:
		return "", true // removed
	}
	return "", false
}

func (g *workspaceGroup) Dispatch(e *wlturbo.Event) {
	s := g.s
	switch e.Opcode {
	case 1:
		if id := e.Uint32(); len(g.outputs) < maxGroupOutputs {
			g.outputs[id] = true
		}
	case 2:
		delete(g.outputs, e.Uint32())
	case 3:
		if w := s.ws.handles[e.Uint32()]; w != nil {
			w.group = g
		}
	case 4:
		if w := s.ws.handles[e.Uint32()]; w != nil && w.group == g {
			w.group = nil
		}
	case 5:
		delete(s.ws.groups, g.ID())
		for _, w := range s.ws.handles {
			if w.group == g {
				w.group = nil
			}
		}
		_ = s.wl.Request(wlturbo.Request{Proxy: g, Opcode: reqGroupDestroy, Name: "ext_workspace_group_handle_v1.destroy", Destructor: true})
	}
}

// workspace is one ext_workspace_handle_v1. It is a wlturbo object so that it
// can be the argument of create_workspace_source.
type workspace struct {
	wlturbo.BaseProxy
	s       *Source
	idStr   string // the id event: empty for a temporary workspace
	name    string
	state   uint32
	group   *workspaceGroup
	id      uint64 // Source-local identity, assigned at the first done
	removed bool
	frame   *workspaceFrame // NeferWL's extension only; nil until asked for
}

func (*workspace) EventSignature(op uint16) (string, bool) {
	switch op {
	case 0, 1:
		return "string,", true // id, name
	case 2:
		return "array,", true // coordinates
	case 3, 4:
		return "uint,", true // state, capabilities
	case 5:
		return "", true // removed
	}
	return "", false
}

func (w *workspace) Dispatch(e *wlturbo.Event) {
	switch e.Opcode {
	case 0:
		if w.idStr == "" {
			w.idStr = e.String()
		}
	case 1:
		if n := e.String(); validName(n) {
			w.name = n
		}
	// opcode 2 (coordinates) is not used: nothing here depends on the grid.
	case 3:
		w.state = e.Uint32()
	case 5:
		w.removed = true
		s := w.s
		s.releaseWorkspaceFrame(w)
		delete(s.ws.handles, w.ID())
		if w.id != 0 && s.ws.byID[w.id] == w {
			delete(s.ws.byID, w.id)
		}
		_ = s.wl.Request(wlturbo.Request{Proxy: w, Opcode: reqHandleDestroy, Name: "ext_workspace_handle_v1.destroy", Destructor: true})
	}
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
	slices.SortFunc(fresh, func(a, b *workspace) int { return cmp.Compare(a.ID(), b.ID()) })
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
	m := &workspaceManager{s: s}
	m.SetContext(s.wl)
	_, err := s.display.Registry().BindNegotiated(workspaceIface, workspaceVersion, m)
	switch {
	case err == nil:
		s.ws.mgr = m
		return nil
	case errors.Is(err, wlturbo.ErrGlobalNotFound):
		return nil
	}
	return err
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
				Active: w.state&wsStateActive != 0,
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
		if w.removed || w.state&wsStateActive == 0 || w.group == nil || !w.group.outputs[out.proxy.ID()] {
			return sessionKey{}, ports.Region{}, ports.ErrWorkspaceUnavailable
		}
		s.ws.shown = w.id
	}
	return sessionKey{output: out.id}, ports.Region{}, nil
}
