package testserver

import (
	"github.com/bnema/purego-libwayland/protocol/wayland"
	"github.com/bnema/purego-libwayland/protocol/xdgoutput"
	"github.com/bnema/purego-libwayland/server"
)

// xdg-output is offered only when an output announces a logical size.
func (s *Server) xdgOutputWanted() bool {
	for _, o := range s.outputs {
		if o.spec.LogicalWidth > 0 {
			return true
		}
	}
	return false
}

func (s *Server) addXdgOutput(must func(error)) {
	must(xdgoutput.NewZxdgOutputManagerV1Global(s.d, 3, func(c server.Client, v, id uint32) {
		if _, err := xdgoutput.NewZxdgOutputManagerV1(c, int32(v), id, xdgManager{s}); err != nil {
			must(err)
		}
	}))
}

type xdgManager struct{ s *Server }

func (xdgManager) Destroy(*xdgoutput.ZxdgOutputManagerV1) {}
func (m xdgManager) GetXdgOutput(self *xdgoutput.ZxdgOutputManagerV1, id uint32, out *wayland.Output) {
	res, err := xdgoutput.NewZxdgOutputV1(self.Client(), int32(self.Version()), id, xdgHandler{})
	if err != nil {
		return
	}
	res.OnDestroy = func() { m.s.stats.DestroyedXdgOutputs++ }
	if o := m.s.outputOf(out.Resource); o != nil && o.spec.LogicalWidth > 0 {
		res.SendLogicalPosition(0, 0)
		res.SendLogicalSize(o.spec.LogicalWidth, o.spec.LogicalHeight)
		res.SendDone()
	}
}

type xdgHandler struct{}

func (xdgHandler) Destroy(*xdgoutput.ZxdgOutputV1) {}
