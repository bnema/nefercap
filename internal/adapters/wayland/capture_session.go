package wayland

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bnema/wlturbo"
	"github.com/bnema/wlturbo/protocol/core"

	"github.com/bnema/nefercap/internal/ports"
)

// Native private capture sessions.
//
// A session is a request to the compositor, on the capture connection itself,
// to keep the capturing client's own surfaces out of the frames it copies.
// The session belongs to the connection that began it: the compositor drops it
// when that connection closes. A second connection (the selection indicator)
// proves it belongs to the same peer by presenting the opaque session token
// through AuthorizeLayer.
//
// The token is a credential. It is never logged and never printed by
// SessionState's String method.

const (
	captureManagerIface   = "neferwl_capture_manager_v1"
	captureManagerVersion = 1
	tokenLen              = 64 // lowercase hexadecimal characters of 32 random bytes
	sessionPingInterval   = 2 * time.Second
	maxRegionRetries      = 2
	maxWorkspaces         = 256 // bound of the workspace inventory
	maxNameLen            = 256 // bytes, for output and workspace names

	// DefaultAttachTimeout bounds AuthorizeLayer.
	DefaultAttachTimeout = 5 * time.Second
)

var (
	// ErrSessionUnsupported means the compositor has no private capture
	// protocol, or the request cannot be served by the session (for example a
	// workspace capture without a session on that workspace).
	ErrSessionUnsupported = errors.New("wayland: private capture session unsupported")
	// ErrSessionUnavailable means the session is gone or was never started.
	// Callers that rely on the exclusion must stop recording.
	ErrSessionUnavailable = errors.New("wayland: private capture session unavailable")
	// ErrSessionActive reports BeginSession while a session is already active.
	ErrSessionActive = errors.New("wayland: private capture session already active")
	// ErrWorkspaceNotFound reports an unknown workspace ID.
	ErrWorkspaceNotFound = errors.New("wayland: workspace not found")
	// ErrSessionInactive means the compositor reported the session paused
	// (state active 0). The current compositor never does; the case is handled
	// defensively. It is transient: no frame is returned until it resumes.
	ErrSessionInactive = errors.New("wayland: private capture session paused")
	// ErrSessionGeometryChanging is transient: the session's region kept
	// changing during the bounded captures, so no frame is returned.
	ErrSessionGeometryChanging = errors.New("wayland: private capture session geometry is changing")
	// ErrServerMetadata means the compositor sent private-protocol metadata
	// that is out of bounds or hostile. The private capability is then
	// disabled and an active session fails closed; standard capture of
	// outputs is not affected.
	ErrServerMetadata = errors.New("wayland: invalid private capture metadata")
	// errLayerProtocol is a violation of the layer attachment state machine.
	errLayerProtocol = fmt.Errorf("%w: capture layer protocol violation", ErrSessionUnavailable)
	// ErrAttachFailed is matched by every AttachFailedError.
	ErrAttachFailed = errors.New("wayland: attach layer failed")
	// ErrUnauthorized means the compositor refused the peer.
	ErrUnauthorized            = errors.New("wayland: capture session peer unauthorized")
	errCompositorFailedCapture = errors.New("wayland: compositor failed the capture")
)

// StopReason is neferwl_capture_session_v1.stop_reason.
type StopReason uint32

const (
	StopRequested       StopReason = 0
	StopOutputGone      StopReason = 1
	StopOutputOff       StopReason = 2
	StopPingTimeout     StopReason = 3
	StopWorkspaceGone   StopReason = 5
	StopBusy            StopReason = 6
	StopInvalidRegion   StopReason = 7
	StopUnauthorized    StopReason = 8
	StopTooManyExcluded StopReason = 9
)

// SessionStoppedError reports that the compositor ended the session, or
// refused it. It matches ErrSessionUnavailable.
type SessionStoppedError struct{ Reason StopReason }

func (e *SessionStoppedError) Error() string {
	return fmt.Sprintf("%v: stopped by the compositor (reason %d)", ErrSessionUnavailable, e.Reason)
}

func (e *SessionStoppedError) Is(target error) bool { return target == ErrSessionUnavailable }

