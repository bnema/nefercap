// Package wayland captures frames over ext-image-copy-capture-v1 with the
// wlturbo transport and wl_shm buffers. It lists workspaces through
// ext-workspace-v1 and uses NeferWL's optional extension, when the compositor
// has it, for workspace and region sources and for excluding the recording
// HUD from frames.
package wayland

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bnema/wlturbo"
	"github.com/bnema/wlturbo/protocol/core"
	"github.com/bnema/zerowrap"

	"github.com/bnema/nefercap/internal/logging"
	"github.com/bnema/nefercap/internal/ports"
)

const (
	outputVersion    = 4 // wl_output.name arrived in version 4
	shmVersion       = 1
	copyVersion      = 1
	xdgOutputVersion = 3
	maxSettleRounds  = 4
	maxOutputScale   = 16 // informational; larger announcements are clamped
	// defaultPollWait is how long Capture waits for a frame the compositor may
	// hold back until the screen changes; short against a frame interval.
	defaultPollWait = 2 * time.Millisecond
)

// Globals of the capture protocols.
const (
	outputSourceIface = "ext_output_image_capture_source_manager_v1"
	copyManagerIface  = "ext_image_copy_capture_manager_v1"
	xdgOutputIface    = "zxdg_output_manager_v1"
)

// output is the client-side state of one bound wl_output global.
type output struct {
	proxy         *core.Output
	id            uint32 // wl_registry global name, stable for the connection
	version       uint32
	name          string
	width, height int32
	scale         int32
	lw, lh        int32 // logical size from xdg-output, 0 when unknown
	xdg           *xdgOutput
	removed       bool
}

// Source is a ports.Source backed by one Wayland connection.
//
// All protocol events are dispatched synchronously by the goroutine that runs
// Outputs or Capture, so event handlers need no locking. Calls are serialized
// by opMu. Cancelling the context of a running call, a protocol failure or
// Close closes the connection: the Source is then terminal and every later
// call returns ports.ErrClosed. The call that observed a cancellation returns
// its context's error. A context cancelled between calls affects only a later
// call that is given that same context.
type Source struct {
	log     zerowrap.Logger
	display *wlturbo.Display
	wl      *wlturbo.Context

	opMu sync.Mutex // serializes operations and guards everything below
	dead atomic.Bool

	// Cancellation watcher, reused while consecutive calls share a Done
	// channel. watchDone and watchStop are guarded by opMu; the callback only
	// reads the atomics, and acts only while a call is active and its
	// registration is still the current generation.
	watchDone   <-chan struct{}
	watchStop   func() bool
	watchGen    atomic.Uint64
	watchActive atomic.Bool

	shm         *core.Shm
	copyManager *requestOnly // ext_image_copy_capture_manager_v1
	outSource   *requestOnly // ext_output_image_capture_source_manager_v1
	xdgOutputs  *requestOnly // zxdg_output_manager_v1, nil when absent
	outputs     map[uint32]*output
	byProxy     map[uint32]*output // wl_output proxy ID to output
	bound       int                // wl_output globals bound so far
	bufs        [2]shmBuffer
	conn        net.Conn
	pollWait    time.Duration // how long Capture waits for a pending frame

	sess *session // the one live capture session, nil before the first capture
	ext  extensions
	ws   workspaceState
}

var _ ports.Source = (*Source)(nil)

// New connects to socket (an absolute path, or a name relative to
// XDG_RUNTIME_DIR; empty selects WAYLAND_DISPLAY) and discovers outputs.
//
// ctx bounds the constructor only: it interrupts discovery, and is detached
// once New returns, so cancelling it afterwards does not close the Source.
// Later calls are bound by their own contexts. The logger is derived from ctx.
func New(ctx context.Context, socket string) (*Source, error) {
	path, err := socketPath(socket)
	if err != nil {
		return nil, err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("wayland: connect: %w", err)
	}
	display, err := wlturbo.ConnectFromConn(conn)
	if err != nil {
		return nil, fmt.Errorf("wayland: %w", err)
	}
	s := &Source{
		log:      logging.For(ctx, "wayland"),
		display:  display,
		wl:       display.Context(),
		outputs:  make(map[uint32]*output),
		byProxy:  make(map[uint32]*output),
		conn:     conn,
		pollWait: defaultPollWait,
	}
	stop := context.AfterFunc(ctx, s.terminate)
	err = s.discover()
	cancelled := !stop()
	if cancelled || err != nil {
		s.terminate()
		if cancelled || ctx.Err() != nil {
			err = ctx.Err()
		}
		s.log.Error().Err(err).Msg("wayland source failed to start")
		return nil, err
	}
	s.log.Info().Int("outputs", len(s.outputs)).Interface("capabilities", s.Capabilities()).
		Bool("region_source", s.ext.source != nil).
		Bool("hidden_workspaces", s.ws.mgr != nil && s.ext.source != nil).
		Msg("wayland source started")
	return s, nil
}

