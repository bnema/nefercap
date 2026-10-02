package app

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/bnema/nefercap/internal/adapters/control"
	"github.com/bnema/nefercap/internal/adapters/ffmpeg"
	"github.com/bnema/nefercap/internal/adapters/indicator"
	"github.com/bnema/nefercap/internal/adapters/uierrors"
	"github.com/bnema/nefercap/internal/adapters/wayland"
	"github.com/bnema/nefercap/internal/core"
	"github.com/bnema/nefercap/internal/logging"
	"github.com/bnema/nefercap/internal/ports"
	"github.com/bnema/wlturbo"
)

// recordInteractive records and listens for stop requests. With the
// compositor's capture exclusion it also shows the HUD, excluded from the
// video; without it nothing is drawn, so the user stops with the same
// shortcut or `nefercap stop`. A stop requested by the user is success; the
// outcome says whether a file exists.
func recordInteractive(ctx context.Context, notices io.Writer, source *wayland.Source, outputs []ports.Output, sel ports.Selection) (outcome recordingOutcome, err error) {
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
	token, withHUD, err := startExclusion(ctx, notices, source, sel.Target)
	if err != nil {
		return outcome, err
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

	end := ending{}
	var video *ffmpeg.Writer
	run := func() <-chan error {
		video = ffmpeg.New()
		captureDone := make(chan error, 1)
		go func() { captureDone <- core.New(source, nil, video).Run(recordCtx, sel) }()
		return captureDone
	}
	if !withHUD {
		end.runErr = <-run()
		life.beginShutdown()
	} else {
		hud := startHUD(hudCtx, output, recordedFrame(ctx, source, sel.Target), token, hudStop, life)
		// Do not start the encoder until the compositor acknowledges the
		// exclusion: a HUD that is not excluded would be in the video.
		select {
		case <-hud.ready:
			captureDone := run()
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
	}
	end.failure = life.failed()
	end.parentErr = ctx.Err()
	end.userStop = context.Cause(recordCtx) == errUserStop
	err = classify(end)
	if video != nil {
		// Finalization may retain frames despite a late capture or HUD error.
		// The writer knows whether it kept its file, not a pre-existing path.
		outcome.Saved = video.Saved
	}
	return outcome, err
}

// startExclusion asks the compositor to keep the HUD out of the recording. It
// reports whether a HUD can be shown: without the extension, or when the
// compositor refuses the exclusion because another one is live (busy), the
// recording goes on without HUD, like on a compositor with no extension. Any
// other failure is fatal, including a session the compositor stopped.
func startExclusion(ctx context.Context, notices io.Writer, source *wayland.Source, target ports.Target) (token string, withHUD bool, err error) {
	if !source.Capabilities().Exclusion {
		return "", false, nil
	}
	token, err = source.BeginExclusion(ctx, target)
	switch {
	case err == nil:
		return token, true, nil
	case errors.Is(err, wayland.ErrExclusionBusy) && ctx.Err() == nil:
		log := logging.For(ctx, "app")
		log.Warn().Err(err).Msg("capture exclusion refused: recording without indicator")
		fmt.Fprintln(notices, "recording without indicator: stop with the same shortcut or `nefercap stop`")
		return "", false, nil
	}
	return "", false, exclusionError(ctx, err)
}

// exclusionError classifies a BeginExclusion failure. The caller's own
// cancellation stays the bare context error; every other failure gets the
// phase and keeps its identity.
func exclusionError(ctx context.Context, err error) error {
	if ctx.Err() != nil && uierrors.IsCancellation(err) {
		return ctx.Err()
	}
	return fmt.Errorf("begin capture exclusion: %w", err)
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
func startHUD(ctx context.Context, output ports.Output, frame ports.Region, token string, stop chan<- struct{}, life *lifecycle) hudHandle {
	ready := make(chan struct{}, 1)
	done := make(chan error, 1)
	authorize := func(hookCtx context.Context, display *wlturbo.Display, surface wlturbo.Proxy) error {
		if err := wayland.AuthorizeLayer(hookCtx, display, surface, token, life.detached); err != nil {
			return err
		}
		ready <- struct{}{}
		return nil
	}
	go func() {
		hud := indicator.New(authorize, stop)
		hud.Frame = frame
		done <- hud.Run(ctx, output)
	}()
	return hudHandle{ready: ready, done: done}
}

// recordedFrame is where a workspace target is on its output, so that the HUD
// sits in it. Anything else, or a workspace that cannot be found, is the whole
// output (the zero Region).
func recordedFrame(ctx context.Context, source *wayland.Source, t ports.Target) ports.Region {
	if t.WorkspaceID == 0 {
		return ports.Region{}
	}
	list, err := source.Workspaces(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ports.Region{}
		}
		log := logging.For(ctx, "app")
		log.Warn().Err(err).Msg("workspace frame unavailable: HUD on the whole output")
		return ports.Region{}
	}
	for _, w := range list {
		if w.ID == t.WorkspaceID && w.OutputID == t.OutputID {
			return w.Region
		}
	}
	return ports.Region{}
}