// AttachFailure is neferwl_capture_layer_v1.failure.
type AttachFailure uint32

const (
	AttachUnknownToken    AttachFailure = 0
	AttachUnauthorized    AttachFailure = 1
	AttachTooManyLayers   AttachFailure = 2
	AttachAlreadyAttached AttachFailure = 3
	AttachSessionEnded    AttachFailure = 4
	AttachLayerDestroyed  AttachFailure = 5
)

// AttachFailedError is the compositor's refusal of AuthorizeLayer. It matches
// ErrAttachFailed; a missing or ended session also matches
// ErrSessionUnavailable, and an unauthorized peer ErrUnauthorized.
type AttachFailedError struct{ Reason AttachFailure }

func (e *AttachFailedError) Error() string {
	return fmt.Sprintf("%v (reason %d)", ErrAttachFailed, e.Reason)
}

func (e *AttachFailedError) Is(target error) bool {
	switch target {
	case ErrAttachFailed:
		return true
	case ErrSessionUnavailable:
		return e.Reason == AttachUnknownToken || e.Reason == AttachSessionEnded
	case ErrUnauthorized:
		return e.Reason == AttachUnauthorized
	}
	return false
}

// DetachReason is neferwl_capture_layer_v1.detach_reason.
type DetachReason uint32

const (
	// DetachSessionEnded: the session stopped and the compositor closed the
	// layer surface, which the client must destroy.
	DetachSessionEnded DetachReason = 0
	// DetachLayerDestroyed: the layer role was destroyed.
	DetachLayerDestroyed DetachReason = 1
)

// LayerDetachedError is what onDetached receives. A session end also matches
// ErrSessionUnavailable.
type LayerDetachedError struct{ Reason DetachReason }

func (e *LayerDetachedError) Error() string {
	return fmt.Sprintf("wayland: layer detached from the capture session (reason %d)", e.Reason)
}

func (e *LayerDetachedError) Is(target error) bool {
	return target == ErrSessionUnavailable && e.Reason == DetachSessionEnded
}

// SessionState is a snapshot of a session. Token is opaque.
type SessionState struct {
	// Token is a secret: it is omitted from JSON, String and GoString.
	Token string `json:"-"`
	// Revision grows whenever the compositor changes the session's
	// configuration (layers, popups, target, pause). It starts at 1.
	Revision uint64
	// Target is directly usable with Capture. For an output session it holds
	// the output and the granted Region. For a workspace session it holds the
	// output and WorkspaceID with a zero Region: the workspace frame is
	// resolved live by the Source, so Region is never a stale crop.
	Target ports.Target
	// Region is the geometry the compositor granted, output-local logical
	// pixels, at the time of this snapshot (for display, e.g. the indicator).
	Region ports.Region
	Active bool
}

// String omits the token.
func (s SessionState) String() string {
	return fmt.Sprintf("session{target:%+v active:%t}", s.Target, s.Active)
}

// GoString omits the token from %#v as well.
func (s SessionState) GoString() string { return s.String() }

// MarshalJSON omits the token, also when SessionState is embedded or logged
// through a structured logger.
func (s SessionState) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Revision uint64
		Target   ports.Target
		Region   ports.Region
		Active   bool
	}{s.Revision, s.Target, s.Region, s.Active})
}

// ---------------------------------------------------------------------------
// Wire layer: neferwl-capture-v1.xml of the compositor, interface versions 1.
//
//	neferwl_capture_manager_v1
//	  requests: 0 destroy, 1 begin_session(new_id, output, x, y, w, h,
//	            workspace_hi, workspace_lo, record),
//	            2 attach_surface(new_id layer, token, surface)
//	  events:   0 workspace(hi, lo, output_name, name, x, y, w, h, active)
//	            1 workspace_removed(hi, lo)
//	neferwl_capture_layer_v1
//	  requests: 0 destroy
//	  events:   0 attached   1 failed(reason)   2 detached(reason)
//	neferwl_capture_session_v1
//	  requests: 0 destroy, 1 ping
//	  events:   0 state(token, active, x, y, w, h, workspace_hi, workspace_lo,
//	            revision_hi, revision_lo)   1 stopped(reason)
// ---------------------------------------------------------------------------

