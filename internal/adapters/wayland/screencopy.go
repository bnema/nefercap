package wayland

import (
	"errors"
	"fmt"

	"github.com/bnema/wlturbo"

	"github.com/bnema/nefercap/internal/ports"
)

// wl_shm format values for the two supported native little-endian layouts.
const (
	shmARGB8888 = uint32(ports.ARGB8888)
	shmXRGB8888 = uint32(ports.XRGB8888)
)

const flagYInvert = 1 // zwlr_screencopy_frame_v1.flags.y_invert

// The generated bindings of wlturbo v0.3 do not cover wlr-screencopy, so the
// two objects used are declared here with their wire signatures.

type screencopyManager struct{ wlturbo.BaseProxy }

func (*screencopyManager) EventSignature(uint16) (string, bool) { return "", false }

const (
	reqCaptureOutput       = 0
	reqCaptureOutputRegion = 1
	reqFrameCopy           = 0
	reqFrameDestroy        = 1
)

// frameProxy is one zwlr_screencopy_frame_v1. Its events are dispatched on the
// goroutine that owns the operation.
type frameProxy struct {
	wlturbo.BaseProxy
	out *output

	haveBuffer  bool // a buffer event announcing a supported shm format arrived
	sawBuffer   bool
	unsupported uint32 // last rejected format
	bufDone     bool
	geom        bufGeom

	copied  bool
	yInvert bool
	ready   bool
	failed  bool
}

func (*frameProxy) EventSignature(op uint16) (string, bool) {
	switch op {
	case 0, 4:
		return "uint,uint,uint,uint,", true // buffer, damage
	case 1:
		return "uint,", true // flags
	case 2, 5:
		return "uint,uint,uint,", true // ready, linux_dmabuf
	case 3, 6:
		return "", true // failed, buffer_done
	}
	return "", false
}

func (f *frameProxy) Dispatch(e *wlturbo.Event) {
	switch e.Opcode {
	case 0:
		format, w, h, stride := e.Uint32(), e.Uint32(), e.Uint32(), e.Uint32()
		f.sawBuffer = true
		switch {
		case format != shmARGB8888 && format != shmXRGB8888:
			f.unsupported = format
		case !f.haveBuffer:
			f.haveBuffer = true
			f.geom = bufGeom{format: format, width: w, height: h, stride: stride}
		}
	case 1:
		f.yInvert = e.Uint32()&flagYInvert != 0
	case 2:
		f.ready = true
	case 3:
		f.failed = true
	case 6:
		f.bufDone = true
	}
}

func validRegion(r ports.Region) error {
	if r == (ports.Region{}) {
		return nil
	}
	if r.X < 0 || r.Y < 0 || r.Width < 1 || r.Height < 1 ||
		r.X > ports.MaxDimension || r.Y > ports.MaxDimension ||
		r.Width > ports.MaxDimension || r.Height > ports.MaxDimension {
		return fmt.Errorf("%w: %+v", ports.ErrInvalidRegion, r)
	}
	return nil
}

func (s *Source) capture(t ports.Target) (ports.Frame, error) {
	out := s.outputs[t.OutputID]
	if out == nil {
		return ports.Frame{}, ports.ErrOutputNotFound
	}
	f := &frameProxy{out: out}
	f.SetContext(s.wl)
	var err error
	if t.Region == (ports.Region{}) {
		err = s.wl.Request(wlturbo.Request{Proxy: s.screencopy, Opcode: reqCaptureOutput, Name: "zwlr_screencopy_manager_v1.capture_output", Child: f},
			f, int32(0), out.proxy)
	} else {
		r := t.Region
		err = s.wl.Request(wlturbo.Request{Proxy: s.screencopy, Opcode: reqCaptureOutputRegion, Name: "zwlr_screencopy_manager_v1.capture_output_region", Child: f},
			f, int32(0), out.proxy, int32(r.X), int32(r.Y), int32(r.Width), int32(r.Height))
	}
	if err != nil {
		s.terminate()
		return ports.Frame{}, fmt.Errorf("wayland: capture request: %w", err)
	}
	// Until copy is sent the compositor does not touch our buffer, so a
	// negotiation failure leaves the Source usable. After copy, any failure
	// other than the compositor's own "failed" event ends in terminal state.
	for {
		if err := s.dispatch(); err != nil {
			return ports.Frame{}, err
		}
		switch {
		case f.failed:
			s.destroyFrame(f)
			// The removal may trail the failure on the wire: read it.
			if err := s.roundtrip(); err != nil {
				return ports.Frame{}, err
			}
			if f.out.removed {
				return ports.Frame{}, ports.ErrOutputNotFound
			}
			return ports.Frame{}, errCompositorFailedCapture
		case f.ready && !f.copied:
			s.terminate()
			return ports.Frame{}, errors.New("wayland: compositor completed a frame that was never copied")
		case f.ready:
			s.destroyFrame(f)
			return s.frameOf(f)
		case !f.copied && s.negotiated(f):
			if err := s.startCopy(f); err != nil {
				s.destroyFrame(f)
				return ports.Frame{}, err
			}
		}
	}
}

// negotiated reports whether the compositor announced every buffer type.
// Before version 3 there is no buffer_done: the single buffer event is final.
func (s *Source) negotiated(f *frameProxy) bool {
	if s.screencopy.Version() >= 3 {
		return f.bufDone
	}
	return f.sawBuffer
}

func (s *Source) startCopy(f *frameProxy) error {
	if !f.sawBuffer {
		return fmt.Errorf("%w: compositor offers no wl_shm buffer", ports.ErrUnsupportedFormat)
	}
	if !f.haveBuffer {
		return fmt.Errorf("%w: compositor offers shm format %#x", ports.ErrUnsupportedFormat, f.unsupported)
	}
	if err := f.geom.validate(); err != nil {
		return err
	}
	if err := s.ensureBuffer(f.geom); err != nil {
		return err
	}
	f.copied = true
	if err := s.wl.Request(wlturbo.Request{Proxy: f, Opcode: reqFrameCopy, Name: "zwlr_screencopy_frame_v1.copy"}, s.buf.buffer); err != nil {
		s.terminate()
		return fmt.Errorf("wayland: copy request: %w", err)
	}
	return nil
}

func (s *Source) destroyFrame(f *frameProxy) {
	_ = s.wl.Request(wlturbo.Request{Proxy: f, Opcode: reqFrameDestroy, Name: "zwlr_screencopy_frame_v1.destroy", Destructor: true})
}

func (s *Source) frameOf(f *frameProxy) (ports.Frame, error) {
	g := f.geom
	frame := ports.Frame{
		Pixels: s.buf.data[:g.size:g.size],
		Width:  int(g.width), Height: int(g.height), Stride: int(g.stride),
		Format: ports.PixelFormat(g.format), YInvert: f.yInvert,
	}
	if err := frame.Validate(); err != nil {
		return ports.Frame{}, err
	}
	return frame, nil
}
