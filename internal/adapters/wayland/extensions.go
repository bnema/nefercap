package wayland

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	imagecapturesource "github.com/bnema/go-wayland-bindings/client/extimagecapturesource"
	"github.com/bnema/go-wayland-bindings/client/xdgoutput"
	"github.com/bnema/wlturbo"

	"github.com/bnema/nefercap/internal/ports"
)

// NeferWL's optional extension to ext-image-copy-capture-v1, see
// neferwl-image-capture-v1.xml in the NeferWL repository. Every part is
// optional: without it, capture still works through the standard protocols.
//
//	neferwl_image_capture_source_manager_v1
//	  requests: 0 destroy, 1 create_workspace_source(new_id, workspace),
//	            2 create_output_region_source(new_id, output, x, y, w, h),
//	            3 get_workspace_frame(new_id, workspace)
//	neferwl_workspace_frame_v1
//	  requests: 0 destroy   events: 0 frame(x, y, width, height)
//	neferwl_capture_exclusion_manager_v1
//	  requests: 0 destroy, 1 get_exclusion(new_id, session),
//	            2 attach_surface(new_id, token, surface)
//	neferwl_capture_exclusion_v1
//	  requests: 0 destroy   events: 0 token(string), 1 failed(reason)
//	neferwl_capture_layer_v1
//	  requests: 0 destroy   events: 0 attached, 1 failed(reason), 2 detached(reason)

const (
	sourceManagerIface    = "neferwl_image_capture_source_manager_v1"
	exclusionManagerIface = "neferwl_capture_exclusion_manager_v1"
	extensionVersion      = 1
	tokenLen              = 64 // lowercase hexadecimal characters of 32 random bytes

	reqNeferwlWorkspaceSource = 1
	reqNeferwlRegionSource    = 2
	reqGetWorkspaceFrame      = 3
	reqWorkspaceFrameDestroy  = 0
	reqGetExclusion           = 1
	reqAttachSurface          = 2
	reqManagerDestroy         = 0
	reqExclusionDestroy       = 0
	reqLayerDestroy           = 0

	// DefaultAttachTimeout bounds AuthorizeLayer.
	DefaultAttachTimeout = 5 * time.Second
)

var (
	// ErrExclusionUnsupported means the compositor cannot leave the client's
	// own surfaces out of its captures.
	ErrExclusionUnsupported = errors.New("wayland: capture exclusion unsupported")
	// ErrExclusionUnavailable means the exclusion is gone, refused or never
	// existed. Callers that rely on it must stop recording.
	ErrExclusionUnavailable = errors.New("wayland: capture exclusion unavailable")
	// ErrExclusionBusy is an ErrExclusionUnavailable: another exclusion is live
	// in the compositor.
	ErrExclusionBusy = fmt.Errorf("%w: another exclusion is live", ErrExclusionUnavailable)
	// ErrAttachFailed is matched by every AttachFailedError.
	ErrAttachFailed = errors.New("wayland: attach layer failed")
	// ErrUnauthorized means the compositor refused the peer.
	ErrUnauthorized = errors.New("wayland: capture exclusion peer unauthorized")
	// errLayerProtocol is a violation of the layer attachment state machine.
	errLayerProtocol = fmt.Errorf("%w: capture layer protocol violation", ErrExclusionUnavailable)
)

// neferwl_capture_exclusion_v1.failure
const (
	exclusionBusy           = 0
	exclusionSessionStopped = 1
)

// requestOnly is a bound NeferWL global or created object that never sends
// events: wlturbo has no generated binding for the extension.
type requestOnly struct{ wlturbo.BaseProxy }

func (*requestOnly) EventSignature(uint16) (string, bool) { return "", false }

func newRequestOnly(ctx *wlturbo.Context) *requestOnly {
	p := &requestOnly{}
	p.SetContext(ctx)
	return p
}

// extensions holds the bound optional NeferWL globals.
type extensions struct {
	source    *requestOnly
	exclusion *requestOnly
}