const (
	reqBeginSession   = 1
	reqAttachSurface  = 2
	reqManagerDestroy = 0
	reqLayerDestroy   = 0
	reqSessionDestroy = 0
	reqSessionPing    = 1
)

// captureManager is the bound manager. s is nil on a connection that only
// authorizes a layer surface: workspace events are then ignored.
type captureManager struct {
	wlturbo.BaseProxy
	s *Source
}

func (*captureManager) EventSignature(op uint16) (string, bool) {
	switch op {
	case 0:
		return "uint,uint,string,string,int,int,int,int,uint,", true
	case 1:
		return "uint,uint,", true
	}
	return "", false
}

func (m *captureManager) Dispatch(e *wlturbo.Event) {
	if m.s == nil {
		return
	}
	switch e.Opcode {
	case 0:
		hi, lo := e.Uint32(), e.Uint32()
		outName, name := e.String(), e.String()
		x, y, w, h := e.Int32(), e.Int32(), e.Int32(), e.Int32()
		m.s.onWorkspace(join64(hi, lo), outName, name, ports.Region{X: int(x), Y: int(y), Width: int(w), Height: int(h)}, e.Uint32() != 0)
	case 1:
		if m.s.metaErr == nil {
			delete(m.s.workspaces, join64(e.Uint32(), e.Uint32()))
		}
	}
}

// join64 joins the hi and lo halves of a 64-bit wire value.
func join64(hi, lo uint32) uint64 { return uint64(hi)<<32 | uint64(lo) }

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

// captureSession is one private capture session proxy.
type captureSession struct {
	wlturbo.BaseProxy

	// Pinned at begin: what the owner asked for and where.
	outputID           uint32
	requestedWorkspace uint64
	requestedRegion    ports.Region // output targets only; zero is the full output

	haveState bool
	token     string
	active    bool
	region    ports.Region // last granted geometry
	revision  uint64

	stopped    bool
	stopReason uint32
	violation  bool // the compositor broke the protocol: fail closed

	lastPing  time.Time // monotonic
	destroyed bool
}

func (*captureSession) EventSignature(op uint16) (string, bool) {
	switch op {
	case 0:
		return "string,uint,int,int,int,int,uint,uint,uint,uint,", true
	case 1:
		return "uint,", true
	}
	return "", false
}

func (c *captureSession) Dispatch(e *wlturbo.Event) {
	switch e.Opcode {
	case 0:
		token, active := e.String(), e.Uint32() != 0
		x, y, w, h := e.Int32(), e.Int32(), e.Int32(), e.Int32()
		ws := join64(e.Uint32(), e.Uint32())
		rev := join64(e.Uint32(), e.Uint32())
		if c.lost() {
			return // final: later events change nothing
		}
		grant := ports.Region{X: int(x), Y: int(y), Width: int(w), Height: int(h)}
		switch {
		case !c.haveState && !validToken(token),
			c.haveState && token != c.token,
			rev == 0 || rev < c.revision, // revisions start at 1 and never go back
			!c.validGrant(grant, ws):
			c.violation = true
			return
		}
		c.haveState, c.token = true, token
		c.active, c.revision, c.region = active, rev, grant
	case 1:
		c.stopped, c.stopReason = true, e.Uint32()
	}
}

// validGrant checks a granted geometry against what was requested. A zero,
// negative or oversized rectangle is never read as "the full output".
func (c *captureSession) validGrant(g ports.Region, workspace uint64) bool {
	if workspace != c.requestedWorkspace || !boundedRegion(g) {
		return false
	}
	if c.requestedWorkspace != 0 || c.requestedRegion == (ports.Region{}) {
		// The workspace frame may move and resize; the full output has no
		// exactly known logical size (scales can be fractional), so only the
		// hard bounds apply.
		return true
	}
	r := c.requestedRegion
	return g.X >= r.X && g.Y >= r.Y &&
		int64(g.X)+int64(g.Width) <= int64(r.X)+int64(r.Width) &&
		int64(g.Y)+int64(g.Height) <= int64(r.Y)+int64(r.Height)
}

