package wayland

import "github.com/bnema/nefercap/internal/ports"

// zxdg_output_v1 gives the logical size of an output, which maps a logical
// region onto buffer pixels when a region has to be cropped from the output
// frame. It is optional: the current mode over the integer scale is the
// fallback.

// watchLogicalSize subscribes to the logical size of o.
func (s *Source) watchLogicalSize(o *output) {
	if s.xdgOutputs == nil {
		return
	}
	x, err := s.xdgOutputs.GetXdgOutput(o.proxy)
	if err != nil {
		s.log.Warn().Err(err).Msg("xdg-output request failed")
		return
	}
	x.OnLogicalSize(func(w, h int32) {
		if w >= 0 && h >= 0 && w <= ports.MaxDimension && h <= ports.MaxDimension {
			o.lw, o.lh = w, h
		}
	})
	o.xdg = x
}

// releaseXdg destroys the zxdg_output_v1 of an output that went away.
func (s *Source) releaseXdg(o *output) {
	if x := o.xdg; x != nil {
		o.xdg = nil
		_ = x.Destroy()
	}
}
