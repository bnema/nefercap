package wayland

import (
	"errors"
	"fmt"
	"time"

	"github.com/bnema/wlturbo"

	"github.com/bnema/nefercap/internal/ports"
)

// wl_shm format values for the two supported native little-endian layouts.
const (
	shmARGB8888 = uint32(ports.ARGB8888)
	shmXRGB8888 = uint32(ports.XRGB8888)
)

// Hand-written proxies for ext-image-capture-source-v1 and
// ext-image-copy-capture-v1: the wlturbo release in use does not generate
// them. Only the requests and events nefercap uses are declared.

// requestOnly is a bound global or created object that never sends events.
type requestOnly struct{ wlturbo.BaseProxy }

func (*requestOnly) EventSignature(uint16) (string, bool) { return "", false }

func newRequestOnly(ctx *wlturbo.Context) *requestOnly {
	p := &requestOnly{}
	p.SetContext(ctx)
	return p
}

const (
	reqOutputSourceCreate = 0 // ext_output_image_capture_source_manager_v1.create_source
	reqSourceDestroy      = 0 // ext_image_capture_source_v1.destroy
	reqCopyCreateSession  = 0 // ext_image_copy_capture_manager_v1.create_session
	reqSessionCreateFrame = 0
	reqSessionDestroy     = 1
	reqFrameDestroy       = 0
	reqFrameAttachBuffer  = 1
	reqFrameDamageBuffer  = 2
	reqFrameCapture       = 3
)

// ext_image_copy_capture_frame_v1.failure_reason
const (
	failUnknown           = 0
	failBufferConstraints = 1
	failStopped           = 2
)

// maxFrameAttempts bounds the retries of one frame after failures that the
// protocol calls recoverable.
const maxFrameAttempts = 4

var errCompositorFailedCapture = errors.New("wayland: compositor failed the capture")

// errCaptureRefused is ErrCaptureStopped for a session that stopped before
// serving any frame.
var errCaptureRefused = fmt.Errorf("%w: compositor refused capture (on NeferWL, list this executable in /etc/neferwl/capture-allow)", ports.ErrCaptureStopped)

// constraints is one batch of buffer constraints, valid once done arrived.
type constraints struct {
	width, height uint32
	haveSize      bool
	format        uint32 // first supported shm format of the batch
	haveFormat    bool
	sawShm        bool
	unsupported   uint32 // last rejected shm format
}

// session is one ext_image_copy_capture_session_v1. Its events are dispatched
// on the goroutine that owns the operation.
type session struct {
	wlturbo.BaseProxy
	key sessionKey

	pending constraints
	current constraints
	gen     uint64 // number of constraint batches completed
	stopped bool
	dead    bool // destroy sent

	exclusion *exclusion // nil until requested

	// Frame pacing. At most one frame is in flight. The last completed frame
	// lives in buffer cur and stays untouched while the next one is written to
	// the other buffer, so a Capture that finds the compositor idle (it waits
	// for damage after the first frame) can repeat the last frame at once.
	inflight     *frameProxy
	inflightBuf  int
	inflightGeom bufGeom // buffer layout the in-flight frame was made for
	haveLast     bool
	cur          int
	lastGeom     bufGeom
	served       int // frames handed out by this session
	fails        int // consecutive failed frames
}

func (*session) EventSignature(op uint16) (string, bool) {
	switch op {
	case 0:
		return "uint,uint,", true // buffer_size
	case 1:
		return "uint,", true // shm_format
	case 2:
		return "array,", true // dmabuf_device
	case 3:
		return "uint,array,", true // dmabuf_format
	case 4, 5:
		return "", true // done, stopped
	}
	return "", false
}

