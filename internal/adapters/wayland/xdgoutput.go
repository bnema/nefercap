package wayland

import (
	"github.com/bnema/wlturbo"

	"github.com/bnema/nefercap/internal/ports"
)

// zxdg_output_v1 gives the logical size of an output, which maps a logical
// region onto buffer pixels when a region has to be cropped from the output
// frame. It is optional: the current mode over the integer scale is the
// fallback.

const (
	reqGetXdgOutput     = 1 // zxdg_output_manager_v1.get_xdg_output
	reqXdgOutputDestroy = 0 // zxdg_output_v1.destroy
)

type xdgOutput struct {
	wlturbo.BaseProxy
	o *output
}

func (*xdgOutput) EventSignature(op uint16) (string, bool) {
	switch op {
	case 0, 1:
		return "int,int,", true // logical_position, logical_size
	case 2:
		return "", true // done
	case 3, 4:
		return "string,", true // name, description
	}
	return "", false
}

func (x *xdgOutput) Dispatch(e *wlturbo.Event) {
	if e.Opcode != 1 {
		return
	}
	w, h := e.Int32(), e.Int32()
	if w >= 0 && h >= 0 && w <= ports.MaxDimension && h <= ports.MaxDimension {
		x.o.lw, x.o.lh = w, h
	}
}

// watchLogicalSize subscribes to the logical size of o.
func (s *Source) watchLogicalSize(o *output) {
	if s.xdgOutputs == nil {
		return
	}
	x := &xdgOutput{o: o}
	x.SetContext(s.wl)
	if err := s.wl.Request(wlturbo.Request{Proxy: s.xdgOutputs, Opcode: reqGetXdgOutput, Name: "zxdg_output_manager_v1.get_xdg_output", Child: x}, x, o.proxy); err != nil {
		s.log.Warn().Err(err).Msg("xdg-output request failed")
		return
	}
	o.xdg = x
}

// releaseXdg destroys the zxdg_output_v1 of an output that went away.
func (s *Source) releaseXdg(o *output) {
	if x := o.xdg; x != nil {
		o.xdg = nil
		_ = s.wl.Request(wlturbo.Request{Proxy: x, Opcode: reqXdgOutputDestroy, Name: "zxdg_output_v1.destroy", Destructor: true})
	}
}