// bindExtras binds every optional global. None is an error when absent.
func (s *Source) bindExtras() error {
	x := xdgoutput.NewZxdgOutputManager(s.wl)
	switch ok, err := s.bindOptional(xdgoutput.ZxdgOutputManagerInterface, xdgOutputVersion, x); {
	case err != nil:
		return err
	case ok:
		s.xdgOutputs = x
	}
	for _, o := range s.outputs {
		s.watchLogicalSize(o)
	}
	var err error
	if s.ext.source, err = s.bindOptionalExtension(sourceManagerIface); err != nil {
		return err
	}
	if s.ext.exclusion, err = s.bindOptionalExtension(exclusionManagerIface); err != nil {
		return err
	}
	return s.bindWorkspaces()
}

// bindOptionalExtension binds a NeferWL global, nil when the compositor has
// none.
func (s *Source) bindOptionalExtension(iface string) (*requestOnly, error) {
	p := newRequestOnly(s.wl)
	if ok, err := s.bindOptional(iface, extensionVersion, p); err != nil || !ok {
		return nil, err
	}
	return p, nil
}

// createWorkspaceSource makes a source that follows the workspace id.
func (s *Source) createWorkspaceSource(id uint64) (*imagecapturesource.ExtImageCaptureSource, error) {
	h := s.ws.byID[id]
	if h == nil || h.removed {
		return nil, ports.ErrWorkspaceUnavailable
	}
	src := imagecapturesource.NewExtImageCaptureSource(s.wl)
	err := s.wl.Request(wlturbo.Request{Proxy: s.ext.source, Opcode: reqNeferwlWorkspaceSource, Name: "neferwl_image_capture_source_manager_v1.create_workspace_source", Child: src}, src, h.proxy)
	return src, err
}

// createRegionSource makes a source of a region of out, in logical pixels.
func (s *Source) createRegionSource(out *output, r ports.Region) (*imagecapturesource.ExtImageCaptureSource, error) {
	src := imagecapturesource.NewExtImageCaptureSource(s.wl)
	err := s.wl.Request(wlturbo.Request{Proxy: s.ext.source, Opcode: reqNeferwlRegionSource, Name: "neferwl_image_capture_source_manager_v1.create_output_region_source", Child: src},
		src, out.proxy, int32(r.X), int32(r.Y), int32(r.Width), int32(r.Height))
	return src, err
}

// workspaceFrame is one neferwl_workspace_frame_v1: the frame of a workspace
// in output-local logical pixels. It is read under opMu, with every other
// state of the connection, so it needs no lock of its own.
type workspaceFrame struct {
	wlturbo.BaseProxy
	x, y, w, h int32
	known      bool
}

func (*workspaceFrame) EventSignature(op uint16) (string, bool) {
	if op == 0 {
		return "int,int,int,int,", true
	}
	return "", false
}

func (f *workspaceFrame) Dispatch(ev *wlturbo.Event) {
	if ev.Opcode == 0 {
		f.x, f.y, f.w, f.h = ev.Int32(), ev.Int32(), ev.Int32(), ev.Int32()
		f.known = true
	}
}

// region is the frame when it is a sane rectangle.
func (f *workspaceFrame) region() (ports.Region, bool) {
	if f == nil || !f.known || f.w < 1 || f.h < 1 || f.w > ports.MaxDimension || f.h > ports.MaxDimension ||
		f.x < -ports.MaxDimension || f.x > ports.MaxDimension || f.y < -ports.MaxDimension || f.y > ports.MaxDimension {
		return ports.Region{}, false
	}
	return ports.Region{X: int(f.x), Y: int(f.y), Width: int(f.w), Height: int(f.h)}, true
}

// requestFrames asks for the frame of every workspace that has none yet and
// reports whether it asked for any. It does nothing without the extension.
func (s *Source) requestFrames() (bool, error) {
	if s.ext.source == nil {
		return false, nil
	}
	asked := false
	for _, w := range s.ws.byID {
		if w.frame != nil || w.removed {
			continue
		}
		f := &workspaceFrame{}
		f.SetContext(s.wl)
		if err := s.wl.Request(wlturbo.Request{Proxy: s.ext.source, Opcode: reqGetWorkspaceFrame, Name: "neferwl_image_capture_source_manager_v1.get_workspace_frame", Child: f}, f, w.proxy); err != nil {
			s.terminate()
			return false, fmt.Errorf("wayland: get workspace frame: %w", err)
		}
		w.frame, asked = f, true
	}
	return asked, nil
}