// boundedRegion reports a positive rectangle inside the maximum dimensions.
func boundedRegion(r ports.Region) bool {
	return r.X >= 0 && r.Y >= 0 && r.Width >= 1 && r.Height >= 1 &&
		r.Width <= ports.MaxDimension && r.Height <= ports.MaxDimension &&
		int64(r.X)+int64(r.Width) <= ports.MaxDimension &&
		int64(r.Y)+int64(r.Height) <= ports.MaxDimension
}

// lost reports that the exclusion can no longer be relied upon.
func (c *captureSession) lost() bool { return c.stopped || c.violation }

// begun reports that the compositor answered begin_session with a state.
func (c *captureSession) begun() bool { return c.haveState }

// workspace is the client-side state of one advertised workspace.
type workspace struct {
	id     uint64
	output string
	name   string
	region ports.Region
	active bool
}

func (s *Source) onWorkspace(id uint64, outName, name string, r ports.Region, active bool) {
	if s.metaErr != nil {
		return
	}
	switch {
	case !validName(outName, false), !validName(name, true):
		s.failMetadata()
	case id == 0 || !boundedRegion(r):
		delete(s.workspaces, id) // unusable announcement
	case s.workspaces[id] == nil && len(s.workspaces) >= maxWorkspaces:
		s.failMetadata()
	default:
		s.workspaces[id] = &workspace{id: id, output: outName, name: name, region: r, active: active}
	}
}

// failMetadata disables the private capability: the inventory is emptied, and
// an active session fails closed. Standard output capture is unaffected.
func (s *Source) failMetadata() {
	s.metaErr = fmt.Errorf("%w: %w", ErrSessionUnavailable, ErrServerMetadata)
	clear(s.workspaces)
	if s.session != nil {
		s.session.violation = true
	}
	s.log.Warn().Msg("private capture metadata rejected")
}

// validName accepts a bounded, valid UTF-8, single-line name without control
// or bidirectional formatting characters.
func validName(n string, emptyOK bool) bool {
	if len(n) > maxNameLen || (n == "" && !emptyOK) || !utf8.ValidString(n) {
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
		return fmt.Sprintf("output-%d", o.id)
	}
	return o.name
}

// bindCaptureManager binds the private manager when the compositor offers it.
// Its absence is not an error: the session methods then report
// ErrSessionUnsupported.
func (s *Source) bindCaptureManager() error {
	s.workspaces = make(map[uint64]*workspace)
	m := &captureManager{s: s}
	m.SetContext(s.wl)
	_, err := s.display.Registry().BindNegotiated(captureManagerIface, captureManagerVersion, m)
	switch {
	case err == nil:
		s.private = m
		return nil
	case errors.Is(err, wlturbo.ErrGlobalNotFound):
		return nil
	}
	return fmt.Errorf("wayland: bind %s: %w", captureManagerIface, err)
}

func (s *Source) newSessionProxy() *captureSession {
	c := &captureSession{}
	c.SetContext(s.wl)
	return c
}

func (s *Source) destroySession(c *captureSession) {
	if c.destroyed {
		return
	}
	c.destroyed = true
	_ = s.wl.Request(wlturbo.Request{Proxy: c, Opcode: reqSessionDestroy, Name: "neferwl_capture_session_v1.destroy", Destructor: true})
}

// Workspaces lists the workspaces the compositor advertises through the
// private capture protocol, by ID. It returns ErrSessionUnsupported when the
// compositor has no such protocol: workspace identities are never guessed.
func (s *Source) Workspaces(ctx context.Context) ([]ports.Workspace, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if err := s.begin(ctx); err != nil {
		return nil, err
	}
	err := s.settle()
	if ferr := s.finish(ctx, err); ferr != nil {
		return nil, ferr
	}
	if s.private == nil {
		return nil, ErrSessionUnsupported
	}
	if s.metaErr != nil {
		return nil, s.metaErr
	}
	return s.workspaceList(), nil
}