func (c *session) Dispatch(e *wlturbo.Event) {
	switch e.Opcode {
	case 0:
		c.pending.width, c.pending.height, c.pending.haveSize = e.Uint32(), e.Uint32(), true
	case 1:
		format := e.Uint32()
		c.pending.sawShm = true
		switch {
		case format != shmARGB8888 && format != shmXRGB8888:
			c.pending.unsupported = format
		case !c.pending.haveFormat:
			c.pending.format, c.pending.haveFormat = format, true
		}
	case 4:
		c.current, c.pending = c.pending, constraints{}
		c.gen++
	case 5:
		c.stopped = true
	}
}

// frameProxy is one ext_image_copy_capture_frame_v1.
type frameProxy struct {
	wlturbo.BaseProxy
	transform uint32
	ready     bool
	failed    bool
	reason    uint32
}

func (*frameProxy) EventSignature(op uint16) (string, bool) {
	switch op {
	case 0, 4:
		return "uint,", true // transform, failed
	case 1:
		return "int,int,int,int,", true // damage
	case 2:
		return "uint,uint,uint,", true // presentation_time
	case 3:
		return "", true // ready
	}
	return "", false
}

func (f *frameProxy) Dispatch(e *wlturbo.Event) {
	switch e.Opcode {
	case 0:
		f.transform = e.Uint32()
	case 3:
		f.ready = true
	case 4:
		f.failed, f.reason = true, e.Uint32()
	}
}

