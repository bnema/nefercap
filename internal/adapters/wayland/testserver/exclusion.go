package testserver

import (
	"github.com/bnema/purego-libwayland/protocol/extimagecopycapture"
	"github.com/bnema/purego-libwayland/protocol/wayland"

	"github.com/bnema/nefercap/internal/adapters/wayland/testserver/neferwl"
)

type exclusionManager struct{ s *Server }

func (exclusionManager) Destroy(*neferwl.NeferwlCaptureExclusionManagerV1) {}

func (m exclusionManager) GetExclusion(self *neferwl.NeferwlCaptureExclusionManagerV1, id uint32, sess *extimagecopycapture.ExtImageCopyCaptureSessionV1) {
	s := m.s
	res, err := neferwl.NewNeferwlCaptureExclusionV1(self.Client(), 1, id, exclusionHandler{s})
	if err != nil {
		return
	}
	s.stats.Exclusions++
	res.OnDestroy = func() { s.stats.DestroyedExclusions++ }
	if s.cfg.ExclusionFail != 0 {
		res.SendFailed(s.cfg.ExclusionFail - 1)
		return
	}
	res.SendToken(Token)
}

func (m exclusionManager) AttachSurface(self *neferwl.NeferwlCaptureExclusionManagerV1, id uint32, token string, _ *wayland.Surface) {
	s := m.s
	res, err := neferwl.NewNeferwlCaptureLayerV1(self.Client(), 1, id, layerHandler{})
	if err != nil {
		return
	}
	s.stats.Attached = append(s.stats.Attached, token)
	s.layers = append(s.layers, res.Resource)
	layer := res
	switch {
	case s.cfg.AttachSilent:
	case s.cfg.AttachFail != 0:
		layer.SendFailed(s.cfg.AttachFail - 1)
	default:
		layer.SendAttached()
	}
}

type exclusionHandler struct{ s *Server }

func (exclusionHandler) Destroy(*neferwl.NeferwlCaptureExclusionV1) {}

type layerHandler struct{}

func (layerHandler) Destroy(*neferwl.NeferwlCaptureLayerV1) {}

// Detach sends detached(reason) on the n-th attached layer.
func (s *Server) Detach(n int, reason uint32) {
	s.Do(func() {
		if n < len(s.layers) {
			neferwl.WrapNeferwlCaptureLayerV1(s.layers[n]).SendDetached(reason)
		}
	})
}

// SendRaw posts a layer event out of order, to test violations: opcode 0
// attached, 1 failed, 2 detached.
func (s *Server) SendRaw(n int, opcode uint32, arg uint32) {
	s.Do(func() {
		if n >= len(s.layers) {
			return
		}
		l := neferwl.WrapNeferwlCaptureLayerV1(s.layers[n])
		switch opcode {
		case 0:
			l.SendAttached()
		case 1:
			l.SendFailed(arg)
		default:
			l.SendDetached(arg)
		}
	})
}