func (s *Source) workspaceList() []ports.Workspace {
	byName := make(map[string]uint32, len(s.outputs))
	for _, o := range s.outputs {
		byName[outputName(o)] = o.id
	}
	list := make([]ports.Workspace, 0, len(s.workspaces))
	for _, w := range s.workspaces {
		id, ok := byName[w.output]
		if !ok {
			continue // its output is not (or no longer) announced
		}
		list = append(list, ports.Workspace{ID: w.id, OutputID: id, Name: w.name, Region: w.region, Active: w.active})
	}
	slices.SortFunc(list, func(a, b ports.Workspace) int { return cmp.Compare(a.ID, b.ID) })
	return list
}

// BeginSession starts the private capture session of this connection for
// target and waits until the compositor confirms it active. record tells the
// compositor whether the session backs a recording. The Source owns at most
// one session; it ends with EndSession, a compositor stop, or Close.
//
// Output targets (with an optional output-local region) and workspace targets
// are accepted. A workspace target needs a workspace advertised by Workspaces,
// on screen or not, and no region: the request carries a zero region and the
// compositor grants the workspace frame. The output, the workspace and the
// requested region are pinned for the session; the compositor's state events
// are validated against them.
func (s *Source) BeginSession(ctx context.Context, target ports.Target, record bool) (SessionState, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if err := s.begin(ctx); err != nil {
		return SessionState{}, err
	}
	state, err := s.beginSession(target, record)
	if err = s.finish(ctx, err); err != nil {
		return SessionState{}, err
	}
	return state, nil
}

func (s *Source) beginSession(target ports.Target, record bool) (SessionState, error) {
	if s.private == nil {
		return SessionState{}, ErrSessionUnsupported
	}
	if s.session != nil {
		return SessionState{}, ErrSessionActive
	}
	if err := s.settle(); err != nil { // current outputs and workspaces
		return SessionState{}, err
	}
	if s.metaErr != nil {
		return SessionState{}, s.metaErr
	}
	out, region, err := s.sessionTarget(target)
	if err != nil {
		return SessionState{}, err
	}
	c := s.newSessionProxy()
	c.outputID, c.requestedWorkspace, c.requestedRegion = out.id, target.WorkspaceID, region
	hi, lo := uint32(target.WorkspaceID>>32), uint32(target.WorkspaceID)
	rec := uint32(0)
	if record {
		rec = 1
	}
	err = s.wl.Request(wlturbo.Request{Proxy: s.private, Opcode: reqBeginSession, Name: "neferwl_capture_manager_v1.begin_session", Child: c},
		c, out.proxy, int32(region.X), int32(region.Y), int32(region.Width), int32(region.Height), hi, lo, rec)
	if err != nil {
		s.terminate()
		return SessionState{}, fmt.Errorf("wayland: begin session: %w", err)
	}
	// The answer is a state (possibly paused) or a stop without a state.
	for !c.begun() && !c.lost() {
		if err := s.dispatch(); err != nil {
			return SessionState{}, err
		}
	}
	if c.lost() {
		s.destroySession(c)
		if err := s.roundtrip(); err != nil {
			return SessionState{}, err
		}
		return SessionState{}, sessionLostError(c)
	}
	c.lastPing = time.Now()
	s.session = c
	return snapshot(c), nil
}

// sessionTarget resolves target to an output and the region to request: zero
// for a workspace, the whole frame.
func (s *Source) sessionTarget(t ports.Target) (*output, ports.Region, error) {
	if t.WorkspaceID != 0 {
		ws := s.workspaces[t.WorkspaceID]
		if ws == nil {
			return nil, ports.Region{}, ErrWorkspaceNotFound
		}
		if t.Region != (ports.Region{}) {
			return nil, ports.Region{}, fmt.Errorf("%w: a workspace target has no region", ports.ErrInvalidRegion)
		}
		out := s.outputByName(ws.output)
		if out == nil || (t.OutputID != 0 && t.OutputID != out.id) {
			return nil, ports.Region{}, ports.ErrOutputNotFound
		}
		return out, ports.Region{}, nil
	}
	if err := validRegion(t.Region); err != nil {
		return nil, ports.Region{}, err
	}
	out := s.outputs[t.OutputID]
	if out == nil {
		return nil, ports.Region{}, ports.ErrOutputNotFound
	}
	return out, t.Region, nil
}