// sessionKey identifies what a session captures. Crop sessions have a zero
// region: the region is cut from each frame.
type sessionKey struct {
	output    uint32
	workspace uint64
	region    ports.Region
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

// captureTarget resolves t to a session, creating or replacing it, and copies
// one frame. The caller holds opMu.
func (s *Source) captureTarget(t ports.Target) (ports.Frame, error) {
	out := s.outputs[t.OutputID]
	if out == nil {
		return ports.Frame{}, ports.ErrOutputNotFound
	}
	key, crop, err := s.resolve(t, out)
	if err != nil {
		return ports.Frame{}, err
	}
	sess, err := s.sessionFor(key, out)
	if err != nil {
		return ports.Frame{}, err
	}
	frame, err := s.nextFrame(sess, out)
	if err != nil {
		return ports.Frame{}, err
	}
	if crop == (ports.Region{}) {
		return frame, nil
	}
	return cropFrame(frame, crop, out)
}

// resolve maps a target to its session key and the crop to apply per frame.
func (s *Source) resolve(t ports.Target, out *output) (sessionKey, ports.Region, error) {
	if t.WorkspaceID != 0 {
		return s.workspaceKey(t, out)
	}
	if t.Region != (ports.Region{}) && s.ext.source != nil {
		return sessionKey{output: out.id, region: t.Region}, ports.Region{}, nil
	}
	return sessionKey{output: out.id}, t.Region, nil
}

// sessionFor returns the live session for key. A different key replaces the
// current session, which ends its exclusion.
func (s *Source) sessionFor(key sessionKey, out *output) (*session, error) {
	if c := s.sess; c != nil {
		if c.key == key && (!c.stopped || c.exclusion != nil) {
			// A stopped session with an exclusion stays stopped: replacing it
			// would quietly lose the exclusion in the middle of a recording.
			return c, nil
		}
		s.dropSession()
	}
	src := newRequestOnly(s.wl)
	var err error
	switch {
	case key.workspace != 0:
		err = s.createWorkspaceSource(src, key.workspace)
	case key.region != (ports.Region{}):
		r := key.region
		err = s.wl.Request(wlturbo.Request{Proxy: s.ext.source, Opcode: reqNeferwlRegionSource, Name: "neferwl_image_capture_source_manager_v1.create_output_region_source", Child: src},
			src, out.proxy, int32(r.X), int32(r.Y), int32(r.Width), int32(r.Height))
	default:
		err = s.wl.Request(wlturbo.Request{Proxy: s.outSource, Opcode: reqOutputSourceCreate, Name: "ext_output_image_capture_source_manager_v1.create_source", Child: src}, src, out.proxy)
	}
	if err != nil {
		s.terminate()
		return nil, fmt.Errorf("wayland: create capture source: %w", err)
	}
	c := &session{key: key}
	c.SetContext(s.wl)
	err = s.wl.Request(wlturbo.Request{Proxy: s.copyManager, Opcode: reqCopyCreateSession, Name: "ext_image_copy_capture_manager_v1.create_session", Child: c}, c, src, uint32(0))
	_ = s.wl.Request(wlturbo.Request{Proxy: src, Opcode: reqSourceDestroy, Name: "ext_image_capture_source_v1.destroy", Destructor: true})
	if err != nil {
		s.terminate()
		return nil, fmt.Errorf("wayland: create capture session: %w", err)
	}
	s.sess = c
	return c, nil
}

// dropSession destroys the current session, if any.
func (s *Source) dropSession() {
	c := s.sess
	s.sess = nil
	if c == nil || c.dead {
		return
	}
	c.dead = true
	s.destroyFrame(c)
	if c.exclusion != nil {
		c.exclusion.destroy(s)
	}
	_ = s.wl.Request(wlturbo.Request{Proxy: c, Opcode: reqSessionDestroy, Name: "ext_image_copy_capture_session_v1.destroy", Destructor: true})
}

// nextFrame returns the session's current frame.
//
// The first frame of a session is waited for: the protocol guarantees it does
// not wait for damage. After that the compositor may hold a capture back until
// the screen changes, so a capture stays pending across calls (still one frame
// in flight) and a call that finds it unfinished after pollWait returns the
// last completed frame again. The caller's schedule is then never stretched by
// a static screen, and a recording repeats that frame.
//
// A failed frame is retried after a roundtrip, at most maxFrameAttempts
// failures in a row (the count survives calls and restarts at each completed
// frame): stopped ends the capture, buffer_constraints is retried with the
// latest constraints, which are the current ones when the compositor sent no
// new batch. An unknown failure is retried the same way, unless a last frame
// exists: that one is repeated, like when a capture is still pending.
func (s *Source) nextFrame(c *session, out *output) (ports.Frame, error) {
	for {
		for c.gen == 0 && !c.stopped {
			if err := s.dispatch(); err != nil {
				return ports.Frame{}, err
			}
		}
		if c.stopped {
			return ports.Frame{}, s.stoppedError(c, out)
		}
		g, err := s.geometry(c)
		if err != nil {
			return ports.Frame{}, err
		}
		// A new constraint batch with the same layout changes nothing: the last
		// frame stays valid and the pending capture stays in flight.
		if c.haveLast && c.lastGeom != g {
			c.haveLast = false
		}
		if c.inflight != nil && c.inflightGeom != g {
			s.destroyFrame(c)
		}
		if c.inflight == nil {
			if err := s.startFrame(c, g); err != nil {
				return ports.Frame{}, err
			}
		}
		if err := s.awaitFrame(c, c.haveLast); err != nil {
			return ports.Frame{}, err
		}
		f := c.inflight
		switch {
		case c.stopped:
			return ports.Frame{}, s.stoppedError(c, out)
		case f.ready:
			return s.complete(c, g, f)
		case f.failed:
			if err := s.retryAfterFailure(c, f); err != nil {
				if c.stopped {
					return ports.Frame{}, s.stoppedError(c, out)
				}
				c.fails = 0 // the next call starts afresh
				return ports.Frame{}, err
			}
			if c.stopped {
				return ports.Frame{}, s.stoppedError(c, out)
			}
			if c.haveLast && f.reason == failUnknown {
				return s.lastFrame(c)
			}
		default: // still pending after the poll: repeat the last frame
			return s.lastFrame(c)
		}
	}
}

// awaitFrame dispatches until the in-flight frame ended or the session
// stopped. With poll set it gives up after pollWait.
func (s *Source) awaitFrame(c *session, poll bool) error {
	f := c.inflight
	var deadline time.Time
	if poll {
		deadline = time.Now().Add(s.pollWait)
	}
	for !f.ready && !f.failed && !c.stopped {
		timedOut, err := s.dispatchUntil(deadline)
		if err != nil {
			return err
		}
		if timedOut {
			return nil
		}
	}
	return nil
}

// retryAfterFailure handles a failed frame: one roundtrip, so that a stop or a
// new constraint batch that follows the failure is seen, then a retry.
func (s *Source) retryAfterFailure(c *session, f *frameProxy) error {
	s.destroyFrame(c)
	if f.reason == failStopped {
		c.stopped = true
		return nil
	}
	if c.fails++; c.fails >= maxFrameAttempts {
		return errCompositorFailedCapture
	}
	switch f.reason {
	case failBufferConstraints, failUnknown:
		return s.roundtrip()
	}
	return fmt.Errorf("%w: failure reason %d", errCompositorFailedCapture, f.reason)
}

// complete hands out a frame the compositor finished.
func (s *Source) complete(c *session, g bufGeom, f *frameProxy) (ports.Frame, error) {
	transform := f.transform
	idx := c.inflightBuf
	s.destroyFrame(c)
	if transform != 0 {
		return ports.Frame{}, fmt.Errorf("%w: output transform %d is not supported", ports.ErrUnsupportedTransform, transform)
	}
	c.haveLast, c.cur, c.lastGeom = true, idx, g
	c.served++
	c.fails = 0
	frame, err := s.frameOf(idx, g)
	// From the second frame on the next capture starts right away, so the
	// compositor can answer during the wait for the next slot. A one-shot
	// screenshot never reaches it and allocates a single buffer.
	if err == nil && c.served > 1 {
		err = s.startFrame(c, g)
	}
	return frame, err
}

// lastFrame repeats the last completed frame, still held in its own buffer.
func (s *Source) lastFrame(c *session) (ports.Frame, error) {
	return s.frameOf(c.cur, c.lastGeom)
}

// startFrame sends one capture into the buffer the last frame is not in.
func (s *Source) startFrame(c *session, g bufGeom) error {
	idx := 0
	if c.haveLast {
		idx = 1 - c.cur
	}
	if err := s.ensureBuffer(idx, g); err != nil {
		return err
	}
	f := &frameProxy{}
	f.SetContext(s.wl)
	if err := s.wl.Request(wlturbo.Request{Proxy: c, Opcode: reqSessionCreateFrame, Name: "ext_image_copy_capture_session_v1.create_frame", Child: f}, f); err != nil {
		s.terminate()
		return fmt.Errorf("wayland: create frame: %w", err)
	}
	c.inflight, c.inflightBuf, c.inflightGeom = f, idx, g
	if err := s.wl.Request(wlturbo.Request{Proxy: f, Opcode: reqFrameAttachBuffer, Name: "ext_image_copy_capture_frame_v1.attach_buffer"}, s.bufs[idx].buffer); err != nil {
		s.terminate()
		return fmt.Errorf("wayland: attach buffer: %w", err)
	}
	if err := s.wl.Request(wlturbo.Request{Proxy: f, Opcode: reqFrameDamageBuffer, Name: "ext_image_copy_capture_frame_v1.damage_buffer"}, int32(0), int32(0), int32(g.width), int32(g.height)); err != nil {
		s.terminate()
		return fmt.Errorf("wayland: damage buffer: %w", err)
	}
	if err := s.wl.Request(wlturbo.Request{Proxy: f, Opcode: reqFrameCapture, Name: "ext_image_copy_capture_frame_v1.capture"}); err != nil {
		s.terminate()
		return fmt.Errorf("wayland: capture request: %w", err)
	}
	return nil
}

// destroyFrame destroys the in-flight frame object once it ended.
func (s *Source) destroyFrame(c *session) {
	if f := c.inflight; f != nil {
		c.inflight = nil
		_ = s.wl.Request(wlturbo.Request{Proxy: f, Opcode: reqFrameDestroy, Name: "ext_image_copy_capture_frame_v1.destroy", Destructor: true})
	}
}

// stoppedError classifies a stopped session. An output that went away is the
// usual cause: read its removal first. A workspace session stops when its
// workspace is removed: that is a plain stop, whatever the session served. Any
// other session that stopped before serving a frame was refused by the
// compositor, which the error says.
func (s *Source) stoppedError(c *session, out *output) error {
	if err := s.roundtrip(); err != nil {
		return err
	}
	switch {
	case out.removed:
		return ports.ErrOutputNotFound
	case c.key.workspace != 0 && s.ws.byID[c.key.workspace] == nil:
		return fmt.Errorf("%w: %w", ports.ErrWorkspaceUnavailable, ports.ErrCaptureStopped)
	case c.served == 0:
		return errCaptureRefused
	}
	return ports.ErrCaptureStopped
}

// geometry validates the session's current constraints and returns the buffer
// layout to allocate.
func (s *Source) geometry(c *session) (bufGeom, error) {
	k := c.current
	switch {
	case !k.sawShm:
		return bufGeom{}, fmt.Errorf("%w: compositor offers no wl_shm buffer", ports.ErrUnsupportedFormat)
	case !k.haveFormat:
		return bufGeom{}, fmt.Errorf("%w: compositor offers shm format %#x", ports.ErrUnsupportedFormat, k.unsupported)
	case !k.haveSize:
		return bufGeom{}, errors.New("wayland: compositor sent no buffer size")
	}
	g := bufGeom{format: k.format, width: k.width, height: k.height, stride: k.width * ports.BytesPerPixel}
	if err := g.validate(); err != nil {
		return bufGeom{}, err
	}
	return g, nil
}

func (s *Source) frameOf(idx int, g bufGeom) (ports.Frame, error) {
	frame := ports.Frame{
		Pixels: s.bufs[idx].data[:g.size:g.size],
		Width:  int(g.width), Height: int(g.height), Stride: int(g.stride),
		Format: ports.PixelFormat(g.format),
	}
	if err := frame.Validate(); err != nil {
		return ports.Frame{}, err
	}
	return frame, nil
}

// logicalSize is the output's size in logical pixels: xdg-output when the
// compositor offers it, else the current mode divided by the integer scale.
func (o *output) logicalSize() (w, h int) {
	if o.lw > 0 && o.lh > 0 {
		return int(o.lw), int(o.lh)
	}
	return int(o.width) / int(max(o.scale, 1)), int(o.height) / int(max(o.scale, 1))
}

// cropFrame returns the window of f that covers region r, given in logical
// pixels of out. The result borrows f's storage: no pixel is copied.
func cropFrame(f ports.Frame, r ports.Region, out *output) (ports.Frame, error) {
	lw, lh := out.logicalSize()
	if lw < 1 || lh < 1 {
		return ports.Frame{}, fmt.Errorf("%w: output %s has no known logical size", ports.ErrInvalidRegion, outputName(out))
	}
	edge := func(v, buf, logical int) int { // round to the nearest buffer pixel
		return int((int64(v)*int64(buf) + int64(logical)/2) / int64(logical))
	}
	x0, x1 := edge(r.X, f.Width, lw), min(edge(r.X+r.Width, f.Width, lw), f.Width)
	y0, y1 := edge(r.Y, f.Height, lh), min(edge(r.Y+r.Height, f.Height, lh), f.Height)
	if x0 >= x1 || y0 >= y1 {
		return ports.Frame{}, fmt.Errorf("%w: %+v is outside output %s", ports.ErrInvalidRegion, r, outputName(out))
	}
	w, h := x1-x0, y1-y0
	start := y0*f.Stride + x0*ports.BytesPerPixel
	end := start + (h-1)*f.Stride + w*ports.BytesPerPixel
	cropped := f
	cropped.Pixels = f.Pixels[start:end:end]
	cropped.Width, cropped.Height = w, h
	if err := cropped.Validate(); err != nil {
		return ports.Frame{}, err
	}
	return cropped, nil
}
