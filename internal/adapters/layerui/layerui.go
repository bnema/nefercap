// Package layerui shows one NeferGUI view on a layer surface: neferclient owns
// the Wayland connection and surface, nefergui.Renderer draws into DMA-BUF
// buffers, and this package copies plain fields between the two. Each Run has
// its own connection and runs everything on the calling goroutine.
package layerui

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bnema/neferclient"
	"github.com/bnema/nefergui"
	"github.com/bnema/wlturbo"
)

// guiMods is the set of modifiers NeferGUI knows. Both libraries use the same
// bit order, so the plain bits can be copied.
const guiMods = neferclient.ModShift | neferclient.ModCtrl | neferclient.ModAlt | neferclient.ModSuper |
	neferclient.ModCapsLock | neferclient.ModNumLock

// pendingRetry is how soon a frame that waits for the GPU is tried again.
const pendingRetry = 2 * time.Millisecond

// Config describes one layer surface and the hooks of its owner loop. Every
// hook but OnSurface runs on the goroutine that called Run.
type Config struct {
	Layer  neferclient.LayerConfig
	Styles string // CSS file path for nefergui.RendererConfig.Styles

	// Wake redraws the view for each value received.
	Wake <-chan struct{}
	// OnResize reports the logical size and scale at the first configure and
	// on every change.
	OnResize func(width, height int, scale float64)
	// OnInput sees every input event before NeferGUI routes it to controls;
	// returning true redraws the view.
	OnInput func(nefergui.Input) bool
	// OnSurface runs once, after the layer is configured and before its first
	// buffer is shown, with the connection's display and the layer's
	// wl_surface. An error ends Run and nothing is shown. It runs on its own
	// goroutine while the owner loop keeps applying events, so it may wait for
	// the compositor's answer to a request of its own; it must only send
	// requests, and return when ctx ends: Run waits for it.
	//
	// The input region starts as Layer.InputRects; a view's
	// Frame.SetInputRects replaces it.
	OnSurface func(ctx context.Context, display *wlturbo.Display, surface wlturbo.Proxy) error
}

// Run shows view on a transparent layer surface until ctx ends (it returns
// ctx.Err()), the compositor closes the surface (nil) or something fails.
func Run[T any](ctx context.Context, cfg Config, model *T, view func(*nefergui.Frame, *T)) (err error) {
	conn, err := neferclient.Connect(ctx, "") // WAYLAND_DISPLAY
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	l := &loop[T]{ctx: ctx, cfg: cfg, conn: conn, model: model, view: view, cursor: neferclient.CursorDefault}
	// Close the connection first (it destroys every Wayland object), then
	// free the renderer and its GPU resources.
	defer func() {
		err = errors.Join(err, conn.Close())
		if l.r != nil {
			err = errors.Join(err, l.r.Close())
		}
	}()
	if l.surf, err = conn.NewLayerSurface(cfg.Layer); err != nil {
		return fmt.Errorf("layer surface: %w", err)
	}
	l.seat = conn.Seat()
	defer l.waitHook()
	return l.run()
}

type loop[T any] struct {
	neferclient.NopHandler

	ctx   context.Context
	cfg   Config
	conn  *neferclient.Conn
	surf  *neferclient.Surface
	seat  *neferclient.Seat
	model *T
	view  func(*nefergui.Frame, *T)

	r      *nefergui.Renderer
	rwake  <-chan struct{} // Renderer.Wake: nil until the renderer exists
	out    nefergui.Output
	damage []neferclient.Rect
	region []neferclient.Rect // reused by setInputRegion

	w, h        int32
	scale       float64
	configured  bool
	canPresent  bool // configured, and the last frame callback fired
	authorized  bool // OnSurface succeeded, or there is none
	hookStarted bool
	hookDone    chan error // OnSurface's result while it is pending
	hookCancel  context.CancelFunc
	haveAcquire bool
	cursor      neferclient.CursorShape

	closed bool
	err    error
}

