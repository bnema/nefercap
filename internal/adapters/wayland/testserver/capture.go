package testserver

import (
	"github.com/bnema/purego-libwayland/protocol/extimagecapturesource"
	"github.com/bnema/purego-libwayland/protocol/extimagecopycapture"
	"github.com/bnema/purego-libwayland/protocol/wayland"
)

// Pixel is the content the server paints: a function of the output pixel and
// the frame sequence, so tests can tell which part of which frame they read.
func Pixel(x, y int, seq uint32) [4]byte { return [4]byte{byte(x), byte(y), byte(seq), 0xff} }

type copyManager struct{ s *Server }

func (m copyManager) CreateSession(self *extimagecopycapture.ExtImageCopyCaptureManagerV1, id uint32, src *extimagecapturesource.ExtImageCaptureSourceV1, options uint32) {
	s := m.s
	c := &session{s: s, src: s.sources[src.Resource]}
	if c.src == nil {
		self.PostError(1, "unknown source")
		return
	}
	res, err := extimagecopycapture.NewExtImageCopyCaptureSessionV1(self.Client(), 1, id, sessionHandler{c})
	if err != nil {
		return
	}
	c.res = res
	c.out = c.src.out
	if c.src.ws != nil {
		c.out = s.outputNamed(c.src.ws.spec.Output)
	}
	s.sessions = append(s.sessions, c)
	s.stats.Sessions++
	s.stats.LastSource = c.src.kind
	s.stats.LastRegion = c.src.region
	res.OnDestroy = func() {
		c.dead = true
		s.stats.DestroyedSessions++
	}
	if s.cfg.StopAtCreate || c.src.gone || c.out == nil || c.out.removed {
		c.stop()
		return
	}
	c.sendConstraints()
}
func (copyManager) CreatePointerCursorSession(self *extimagecopycapture.ExtImageCopyCaptureManagerV1, _ uint32, _ *extimagecapturesource.ExtImageCaptureSourceV1, _ *wayland.Pointer) {
	self.PostError(1, "cursor sessions are not supported")
}
func (copyManager) Destroy(*extimagecopycapture.ExtImageCopyCaptureManagerV1) {}

func (s *Server) outputNamed(name string) *output {
	for _, o := range s.outputs {
		if o.spec.Name == name {
			return o
		}
	}
	return nil
}

type session struct {
	s        *Server
	res      *extimagecopycapture.ExtImageCopyCaptureSessionV1
	src      *source
	out      *output
	frame    *frame // the live frame, nil when none
	w, h     uint32 // the latest announced buffer size
	dead     bool
	stopped  bool
	answered int // frames completed with ready
}

// size is the buffer size of the session's source.
func (c *session) size() (w, h uint32) {
	scale := uint32(max(c.out.spec.Scale, 1))
	if c.src.kind == "region" {
		return uint32(c.src.region[2]) * scale, uint32(c.src.region[3]) * scale
	}
	return c.out.spec.Width, c.out.spec.Height
}

func (c *session) sendConstraints() {
	c.w, c.h = c.size()
	c.res.SendBufferSize(c.w, c.h)
	if !c.s.cfg.NoShmFormat {
		for _, f := range c.s.cfg.Formats {
			c.res.SendShmFormat(f)
		}
	}
	c.res.SendDone()
}

func (c *session) stop() {
	if c.stopped || c.dead {
		return
	}
	c.stopped = true
	c.res.SendStopped()
}

type sessionHandler struct{ c *session }

func (h sessionHandler) CreateFrame(self *extimagecopycapture.ExtImageCopyCaptureSessionV1, id uint32) {
	c := h.c
	if c.frame != nil {
		self.PostError(uint32(extimagecopycapture.ExtImageCopyCaptureSessionV1ErrorDuplicateFrame), "create_frame before destroying the previous frame")
		return
	}
	f := &frame{c: c}
	res, err := extimagecopycapture.NewExtImageCopyCaptureFrameV1(self.Client(), 1, id, f)
	if err != nil {
		return
	}
	f.res = res
	c.frame = f
	c.s.stats.Frames++
	res.OnDestroy = func() {
		if c.frame == f {
			c.frame = nil
		}
		f.gone = true
	}
}
func (sessionHandler) Destroy(*extimagecopycapture.ExtImageCopyCaptureSessionV1) {}