func (s *Source) outputByName(name string) *output {
	for _, o := range s.outputs {
		if outputName(o) == name {
			return o
		}
	}
	return nil
}

func snapshot(c *captureSession) SessionState {
	target := ports.Target{OutputID: c.outputID, Region: c.region}
	if c.requestedWorkspace != 0 {
		target = ports.Target{OutputID: c.outputID, WorkspaceID: c.requestedWorkspace}
	}
	return SessionState{
		Token:    c.token,
		Revision: c.revision,
		Target:   target,
		Region:   c.region,
		Active:   c.active && !c.lost(),
	}
}

// EndSession ends the session. It is a no-op without one and is safe after the
// compositor stopped the session.
func (s *Source) EndSession(ctx context.Context) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if err := s.begin(ctx); err != nil {
		return err
	}
	var err error
	if c := s.session; c != nil {
		s.session = nil
		s.destroySession(c)
		err = s.roundtrip()
	}
	return s.finish(ctx, err)
}

// PingSession sends a keep-alive and reports ErrSessionUnavailable when the
// compositor has stopped the session, or when none is active.
func (s *Source) PingSession(ctx context.Context) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if err := s.begin(ctx); err != nil {
		return err
	}
	err := s.pingSession()
	return s.finish(ctx, err)
}

func (s *Source) pingSession() error {
	c := s.session
	if c == nil {
		return fmt.Errorf("%w: no active session", ErrSessionUnavailable)
	}
	if c.lost() {
		return sessionLostError(c)
	}
	if err := s.sendPing(c); err != nil {
		return err
	}
	if err := s.roundtrip(); err != nil { // observe a stop that answers the ping
		return err
	}
	if c.lost() {
		return sessionLostError(c)
	}
	return nil
}

func (s *Source) sendPing(c *captureSession) error {
	if err := s.wl.Request(wlturbo.Request{Proxy: c, Opcode: reqSessionPing, Name: "neferwl_capture_session_v1.ping"}); err != nil {
		s.terminate()
		return fmt.Errorf("wayland: session ping: %w", err)
	}
	c.lastPing = time.Now()
	return nil
}

func sessionLostError(c *captureSession) error {
	if c.violation {
		return fmt.Errorf("%w: invalid session state from the compositor", ErrSessionUnavailable)
	}
	return &SessionStoppedError{Reason: StopReason(c.stopReason)}
}

// captureTarget is Capture under an active session: it resolves workspace
// targets, sends a keep-alive when one is due (a single request, no wait), and
// never returns a frame captured while the exclusion was lost. The caller
// holds opMu.
func (s *Source) captureTarget(t ports.Target) (ports.Frame, error) {
	c := s.session
	if c != nil && c.lost() {
		return ports.Frame{}, sessionLostError(c)
	}
	if c != nil && !c.active {
		// Events are only read inside operations: look once for a resume.
		if err := s.roundtrip(); err != nil {
			return ports.Frame{}, err
		}
		if c.lost() {
			return ports.Frame{}, sessionLostError(c)
		}
		if !c.active {
			return ports.Frame{}, ErrSessionInactive
		}
	}
	if c != nil && time.Since(c.lastPing) >= s.pingEvery {
		if err := s.sendPing(c); err != nil {
			return ports.Frame{}, err
		}
	}
	// Events are only read while a request is in flight, so a live geometry
	// change is seen during the capture that used the previous region. Such a
	// frame is stale for a workspace target: it is never returned. Capture
	// again, a bounded number of times and without any extra wait, then give up
	// with a transient error.
	retriedFailure := false
	for attempt := 0; ; attempt++ {
		rt, err := s.resolveCapture(t)
		if err != nil {
			return ports.Frame{}, err
		}
		used := rt.Region
		frame, err := s.capture(rt)
		if c != nil && errors.Is(err, ports.ErrOutputNotFound) && !c.lost() {
			// An output that went away ends the session: read that stop.
			if rerr := s.roundtrip(); rerr != nil {
				return ports.Frame{}, rerr
			}
		}
		if c != nil && c.lost() { // a stop was dispatched during the capture
			return ports.Frame{}, sessionLostError(c)
		}
		if errors.Is(err, errCompositorFailedCapture) && c != nil && c.active && !retriedFailure {
			// The compositor's scene may lag a session change by one frame and
			// fail the copy: retry once, never blindly beyond that.
			retriedFailure = true
			attempt--
			continue
		}
		if err != nil {
			return ports.Frame{}, err
		}
		if c != nil && !c.active { // paused meanwhile: the frame is not clean
			return ports.Frame{}, ErrSessionInactive
		}
		if t.WorkspaceID == 0 || c == nil || c.region == used {
			return frame, nil
		}
		if attempt >= maxRegionRetries {
			return ports.Frame{}, ErrSessionGeometryChanging
		}
	}
}