func (l *loop[T]) run() error {
	l.authorized = l.cfg.OnSurface == nil
	retry := time.NewTimer(pendingRetry) // reused: Pending can last many frames
	retry.Stop()
	defer retry.Stop()
	var tick <-chan time.Time
	for !l.closed {
		select {
		case <-l.ctx.Done():
			return l.ctx.Err()
		case <-l.conn.Wake():
			if err := l.conn.Dispatch(l); err != nil {
				return fmt.Errorf("dispatch: %w", err)
			}
		case _, ok := <-l.cfg.Wake:
			if !ok {
				l.cfg.Wake = nil // a closed channel would spin
			} else if l.r != nil {
				l.r.Invalidate()
			}
		case err := <-l.hookDone:
			l.hookDone = nil
			if err != nil {
				return err
			}
			l.authorized = true
		case <-l.rwake: // another goroutine asked for a frame
		case <-tick: // a built frame was waiting for the GPU
		}
		if l.err != nil {
			return l.err
		}
		if l.closed {
			return nil
		}
		if err := l.draw(); err != nil {
			return err
		}
		tick = nil
		if l.r != nil && l.r.Pending() {
			retry.Reset(pendingRetry)
			tick = retry.C
		}
	}
	return nil
}

// FeedbackDone: dmabuf feedback → nefergui.RendererConfig. Only the first
// complete feedback is used: one GPU is assumed.
func (l *loop[T]) FeedbackDone(neferclient.SurfaceID) { l.setup() }

func (l *loop[T]) Configure(neferclient.SurfaceID, int32, int32) {
	if !l.configured { // later configures must not lift the frame-callback gate
		l.canPresent = true
	}
	l.configured = true
	l.resize()
	l.setup()
}

func (l *loop[T]) Scale(neferclient.SurfaceID, float64) { l.resize() }

func (l *loop[T]) setup() {
	fb := l.surf.Feedback()
	if l.r != nil || !l.configured || fb == nil {
		return
	}
	cfg := nefergui.RendererConfig{MainDevice: fb.MainDevice, Transparent: true, Styles: l.cfg.Styles,
		Formats: make([]nefergui.Format, len(fb.Formats))}
	for i, f := range fb.Formats {
		cfg.Formats[i] = nefergui.Format{FourCC: f.FourCC, Modifier: f.Modifier}
	}
	r, err := nefergui.NewRenderer(cfg)
	if err != nil {
		l.fail(fmt.Errorf("renderer: %w", err))
		return
	}
	l.r, l.rwake = r, r.Wake()
	w, h, scale := l.surf.Size()
	r.Resize(int(w), int(h), scale)
}

// resize forwards a changed logical size or scale to OnResize and the
// renderer. Before the first configure the size is not known yet.
func (l *loop[T]) resize() {
	if !l.configured {
		return
	}
	w, h, scale := l.surf.Size()
	if w == l.w && h == l.h && scale == l.scale {
		return
	}
	l.w, l.h, l.scale = w, h, scale
	if l.cfg.OnResize != nil {
		l.cfg.OnResize(int(w), int(h), scale)
	}
	if l.r != nil {
		l.r.Resize(int(w), int(h), scale)
	}
}

// Frame: the compositor showed the last commit; the next Present is allowed.
func (l *loop[T]) Frame(neferclient.SurfaceID) { l.canPresent = true }

func (l *loop[T]) Closed(neferclient.SurfaceID) { l.closed = true }

func (l *loop[T]) Error(err error) { l.fail(err) }

func (l *loop[T]) fail(err error) {
	if l.err == nil {
		l.err = err
	}
}

// FDReady: a buffer's release eventfd is readable (id is the buffer).
func (l *loop[T]) FDReady(id uint64) {
	if err := l.r.Released(id); err != nil {
		l.fail(fmt.Errorf("release buffer %d: %w", id, err))
	}
}

func (l *loop[T]) Pointer(ev *neferclient.PointerEvent) {
	in, ok := pointerInput(ev)
	if !ok {
		return
	}
	if ev.Kind == neferclient.PointerEnter {
		// The cursor shape is only valid after the enter: set it again.
		if err := l.seat.SetCursor(l.cursor); err != nil {
			l.fail(fmt.Errorf("cursor: %w", err))
		}
	}
	l.input(&in)
}

