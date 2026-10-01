package testserver

import (
	"encoding/binary"

	"github.com/bnema/purego-libwayland/protocol/extworkspace"
	"github.com/bnema/purego-libwayland/protocol/wayland"
	"github.com/bnema/purego-libwayland/server"

	"github.com/bnema/nefercap/internal/adapters/wayland/testserver/neferwl"
)

type workspace struct {
	spec    WorkspaceSpec
	removed bool
}

type wsHandle struct {
	w   *workspace
	res *extworkspace.ExtWorkspaceHandleV1
}

type wsManager struct {
	s       *Server
	res     *extworkspace.ExtWorkspaceManagerV1
	group   *extworkspace.ExtWorkspaceGroupHandleV1
	handles []*wsHandle
}

func (m *wsManager) Commit(*extworkspace.ExtWorkspaceManagerV1) {}
func (m *wsManager) Stop(*extworkspace.ExtWorkspaceManagerV1)   {}

type groupHandler struct{}

func (groupHandler) CreateWorkspace(*extworkspace.ExtWorkspaceGroupHandleV1, string) {}
func (groupHandler) Destroy(*extworkspace.ExtWorkspaceGroupHandleV1)                 {}

type handleHandler struct{}

func (handleHandler) Destroy(*extworkspace.ExtWorkspaceHandleV1)    {}
func (handleHandler) Activate(*extworkspace.ExtWorkspaceHandleV1)   {}
func (handleHandler) Deactivate(*extworkspace.ExtWorkspaceHandleV1) {}
func (handleHandler) Assign(*extworkspace.ExtWorkspaceHandleV1, *extworkspace.ExtWorkspaceGroupHandleV1) {
}
func (handleHandler) Remove(*extworkspace.ExtWorkspaceHandleV1) {}

func stateBits(w WorkspaceSpec) uint32 {
	var st uint32
	if w.Active {
		st |= uint32(extworkspace.ExtWorkspaceHandleV1StateActive)
	}
	if w.Hidden {
		st |= uint32(extworkspace.ExtWorkspaceHandleV1StateHidden)
	}
	return st
}

// announce sends the group, its outputs and every workspace, then done.
func (m *wsManager) announce(c server.Client) {
	g, err := extworkspace.NewExtWorkspaceGroupHandleV1(c, 1, 0, groupHandler{})
	if err != nil {
		return
	}
	m.group = g
	m.res.SendWorkspaceGroup(g)
	g.SendCapabilities(0)
	for _, o := range m.s.outputs {
		for _, r := range o.res {
			if r.Client() == c {
				g.SendOutputEnter(r)
			}
		}
	}
	for i, w := range m.s.workspaces {
		if !w.removed {
			m.add(c, g, w, i)
		}
	}
	m.res.SendDone()
}

func (m *wsManager) add(c server.Client, g *extworkspace.ExtWorkspaceGroupHandleV1, w *workspace, index int) {
	r, err := extworkspace.NewExtWorkspaceHandleV1(c, 1, 0, handleHandler{})
	if err != nil {
		return
	}
	m.handles = append(m.handles, &wsHandle{w: w, res: r})
	m.res.SendWorkspace(r)
	if w.spec.ID != "" {
		r.SendId(w.spec.ID)
	}
	r.SendName(w.spec.Name)
	var b [4]byte
	binary.NativeEndian.PutUint32(b[:], uint32(index))
	r.SendCoordinates(b[:])
	r.SendState(stateBits(w.spec))
	r.SendCapabilities(0)
	g.SendWorkspaceEnter(r)
}

func (s *Server) workspaceNamed(name string) *workspace {
	for _, w := range s.workspaces {
		if w.spec.Name == name {
			return w
		}
	}
	return nil
}

// SetActive changes the active state of the named workspace.
func (s *Server) SetActive(name string, active bool) {
	s.Do(func() {
		w := s.workspaceNamed(name)
		if w == nil {
			return
		}
		w.spec.Active = active
		for _, m := range s.wsMgrs {
			for _, h := range m.handles {
				if h.w == w && !w.removed {
					h.res.SendState(stateBits(w.spec))
				}
			}
			m.res.SendDone()
		}
	})
}

