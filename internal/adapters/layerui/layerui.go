// Package layerui shows one NeferGUI view on a layer surface: neferclient owns
// the Wayland connection and surface, and neferclient/gui hosts the
// nefergui.Renderer on it (input, present, cursor, input region, buffer
// release and frame gating). Each Run has its own connection and runs
// everything but OnSurface on the calling goroutine.
package layerui

import (
	"context"
	"errors"
	"fmt"

	"bnema.dev/neferclient"
	"bnema.dev/neferclient/gui"
	"bnema.dev/nefergui"
	"bnema.dev/wlturbo"
)

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
	l := &loop[T]{ctx: ctx, cfg: cfg, conn: conn}
	// Close the connection first (it destroys every Wayland object and its
	// descriptor watches), then free the renderer and its GPU resources.
	defer func() {
		err = errors.Join(err, conn.Close())
		if l.s != nil {
			err = errors.Join(err, l.s.CloseAfterConn())
		}
	}()
	if l.surf, err = conn.NewLayerSurface(cfg.Layer); err != nil {
		return fmt.Errorf("layer surface: %w", err)
	}
	gcfg := gui.Config{
		Renderer:    nefergui.RendererConfig{Transparent: true, Styles: cfg.Styles},
		InputRegion: true,
	}
	if cfg.OnInput != nil {
		gcfg.OnInput = func(in *nefergui.Input) bool { return cfg.OnInput(*in) }
	}
	l.s = gui.NewSurface(conn, l.surf, gcfg, model, view)
	defer l.waitHook()
	return l.run()
}

// loop is the app handler behind the gui.Router: it gets every event after
// the hosted surface, and the ones no surface takes.
type loop[T any] struct {
	neferclient.NopHandler

	ctx  context.Context
	cfg  Config
	conn *neferclient.Conn
	surf *neferclient.Surface
	s    *gui.Surface[T]

	w, h        int32
	scale       float64
	configured  bool
	authorized  bool // OnSurface succeeded, or there is none
	hookStarted bool
	hookDone    chan error // OnSurface's result while it is pending
	hookCancel  context.CancelFunc

	closed bool
	err    error
}

func (l *loop[T]) run() error {
	l.authorized = l.cfg.OnSurface == nil
	router := gui.NewRouter(l)
	router.Add(l.s)
	for !l.closed {
		select {
		case <-l.ctx.Done():
			return l.ctx.Err()
		case <-l.conn.Wake():
			if err := l.conn.Dispatch(router); err != nil {
				return fmt.Errorf("dispatch: %w", err)
			}
		case _, ok := <-l.cfg.Wake:
			if !ok {
				l.cfg.Wake = nil // a closed channel would spin
			} else {
				l.s.Invalidate()
			}
		case err := <-l.hookDone:
			l.hookDone = nil
			if err != nil {
				return err
			}
			l.authorized = true
		case <-l.s.Wake(): // another goroutine asked for a frame
		case <-l.s.Timer(): // a frame waits for the GPU or an animation
		}
		if l.err != nil {
			return l.err
		}
		if l.closed {
			return nil
		}
		// A stored surface error (a failed renderer setup, say) is fatal even
		// while Draw is held back by the hook gate.
		if err := l.s.Err(); err != nil {
			return err
		}
		if !l.authorized {
			// Nothing is shown before OnSurface succeeds. The renderer exists
			// once the layer is configured and its feedback is known.
			if l.s.Renderer() != nil {
				l.startHook()
			}
			continue
		}
		if err := l.s.Draw(); err != nil {
			return err
		}
	}
	return nil
}

func (l *loop[T]) Configure(neferclient.SurfaceID, int32, int32) {
	l.configured = true
	l.resize()
}

func (l *loop[T]) Scale(neferclient.SurfaceID, float64) { l.resize() }

// resize reports a changed logical size or scale to OnResize. Before the
// first configure the size is not known yet.
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
}

func (l *loop[T]) Closed(neferclient.SurfaceID) { l.closed = true }

func (l *loop[T]) Error(err error) {
	if l.err == nil {
		l.err = err
	}
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