// releaseWorkspaceFrame releases the frame object of a workspace that is gone.
func (s *Source) releaseWorkspaceFrame(w *workspace) {
	if w.frame == nil {
		return
	}
	_ = s.wl.Request(wlturbo.Request{Proxy: w.frame, Opcode: reqWorkspaceFrameDestroy, Name: "neferwl_workspace_frame_v1.destroy", Destructor: true})
	w.frame = nil
}

// exclusion is one neferwl_capture_exclusion_v1: it answers with a token or a
// failure, and ends with its capture session.
type exclusion struct {
	wlturbo.BaseProxy
	token  string
	failed bool
	reason uint32
}

func (*exclusion) EventSignature(op uint16) (string, bool) {
	switch op {
	case 0:
		return "string,", true
	case 1:
		return "uint,", true
	}
	return "", false
}

func (e *exclusion) Dispatch(ev *wlturbo.Event) {
	switch ev.Opcode {
	case 0:
		if tok := ev.String(); validToken(tok) && !e.failed && e.token == "" {
			e.token = tok
		} else {
			e.failed, e.reason = true, ^uint32(0)
		}
	case 1:
		if e.token == "" && !e.failed {
			e.failed, e.reason = true, ev.Uint32()
		}
	}
}

func (e *exclusion) destroy(s *Source) {
	_ = s.wl.Request(wlturbo.Request{Proxy: e, Opcode: reqExclusionDestroy, Name: "neferwl_capture_exclusion_v1.destroy", Destructor: true})
}