// RemoveWorkspace removes the named workspace from every manager.
func (s *Server) RemoveWorkspace(name string) {
	s.Do(func() {
		w := s.workspaceNamed(name)
		if w == nil || w.removed {
			return
		}
		w.removed = true
		for _, m := range s.wsMgrs {
			for _, h := range m.handles {
				if h.w == w {
					m.group.SendWorkspaceLeave(h.res)
					h.res.SendRemoved()
				}
			}
			m.res.SendDone()
		}
		for _, c := range s.sessions {
			if c.src.ws == w {
				c.stop()
			}
		}
	})
}

// workspaceOf finds the workspace behind a handle resource.
func (s *Server) workspaceOf(r *server.Resource) *workspace {
	for _, m := range s.wsMgrs {
		for _, h := range m.handles {
			if h.res.Resource == r {
				return h.w
			}
		}
	}
	return nil
}

type neferwlSources struct{ s *Server }

// frameObject is one neferwl_workspace_frame_v1.
type frameObject struct {
	w   *workspace
	res *neferwl.NeferwlWorkspaceFrameV1
}

func (frameObject) Destroy(*neferwl.NeferwlWorkspaceFrameV1) {}

func (f *frameObject) send() {
	if r := f.w.spec.Frame; r != ([4]int32{}) {
		f.res.SendFrame(r[0], r[1], r[2], r[3])
	}
}

func (m neferwlSources) GetWorkspaceFrame(self *neferwl.NeferwlImageCaptureSourceManagerV1, id uint32, h *extworkspace.ExtWorkspaceHandleV1) {
	s := m.s
	f := &frameObject{w: s.workspaceOf(h.Resource)}
	res, err := neferwl.NewNeferwlWorkspaceFrameV1(self.Client(), 1, id, f)
	if err != nil {
		return
	}
	f.res = res
	s.stats.WorkspaceFrames++
	res.OnDestroy = func() {
		s.stats.DestroyedWorkspaceFrames++
		s.frames = removeFrame(s.frames, f)
	}
	if f.w == nil || f.w.removed {
		return
	}
	s.frames = append(s.frames, f)
	f.send()
}

func removeFrame(list []*frameObject, f *frameObject) []*frameObject {
	for i, g := range list {
		if g == f {
			return append(list[:i], list[i+1:]...)
		}
	}
	return list
}

// SetFrame changes the frame of the named workspace and reports it to every
// frame object that watches it.
func (s *Server) SetFrame(name string, frame [4]int32) {
	s.Do(func() {
		w := s.workspaceNamed(name)
		if w == nil {
			return
		}
		w.spec.Frame = frame
		for _, f := range s.frames {
			if f.w == w && !w.removed {
				f.send()
			}
		}
	})
}

func (neferwlSources) Destroy(*neferwl.NeferwlImageCaptureSourceManagerV1) {}
func (m neferwlSources) CreateWorkspaceSource(self *neferwl.NeferwlImageCaptureSourceManagerV1, id uint32, h *extworkspace.ExtWorkspaceHandleV1) {
	w := m.s.workspaceOf(h.Resource)
	m.s.newSource(self.Client(), id, &source{kind: "workspace", ws: w, gone: w == nil || w.removed})
}
func (m neferwlSources) CreateOutputRegionSource(self *neferwl.NeferwlImageCaptureSourceManagerV1, id uint32, out *wayland.Output, x, y, w, h int32) {
	if w < 1 || h < 1 {
		self.PostError(uint32(neferwl.NeferwlImageCaptureSourceManagerV1ErrorInvalidRegion), "empty region")
		return
	}
	m.s.newSource(self.Client(), id, &source{kind: "region", out: m.s.outputOf(out.Resource), region: [4]int32{x, y, w, h}})
}