type frame struct {
	c        *session
	res      *extimagecopycapture.ExtImageCopyCaptureFrameV1
	buf      *buffer
	captured bool
	gone     bool
}

func (f *frame) Destroy(*extimagecopycapture.ExtImageCopyCaptureFrameV1) {}
func (f *frame) AttachBuffer(self *extimagecopycapture.ExtImageCopyCaptureFrameV1, b *wayland.Buffer) {
	f.buf = f.c.s.bufs[b.Resource]
}
func (f *frame) DamageBuffer(*extimagecopycapture.ExtImageCopyCaptureFrameV1, int32, int32, int32, int32) {
}

func (f *frame) Capture(self *extimagecopycapture.ExtImageCopyCaptureFrameV1) {
	s := f.c.s
	if f.buf == nil {
		self.PostError(uint32(extimagecopycapture.ExtImageCopyCaptureFrameV1ErrorNoBuffer), "capture without buffer")
		return
	}
	if f.captured {
		self.PostError(uint32(extimagecopycapture.ExtImageCopyCaptureFrameV1ErrorAlreadyCaptured), "captured twice")
		return
	}
	f.captured = true
	s.stats.Captures++
	if s.cfg.Hold || (s.cfg.HoldAfterFirst && f.c.answered > 0) {
		s.held = append(s.held, f)
		return
	}
	f.complete()
}

// complete answers a captured frame: failed, or pixels and ready.
func (f *frame) complete() {
	c, s := f.c, f.c.s
	switch {
	case f.gone || c.dead:
		return
	case c.stopped:
		f.res.SendFailed(uint32(extimagecopycapture.ExtImageCopyCaptureFrameV1FailureReasonStopped))
		return
	case s.cfg.FailFrames > 0:
		s.cfg.FailFrames--
		f.res.SendFailed(s.cfg.FailReason)
		return
	case f.buf.width != c.w || f.buf.height != c.h || !f.formatOK():
		f.res.SendFailed(uint32(extimagecopycapture.ExtImageCopyCaptureFrameV1FailureReasonBufferConstraints))
		return
	}
	s.seq++
	data := f.buf.bytes()
	if data == nil {
		f.res.SendFailed(uint32(extimagecopycapture.ExtImageCopyCaptureFrameV1FailureReasonUnknown))
		return
	}
	ox, oy, scale := 0, 0, int(max(c.out.spec.Scale, 1))
	if c.src.kind == "region" {
		ox, oy = int(c.src.region[0])*scale, int(c.src.region[1])*scale
	}
	for y := 0; y < int(f.buf.height); y++ {
		row := data[y*int(f.buf.stride):]
		for x := 0; x < int(f.buf.width); x++ {
			p := Pixel(ox+x, oy+y, s.seq)
			copy(row[x*4:], p[:])
		}
	}
	s.stats.Ready++
	c.answered++
	f.res.SendTransform(s.cfg.Transform)
	f.res.SendDamage(0, 0, int32(f.buf.width), int32(f.buf.height))
	f.res.SendReady()
}

func (f *frame) formatOK() bool {
	for _, x := range f.c.s.cfg.Formats {
		if x == f.buf.format {
			return true
		}
	}
	return false
}

// Release completes every held frame.
func (s *Server) Release() {
	s.Do(func() {
		held := s.held
		s.held = nil
		for _, f := range held {
			f.complete()
		}
	})
}

// Held reports how many captured frames wait for Release.
func (s *Server) Held() int {
	n := 0
	s.Do(func() { n = len(s.held) })
	return n
}

// Resize changes an output's mode and sends new constraints to its sessions.
func (s *Server) Resize(name string, w, h uint32) {
	s.Do(func() {
		o := s.outputNamed(name)
		if o == nil {
			return
		}
		o.spec.Width, o.spec.Height = w, h
		for _, c := range s.sessions {
			if c.out == o && !c.dead && !c.stopped {
				c.sendConstraints()
			}
		}
	})
}

// StopSessions ends every live session with the stopped event.
func (s *Server) StopSessions() {
	s.Do(func() {
		for _, c := range s.sessions {
			c.stop()
		}
	})
}

// Fail makes the next n captures answer failed(reason).
func (s *Server) Fail(n int, reason uint32) {
	s.Do(func() { s.cfg.FailFrames, s.cfg.FailReason = n, reason })
}