func socketPath(socket string) (string, error) {
	if socket == "" {
		socket = os.Getenv("WAYLAND_DISPLAY")
		if socket == "" {
			socket = "wayland-0"
		}
	}
	if filepath.IsAbs(socket) {
		return socket, nil
	}
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		return "", errors.New("wayland: XDG_RUNTIME_DIR is not set")
	}
	return filepath.Join(dir, socket), nil
}

func (s *Source) discover() (err error) {
	reg := s.display.Registry()
	reg.AddHandler(core.OutputInterface, s.onOutputGlobal)
	reg.AddGlobalRemoveHandler(globalRemoved{s})
	if err = s.roundtrip(); err != nil {
		return err
	}
	s.shm = core.NewShm(s.wl)
	if _, err = reg.BindNegotiated(core.ShmInterface, shmVersion, s.shm); err != nil {
		return fmt.Errorf("wayland: bind wl_shm: %w", err)
	}
	if s.copyManager, err = s.bindRequired(copyManagerIface, copyVersion); err != nil {
		return err
	}
	if s.outSource, err = s.bindRequired(outputSourceIface, copyVersion); err != nil {
		return err
	}
	if err = s.bindExtras(); err != nil {
		return err
	}
	return s.settle()
}

// bindRequired binds a global that capture cannot work without.
func (s *Source) bindRequired(iface string, version uint32) (*requestOnly, error) {
	p := newRequestOnly(s.wl)
	if _, err := s.display.Registry().BindNegotiated(iface, version, p); err != nil {
		if errors.Is(err, wlturbo.ErrGlobalNotFound) {
			return nil, fmt.Errorf("wayland: the compositor does not support ext-image-copy-capture-v1 (%s missing)", iface)
		}
		return nil, fmt.Errorf("wayland: bind %s: %w", iface, err)
	}
	return p, nil
}

// bindOptional binds a global when the compositor offers it.
func (s *Source) bindOptional(iface string, version uint32) (*requestOnly, error) {
	p := newRequestOnly(s.wl)
	_, err := s.display.Registry().BindNegotiated(iface, version, p)
	switch {
	case err == nil:
		return p, nil
	case errors.Is(err, wlturbo.ErrGlobalNotFound):
		return nil, nil
	}
	return nil, fmt.Errorf("wayland: bind %s: %w", iface, err)
}

func (s *Source) onOutputGlobal(reg *wlturbo.Registry, name, version uint32) {
	if version == 0 || s.outputs[name] != nil {
		return // unusable, or a duplicate announcement of a bound global
	}
	o := &output{id: name, scale: 1, version: min(version, outputVersion), proxy: core.NewOutput(s.wl)}
	o.proxy.OnMode(func(flags uint32, w, h, _ int32) {
		// The mode is informational: implausible values are ignored.
		if flags&1 != 0 && w >= 0 && h >= 0 && w <= ports.MaxDimension && h <= ports.MaxDimension {
			o.width, o.height = w, h
		}
	})
	o.proxy.OnScale(func(f int32) { o.scale = min(max(f, 1), maxOutputScale) })
	o.proxy.OnName(func(n string) { o.name = n })
	if err := reg.Bind(name, core.OutputInterface, o.version, o.proxy); err != nil {
		s.log.Warn().Err(err).Uint32("id", name).Msg("bind wl_output failed")
		return
	}
	s.outputs[name] = o
	s.byProxy[o.proxy.ID()] = o
	s.bound++
	if s.xdgOutputs != nil {
		s.watchLogicalSize(o)
	}
}

type globalRemoved struct{ s *Source }

func (g globalRemoved) HandleRegistryGlobalRemove(e wlturbo.RegistryGlobalRemoveEvent) {
	o := g.s.outputs[e.Name]
	if o == nil {
		return
	}
	delete(g.s.outputs, e.Name)
	delete(g.s.byProxy, o.proxy.ID())
	o.removed = true
	g.s.releaseXdg(o)
	g.s.ws.forgetOutput(o.proxy.ID())
	if o.version >= 3 {
		_ = o.proxy.Release()
	} // older wl_output has no destructor: the proxy must stay to absorb events
}

// terminate closes the connection, which unblocks any Dispatch. It is safe
// from any goroutine and idempotent.
func (s *Source) terminate() {
	s.dead.Store(true)
	_ = s.display.Close()
}

func (s *Source) dispatch() error {
	err := s.display.Dispatch()
	if err != nil {
		s.terminate()
		return fmt.Errorf("wayland: dispatch: %w", err)
	}
	return nil
}

// dispatchUntil is dispatch with a read deadline; a zero deadline waits
// forever. It reports a missed deadline, which is not an error.
func (s *Source) dispatchUntil(deadline time.Time) (timedOut bool, err error) {
	if deadline.IsZero() {
		return false, s.dispatch()
	}
	_ = s.conn.SetReadDeadline(deadline)
	err = s.display.Dispatch()
	_ = s.conn.SetReadDeadline(time.Time{})
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true, nil
	}
	if err != nil {
		s.terminate()
		return false, fmt.Errorf("wayland: dispatch: %w", err)
	}
	return false, nil
}