func (l *loop[T]) Key(ev *neferclient.KeyEvent) {
	in := keyInput(ev)
	l.input(&in)
}

func (l *loop[T]) KeyboardFocus(_ neferclient.SurfaceID, focused bool) {
	in := nefergui.Input{Kind: nefergui.InputFocusOut}
	if focused {
		in.Kind = nefergui.InputFocusIn
	}
	l.input(&in)
}

// input gives ev to OnInput, then to NeferGUI. Before the renderer exists
// nothing is shown, so nothing is delivered.
func (l *loop[T]) input(ev *nefergui.Input) {
	if l.r == nil {
		return
	}
	if l.cfg.OnInput != nil && l.cfg.OnInput(*ev) {
		l.r.Invalidate()
	}
	l.r.Input(ev)
}

// pointerInput converts a neferclient pointer event; ok is false for events
// NeferGUI does not take.
func pointerInput(ev *neferclient.PointerEvent) (in nefergui.Input, ok bool) {
	in = nefergui.Input{X: ev.X, Y: ev.Y}
	switch ev.Kind {
	case neferclient.PointerEnter, neferclient.PointerMotion:
		in.Kind = nefergui.InputPointerMotion
	case neferclient.PointerLeave:
		in.Kind = nefergui.InputPointerLeave
	case neferclient.PointerButton:
		in.Kind, in.Button, in.Pressed = nefergui.InputPointerRelease, ev.Button, ev.Pressed
		if ev.Pressed {
			in.Kind = nefergui.InputPointerPress
		}
	case neferclient.PointerAxis:
		in.Kind, in.DX, in.DY = nefergui.InputPointerAxis, ev.DX, ev.DY
	default:
		return nefergui.Input{}, false
	}
	return in, true
}

// keyInput converts a neferclient key event. Text aliases neferclient storage
// that is valid only during the Key call, as NeferGUI requires.
func keyInput(ev *neferclient.KeyEvent) nefergui.Input {
	return nefergui.Input{
		Kind:      nefergui.InputKey,
		Keysym:    ev.Keysym,
		Text:      ev.Text,
		Pressed:   ev.Pressed,
		Repeat:    ev.Repeat,
		Modifiers: nefergui.Modifiers(ev.Modifiers & guiMods),
	}
}

// draw renders a frame when something changed and presents it.
func (l *loop[T]) draw() error {
	if l.r == nil || !l.canPresent {
		return nil
	}
	if !l.authorized {
		l.startHook()
		return nil
	}
	ok, err := l.r.Render(&l.out, l.model, l.view)
	if err != nil {
		return fmt.Errorf("render: %w", err)
	}
	if !ok {
		return nil
	}
	if err = l.setInputRegion(); err != nil {
		return err
	}
	if err = l.present(); err != nil {
		return err
	}
	return l.setCursor()
}

// setInputRegion: Output.InputRects → Surface.SetInputRegion, only when the
// view changed it. Both are logical surface pixels; nil means the whole
// surface and an empty slice click-through in both libraries.
func (l *loop[T]) setInputRegion() error {
	region, changed := inputRegion(l.region[:0], &l.out)
	if !changed {
		return nil
	}
	if region != nil {
		l.region = region
	}
	if err := l.surf.SetInputRegion(region); err != nil {
		return fmt.Errorf("input region: %w", err)
	}
	return nil
}

// inputRegion converts out's input region into dst; changed is false when
// the view did not change it.
func inputRegion(dst []neferclient.Rect, out *nefergui.Output) (region []neferclient.Rect, changed bool) {
	if !out.InputRectsChanged {
		return nil, false
	}
	if out.InputRects == nil {
		return nil, true
	}
	if dst == nil {
		dst = []neferclient.Rect{}
	}
	for _, r := range out.InputRects {
		dst = append(dst, neferclient.Rect{X: r.X, Y: r.Y, Width: r.Width, Height: r.Height})
	}
	return dst, true
}

