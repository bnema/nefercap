package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/bnema/nefercap/internal/adapters/control"
	"github.com/bnema/nefercap/internal/adapters/ffmpeg"
	"github.com/bnema/nefercap/internal/adapters/indicator"
	"github.com/bnema/nefercap/internal/adapters/uierrors"
	"github.com/bnema/nefercap/internal/adapters/wayland"
	"github.com/bnema/nefercap/internal/core"
	"github.com/bnema/nefercap/internal/ports"
	"github.com/bnema/nefergui"
)

// recordInteractive records with the HUD and the control socket. A stop
// requested by the user is success; the outcome says whether a file exists.
func recordInteractive(ctx context.Context, source *wayland.Source, outputs []ports.Output, sel ports.Selection) (outcome recordingOutcome, err error) {
	session, err := source.BeginSession(ctx, sel.Target, true)
	if err != nil {
		return outcome, sessionError(ctx, true, err)
	}
	sel.Target = session.Target
	var output ports.Output
	for _, candidate := range outputs {
		if candidate.ID == sel.Target.OutputID {
			output = candidate
			break
		}
	}
	if output.ID == 0 {
		return outcome, ports.ErrOutputNotFound
	}
	server, err := control.Listen()
	if err != nil {
		return outcome, err
	}
	defer func() {
		if closeErr := server.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()

	recordCtx, cancelRecord := context.WithCancelCause(ctx)
	defer cancelRecord(nil)
	hudCtx, cancelHUD := context.WithCancel(ctx)
	defer cancelHUD()
	life := newLifecycle(ctx, cancelRecord, cancelHUD)
	hudStop := make(chan struct{}, 1)
	watchCtx, stopWatch := context.WithCancel(context.Background())
	controlDone := stopOnRequest(watchCtx, server.Requests(), life.userStop)
	buttonDone := stopOnSignal(watchCtx, hudStop, life.userStop)
	defer func() { stopWatch(); <-controlDone; <-buttonDone }()

	hud := startHUD(hudCtx, output, session.Token, hudStop, life)
	end := ending{}
	started := false
	// Do not start the encoder until the compositor acknowledges exclusion.
	// A protocol roundtrip alone does not prove the core applied the marking.
	select {
	case <-hud.ready:
		started = true
		captureDone := make(chan error, 1)
		go func() { captureDone <- core.New(source, nil, ffmpeg.New()).Run(recordCtx, sel) }()
		select {
		case end.runErr = <-captureDone:
			life.beginShutdown()
			cancelHUD()
			end.hudErr = <-hud.done
		case hudErr := <-hud.done:
			end.hudErr = life.hudEnded(hudErr, false)
			end.runErr = <-captureDone
		}
	case hudErr := <-hud.done:
		end.hudErr = life.hudEnded(hudErr, !authorized(hud.ready))
	case <-recordCtx.Done():
		life.beginShutdown()
		cancelHUD()
		end.hudErr = <-hud.done
	}
	end.failure = life.failed()
	end.parentErr = ctx.Err()
	end.userStop = context.Cause(recordCtx) == errUserStop
	err = classify(end)
	if started && end.runErr == nil {
		// Frames were written: the file exists whatever the HUD reported after.
		outcome.Saved = savedFile(sel.Path)
	}
	return outcome, err
}

// sessionError classifies a BeginSession failure. The caller's own
// cancellation stays the bare context error, a missing native capability is
// named for a recording, and every other failure gets the phase and keeps its
// identity.
func sessionError(ctx context.Context, record bool, err error) error {
	if ctx.Err() != nil && uierrors.IsCancellation(err) {
		return ctx.Err()
	}
	if record && errors.Is(err, wayland.ErrSessionUnsupported) {
		return fmt.Errorf("recording indicator requires native capture-session support: %w", err)
	}
	return fmt.Errorf("begin capture session: %w", err)
}

// authorized reports, without blocking, whether the HUD signalled ready. Ready
// and done can both be pending; a HUD that was authorized did not close early.
func authorized(ready <-chan struct{}) bool {
	select {
	case <-ready:
		return true
	default:
		return false
	}
}

// hudHandle is the owner's view of a running HUD: ready fires once when the
// compositor acknowledged the exclusion, done delivers Run's result once.
type hudHandle struct {
	ready <-chan struct{}
	done  <-chan error
}

// startHUD runs the indicator on its own goroutine. Its detach callback and
// button only signal the lifecycle; they never touch the recording.
func startHUD(ctx context.Context, output ports.Output, token string, stop chan<- struct{}, life *lifecycle) hudHandle {
	ready := make(chan struct{}, 1)
	done := make(chan error, 1)
	authorize := func(hookCtx context.Context, surface nefergui.WaylandSurface) error {
		if err := wayland.AuthorizeLayer(hookCtx, surface.Display, surface.Surface, token, life.detached); err != nil {
			return err
		}
		ready <- struct{}{}
		return nil
	}
	go func() { done <- indicator.New(authorize, stop).Run(ctx, output) }()
	return hudHandle{ready: ready, done: done}
}