// settle round-trips until no output was bound meanwhile, so that the events
// of an output announced during the last round trip have been received too.
func (s *Source) settle() error {
	for range maxSettleRounds {
		before := s.bound
		if err := s.roundtrip(); err != nil || s.bound == before {
			return err
		}
	}
	return nil
}

func (s *Source) roundtrip() error {
	err := s.display.Roundtrip()
	if err != nil {
		s.terminate()
		return fmt.Errorf("wayland: roundtrip: %w", err)
	}
	return nil
}

// begin starts one serialized operation bound by ctx. The caller must hold
// opMu and must call finish afterwards. Consecutive calls with the same Done
// channel share one context.AfterFunc registration; contexts that can never be
// cancelled register nothing.
func (s *Source) begin(ctx context.Context) error {
	if s.dead.Load() {
		return ports.ErrClosed
	}
	if done := ctx.Done(); done != nil {
		if done != s.watchDone {
			s.detachWatcher()
			gen := s.watchGen.Add(1)
			s.watchDone = done
			s.watchStop = context.AfterFunc(ctx, func() { s.watchFired(gen) })
		}
		// Activate before checking the context: a callback that fired while
		// idle did nothing, and this check then observes the cancellation.
		s.watchActive.Store(true)
	}
	if err := ctx.Err(); err != nil {
		s.watchActive.Store(false)
		s.terminate()
		s.detachWatcher()
		s.releaseBuffers()
		return err
	}
	return nil
}

// watchFired runs on the context's goroutine. It ends the connection only
// when it belongs to the running call's registration.
func (s *Source) watchFired(gen uint64) {
	if s.watchActive.Load() && s.watchGen.Load() == gen {
		s.terminate()
	}
}

// detachWatcher drops the cached registration. The caller holds opMu.
func (s *Source) detachWatcher() {
	if s.watchStop != nil {
		s.watchStop()
	}
	s.watchGen.Add(1) // a callback already running is now stale
	s.watchStop, s.watchDone = nil, nil
}

// finish ends the call started by begin and classifies err. A cancellation
// observed at any point makes the Source terminal. When it is terminal, the
// mapped storage is released before returning.
//
// Ordering invariant: a context sets Err before it closes Done and runs
// AfterFunc callbacks. So a callback that ran while the call was active has
// already made Err visible, and a callback that ran while idle did nothing but
// is caught by the Err check. Clearing active before that check therefore
// cannot report success for a call whose context was cancelled in between.
func (s *Source) finish(ctx context.Context, err error) error {
	s.watchActive.Store(false)
	if cerr := ctx.Err(); cerr != nil {
		s.terminate()
	}
	if !s.dead.Load() {
		return err
	}
	s.detachWatcher()
	s.releaseBuffers()
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	if err == nil || errors.Is(err, net.ErrClosed) {
		return ports.ErrClosed
	}
	s.log.Error().Err(err).Msg("wayland source terminated")
	return err
}

// Outputs lists the outputs currently announced by the compositor, by ID.
func (s *Source) Outputs(ctx context.Context) ([]ports.Output, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if err := s.begin(ctx); err != nil {
		return nil, err
	}
	err := s.settle()
	var list []ports.Output
	if err == nil {
		list = make([]ports.Output, 0, len(s.outputs))
		for _, o := range s.outputs {
			list = append(list, ports.Output{ID: o.id, Name: outputName(o), Width: int(o.width), Height: int(o.height), Scale: int(o.scale)})
		}
		slices.SortFunc(list, func(a, b ports.Output) int { return cmp.Compare(a.ID, b.ID) })
	}
	if err = s.finish(ctx, err); err != nil {
		return nil, err
	}
	return list, nil
}

// Capture copies one frame of the target into storage reused across calls.
// The frame is valid until the next Capture or Close.
func (s *Source) Capture(ctx context.Context, t ports.Target) (ports.Frame, error) {
	if err := validRegion(t.Region); err != nil {
		return ports.Frame{}, err
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if err := s.begin(ctx); err != nil {
		return ports.Frame{}, err
	}
	frame, err := s.captureTarget(t)
	if err = s.finish(ctx, err); err != nil {
		return ports.Frame{}, err
	}
	return frame, nil
}

// Capabilities reports what the compositor offers beyond output capture.
func (s *Source) Capabilities() ports.Capabilities {
	return ports.Capabilities{
		Workspaces: s.ws.mgr != nil,
		Exclusion:  s.ext.exclusion != nil,
	}
}

// Close terminates the connection, waits for a blocked operation to observe
// it, and releases the mapped storage. It is idempotent.
func (s *Source) Close() error {
	s.terminate()
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.detachWatcher()
	return s.releaseBuffers()
}