// startHook starts OnSurface once. The owner loop keeps dispatching while it
// waits: neferclient's reader stops reading when the event queue is full.
func (l *loop[T]) startHook() {
	if l.hookStarted {
		return
	}
	l.hookStarted = true
	ctx, cancel := context.WithCancel(l.ctx)
	done := make(chan error, 1)
	l.hookDone, l.hookCancel = done, cancel
	display, surface := l.conn.Display(), l.surf.WLSurface()
	go func() { done <- l.cfg.OnSurface(ctx, display, surface) }()
}

// waitHook ends a pending OnSurface and waits for it before the connection
// closes.
func (l *loop[T]) waitHook() {
	if l.hookCancel == nil {
		return
	}
	l.hookCancel()
	if l.hookDone != nil {
		<-l.hookDone
	}
}

// present: Renderer.Render output → ImportBuffer, ImportTimeline, Present.
func (l *loop[T]) present() error {
	out := &l.out
	// A resize retired buffers: stop watching and destroy them first.
	for _, rt := range out.Retired {
		if err := l.conn.UnwatchFD(rt.ReleaseFD); err != nil {
			return fmt.Errorf("unwatch retired buffer %d: %w", rt.Buffer, err)
		}
		if err := l.surf.DestroyBuffer(rt.Buffer); err != nil {
			return fmt.Errorf("destroy retired buffer %d: %w", rt.Buffer, err)
		}
		// The release timeline of a buffer is imported under the buffer's id.
		if err := l.surf.DestroyTimeline(rt.Buffer); err != nil {
			return fmt.Errorf("destroy retired timeline %d: %w", rt.Buffer, err)
		}
	}
	if out.NewBuffer {
		buf := neferclient.Buffer{
			Width: out.Width, Height: out.Height, FourCC: out.FourCC, Modifier: out.Modifier,
			PlaneCount: out.PlaneCount,
		}
		for i, p := range out.Planes {
			buf.Planes[i] = neferclient.Plane{FD: p.FD, Offset: p.Offset, Stride: p.Stride}
		}
		if err := l.surf.ImportBuffer(out.Buffer, &buf); err != nil {
			return fmt.Errorf("import buffer: %w", err)
		}
		// The release eventfd is stable per buffer: watch it once.
		if err := l.conn.WatchFD(out.ReleaseFD, out.Buffer); err != nil {
			return fmt.Errorf("watch release fd: %w", err)
		}
	}
	if out.NewTimelines {
		if !l.haveAcquire { // the acquire timeline is shared by every buffer
			if err := l.surf.ImportTimeline(out.Acquire.ID, out.Acquire.FD); err != nil {
				return fmt.Errorf("import acquire timeline: %w", err)
			}
			l.haveAcquire = true
		}
		if err := l.surf.ImportTimeline(out.Release.ID, out.Release.FD); err != nil {
			return fmt.Errorf("import release timeline: %w", err)
		}
	}
	l.damage = l.damage[:0]
	for _, d := range out.Damage {
		l.damage = append(l.damage, neferclient.Rect{X: d.X, Y: d.Y, Width: d.Width, Height: d.Height})
	}
	err := l.surf.Present(&neferclient.Present{
		Buffer:          out.Buffer,
		AcquireTimeline: out.Acquire.ID,
		ReleaseTimeline: out.Release.ID,
		AcquirePoint:    out.AcquirePoint,
		ReleasePoint:    out.ReleasePoint,
		Damage:          l.damage,
	})
	if err != nil {
		return fmt.Errorf("present: %w", err)
	}
	l.canPresent = false // until Frame
	return nil
}

// setCursor: the cursor the hovered element asked for → Seat.SetCursor.
func (l *loop[T]) setCursor() error {
	switch l.out.Cursor {
	case nefergui.CursorPointer:
		l.cursor = neferclient.CursorPointer
	case nefergui.CursorText:
		l.cursor = neferclient.CursorText
	case nefergui.CursorNotAllowed:
		l.cursor = neferclient.CursorNotAllowed
	default:
		l.cursor = neferclient.CursorDefault
	}
	if err := l.seat.SetCursor(l.cursor); err != nil {
		return fmt.Errorf("cursor: %w", err)
	}
	return nil
}