// resolveCapture maps a workspace target to the session's pinned output and
// the live region the compositor last granted. The workspace inventory is not
// consulted: it can change or lose the workspace at any time, and the session
// alone decides what is captured.
func (s *Source) resolveCapture(t ports.Target) (ports.Target, error) {
	if t.WorkspaceID == 0 {
		return t, nil
	}
	c := s.session
	if c == nil || c.requestedWorkspace != t.WorkspaceID {
		return t, fmt.Errorf("%w: workspace capture needs a session on that workspace", ErrSessionUnsupported)
	}
	if t.Region != (ports.Region{}) {
		return t, fmt.Errorf("%w: a workspace target has no region", ports.ErrInvalidRegion)
	}
	if t.OutputID != 0 && t.OutputID != c.outputID {
		return t, ports.ErrOutputNotFound
	}
	return ports.Target{OutputID: c.outputID, Region: c.region}, nil
}

// AuthorizeLayer tells the compositor, from a connection other than the
// capture connection, that surface is a layer surface of the capture peer and
// must be excluded from its session. token comes from SessionState. Call it
// after the layer-shell role is assigned and before the surface is mapped
// (before the first buffer); when it returns nil the compositor core has
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
// After it returns nil the attachment stays until the session stops or the
// layer is destroyed; then onDetached (may be nil) receives a
// *LayerDetachedError, once. A violation of the attachment protocol at that
// point closes display and calls onDetached with an error matching
// ErrSessionUnavailable; the surface may still draw until the caller stops,
// so onDetached must make the caller stop. It runs on the goroutine that reads
// display, so it must only do thread-safe, non-blocking work such as
// cancelling a context. It never sees the token. The attachment object is
// never destroyed after a success: that would not release the exclusion, and
// closing the connection ends it. After DetachSessionEnded the compositor has
// closed the layer surface, which the caller destroys.
func AuthorizeLayer(ctx context.Context, display *wlturbo.Display, surface *core.Surface, token string, onDetached func(error)) error {
	return authorizeLayer(ctx, display, surface, token, onDetached, DefaultAttachTimeout)
}

func authorizeLayer(ctx context.Context, display *wlturbo.Display, surface *core.Surface, token string, onDetached func(error), timeout time.Duration) error {
	if display == nil || surface == nil || surface.Context() != display.Context() {
		return errors.New("wayland: authorize layer: surface does not belong to display")
	}
	if !validToken(token) {
		return errors.New("wayland: authorize layer: invalid token")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m := &captureManager{}
	m.SetContext(display.Context())
	if _, err := display.Registry().BindNegotiated(captureManagerIface, captureManagerVersion, m); err != nil {
		if errors.Is(err, wlturbo.ErrGlobalNotFound) {
			return ErrSessionUnsupported
		}
		return fmt.Errorf("wayland: bind %s: %w", captureManagerIface, err)
	}
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stop := context.AfterFunc(wctx, func() { _ = display.Close() })
	defer stop()

	l := &layerProxy{display: display, onDetached: onDetached}
	l.SetContext(display.Context())
	err := display.Context().Request(wlturbo.Request{Proxy: m, Opcode: reqAttachSurface, Name: "neferwl_capture_manager_v1.attach_surface", Child: l}, l, token, surface)
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
	_ = display.Context().Request(wlturbo.Request{Proxy: m, Opcode: reqManagerDestroy, Name: "neferwl_capture_manager_v1.destroy", Destructor: true})
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