// BeginExclusion asks the compositor to leave the client's own layer surfaces
// out of the frames of the session that captures t, and returns the token a
// HUD on another connection passes to AuthorizeLayer. The token is a secret:
// never log it. The exclusion ends with the session, which later Capture
// calls on the same target keep using; capturing another target replaces the
// session and ends the exclusion.
func (s *Source) BeginExclusion(ctx context.Context, t ports.Target) (string, error) {
	if err := validRegion(t.Region); err != nil {
		return "", err
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if err := s.begin(ctx); err != nil {
		return "", err
	}
	token, err := s.beginExclusion(t)
	if err = s.finish(ctx, err); err != nil {
		return "", err
	}
	return token, nil
}

func (s *Source) beginExclusion(t ports.Target) (string, error) {
	if s.ext.exclusion == nil {
		return "", ErrExclusionUnsupported
	}
	if err := s.settle(); err != nil {
		return "", err
	}
	out := s.outputs[t.OutputID]
	if out == nil {
		return "", ports.ErrOutputNotFound
	}
	key, _, err := s.resolve(t, out)
	if err != nil {
		return "", err
	}
	c, err := s.sessionFor(key, out)
	if err != nil {
		return "", err
	}
	if c.exclusion != nil {
		return "", errors.New("wayland: the capture session already has an exclusion")
	}
	e := &exclusion{}
	e.SetContext(s.wl)
	if err := s.wl.Request(wlturbo.Request{Proxy: s.ext.exclusion, Opcode: reqGetExclusion, Name: "neferwl_capture_exclusion_manager_v1.get_exclusion", Child: e}, e, c.proxy); err != nil {
		s.terminate()
		return "", fmt.Errorf("wayland: get exclusion: %w", err)
	}
	for e.token == "" && !e.failed && !c.stopped {
		if err := s.dispatch(); err != nil {
			return "", err
		}
	}
	if e.token != "" {
		c.exclusion = e // only a live exclusion makes a stopped session sticky
		return e.token, nil
	}
	// Every other outcome (refused, invalid token, session stopped first)
	// leaves no exclusion behind.
	e.destroy(s)
	switch {
	case c.stopped || (e.failed && e.reason == exclusionSessionStopped):
		return "", s.stoppedError(c, out)
	case e.failed && e.reason == exclusionBusy:
		return "", ErrExclusionBusy
	default:
		return "", fmt.Errorf("%w: refused (reason %d)", ErrExclusionUnavailable, e.reason)
	}
}

// AttachFailure is neferwl_capture_layer_v1.failure.
type AttachFailure uint32

const (
	AttachUnknownToken    AttachFailure = 0
	AttachUnauthorized    AttachFailure = 1
	AttachTooManyLayers   AttachFailure = 2
	AttachAlreadyAttached AttachFailure = 3
	AttachExclusionEnded  AttachFailure = 4
	AttachLayerDestroyed  AttachFailure = 5
)

// AttachFailedError is the compositor's refusal of AuthorizeLayer. It matches
// ErrAttachFailed; a missing or ended exclusion also matches
// ErrExclusionUnavailable, and an unauthorized peer ErrUnauthorized.
type AttachFailedError struct{ Reason AttachFailure }

func (e *AttachFailedError) Error() string {
	return fmt.Sprintf("%v (reason %d)", ErrAttachFailed, e.Reason)
}

func (e *AttachFailedError) Is(target error) bool {
	switch target {
	case ErrAttachFailed:
		return true
	case ErrExclusionUnavailable:
		return e.Reason == AttachUnknownToken || e.Reason == AttachExclusionEnded
	case ErrUnauthorized:
		return e.Reason == AttachUnauthorized
	}
	return false
}

// DetachReason is neferwl_capture_layer_v1.detach_reason.
type DetachReason uint32

const (
	// DetachExclusionEnded: the exclusion ended and the compositor closed the
	// layer surface, which the client must destroy.
	DetachExclusionEnded DetachReason = 0
	// DetachLayerDestroyed: the layer role was destroyed.
	DetachLayerDestroyed DetachReason = 1
)

// LayerDetachedError is what onDetached receives. An exclusion end also
// matches ErrExclusionUnavailable.
type LayerDetachedError struct{ Reason DetachReason }

func (e *LayerDetachedError) Error() string {
	return fmt.Sprintf("wayland: layer detached from the capture exclusion (reason %d)", e.Reason)
}

func (e *LayerDetachedError) Is(target error) bool {
	return target == ErrExclusionUnavailable && e.Reason == DetachExclusionEnded
}

// layerProxy is one neferwl_capture_layer_v1 attachment. Its events are
// dispatched by whichever goroutine reads the GUI connection: during
// authorizeLayer that is the caller, afterwards the connection's reader.
//
// State machine: pending -> attached | failed; attached -> detached. Any other
// event is a protocol violation: the exclusion cannot be trusted any more.
type layerProxy struct {
	wlturbo.BaseProxy
	display    *wlturbo.Display
	onDetached func(error)

	mu         sync.Mutex
	state      layerState
	failReason uint32
	violation  bool
	returned   bool // authorizeLayer handed the surface to its caller
	notified   bool
}

type layerState int

const (
	layerPending layerState = iota
	layerAttached
	layerFailed
	layerDetached
)

func (*layerProxy) EventSignature(op uint16) (string, bool) {
	switch op {
	case 0:
		return "", true
	case 1, 2:
		return "uint,", true
	}
	return "", false
}

func (l *layerProxy) Dispatch(e *wlturbo.Event) {
	var arg uint32
	if e.Opcode != 0 {
		arg = e.Uint32()
	}
	var notify error
	closeDisplay := false
	l.mu.Lock()
	returned := l.returned
	switch {
	case l.violation:
		// Terminal: the display is closed already.
	case e.Opcode == 0 && l.state == layerPending:
		l.state = layerAttached
	case e.Opcode == 1 && l.state == layerPending:
		l.state, l.failReason = layerFailed, arg
	case e.Opcode == 2 && l.state == layerAttached:
		l.state = layerDetached
		if returned {
			l.notified, notify = true, &LayerDetachedError{Reason: DetachReason(arg)}
		}
	default:
		l.violation, closeDisplay = true, true
		if returned && !l.notified {
			l.notified, notify = true, errLayerProtocol
		}
	}
	l.mu.Unlock()
	if closeDisplay && returned {
		_ = l.display.Close() // the surface could be visible in captures
	}
	if notify != nil && l.onDetached != nil {
		l.onDetached(notify)
	}
}

// settled reports that the answer arrived, or the layer misbehaved.
func (l *layerProxy) settled() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state != layerPending || l.violation
}

// hand marks the layer as owned by the caller of authorizeLayer, and reports
// its outcome so far.
func (l *layerProxy) hand() (state layerState, reason uint32, violation bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.returned = true
	return l.state, l.failReason, l.violation
}

// AuthorizeLayer tells the compositor, from a connection other than the
// capture connection, that surface is a layer surface of the capturing peer
// and must be excluded from the frames of the session that owns token (from
// Source.BeginExclusion). Call it after the layer-shell role is assigned and
// before the surface is mapped; when it returns nil the compositor has
// confirmed the exclusion for this very attachment, so the surface is safe to
// show.
//
// AuthorizeLayer dispatches display's events itself until the attachment
// answers, so it must run before the connection's event loop starts. A
// refusal returns an *AttachFailedError. A protocol error is returned as the
// display's error. A cancelled ctx, or no answer within DefaultAttachTimeout
// (an error wrapping context.DeadlineExceeded), closes display before
// returning: the surface must then stay unmapped. So does a violation of the
// attachment protocol (an answer twice, a detach before the answer, ...).
//
// After it returns nil the attachment stays until the exclusion ends or the
// layer is destroyed; then onDetached (may be nil) receives a
// *LayerDetachedError, once. A violation at that point closes display and
// calls onDetached with an error matching ErrExclusionUnavailable; the surface
// may still draw until the caller stops, so onDetached must make the caller
// stop. It runs on the goroutine that reads display, so it must only do
// thread-safe, non-blocking work such as cancelling a context. It never sees
// the token. The attachment object is never destroyed after a success: that
// would not release the exclusion, and closing the connection ends it.
//
// surface is the layer's wl_surface proxy, whichever bindings created it; it
// must belong to display.
func AuthorizeLayer(ctx context.Context, display *wlturbo.Display, surface wlturbo.Proxy, token string, onDetached func(error)) error {
	return authorizeLayer(ctx, display, surface, token, onDetached, DefaultAttachTimeout)
}

func authorizeLayer(ctx context.Context, display *wlturbo.Display, surface wlturbo.Proxy, token string, onDetached func(error), timeout time.Duration) error {
	// A typed nil proxy is a non-nil interface: check the value too.
	if display == nil || surface == nil || reflect.ValueOf(surface).IsNil() || surface.Context() != display.Context() {
		return errors.New("wayland: authorize layer: surface does not belong to display")
	}
	if !validToken(token) {
		return errors.New("wayland: authorize layer: invalid token")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m := newRequestOnly(display.Context())
	if _, err := display.Registry().BindNegotiated(exclusionManagerIface, extensionVersion, m); err != nil {
		if errors.Is(err, wlturbo.ErrGlobalNotFound) {
			return ErrExclusionUnsupported
		}
		return fmt.Errorf("wayland: bind %s: %w", exclusionManagerIface, err)
	}
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stop := context.AfterFunc(wctx, func() { _ = display.Close() })
	defer stop()

	l := &layerProxy{display: display, onDetached: onDetached}
	l.SetContext(display.Context())
	err := display.Context().Request(wlturbo.Request{Proxy: m, Opcode: reqAttachSurface, Name: "neferwl_capture_exclusion_manager_v1.attach_surface", Child: l}, l, token, surface)
	for err == nil && !l.settled() {
		err = display.Dispatch()
	}
	// Detach the watcher before anything else is decided or sent. When stop
	// reports false the watcher has fired or is firing: cancellation and the
	// deadline win over any outcome, and the display is closed here too so it
	// is closed on return whatever the watcher goroutine is doing. When it
	// reports true the watcher can never fire, and the caller owns the
	// connection; a context that is already done is still caught below.
	if !stop() || wctx.Err() != nil {
		_ = display.Close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("wayland: attach surface: %w", wctx.Err())
	}
	if err != nil {
		return fmt.Errorf("wayland: attach surface: %w", err)
	}
	state, reason, violation := l.hand()
	if violation {
		_ = display.Close()
		return errLayerProtocol
	}
	// The manager binding is not needed any more.
	_ = display.Context().Request(wlturbo.Request{Proxy: m, Opcode: reqManagerDestroy, Name: "neferwl_capture_exclusion_manager_v1.destroy", Destructor: true})
	if state == layerFailed {
		_ = display.Context().Request(wlturbo.Request{Proxy: l, Opcode: reqLayerDestroy, Name: "neferwl_capture_layer_v1.destroy", Destructor: true})
		return &AttachFailedError{Reason: AttachFailure(reason)}
	}
	return nil
}

// validToken accepts exactly the compositor's token format.
func validToken(token string) bool {
	if len(token) != tokenLen {
		return false
	}
	for i := range len(token) {
		if c := token[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
