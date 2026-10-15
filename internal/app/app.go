// Package app wires the capture adapters to the core workflow.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/bnema/nefercap/internal/adapters/cli"
	"github.com/bnema/nefercap/internal/adapters/clipboard"
	"github.com/bnema/nefercap/internal/adapters/config"
	"github.com/bnema/nefercap/internal/adapters/flash"
	"github.com/bnema/nefercap/internal/adapters/png"
	"github.com/bnema/nefercap/internal/adapters/wayland"
	"github.com/bnema/nefercap/internal/core"
	"github.com/bnema/nefercap/internal/logging"
	"github.com/bnema/nefercap/internal/ports"
)

const discoveryTimeout = 5 * time.Second

// Run owns the source connection and all capture work for one invocation.
func Run(ctx context.Context, options cli.Options, output io.Writer) (err error) {
	if handled, controlErr := controlCommand(ctx, options.Command, output); handled {
		return controlErr
	}
	discovery, cancel := context.WithTimeout(ctx, discoveryTimeout)
	source, err := wayland.New(discovery, "")
	if err != nil {
		cancel()
		return discoveryError(ctx, "connect Wayland", err)
	}
	defer func() {
		if closeErr := source.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	outputs, err := source.Outputs(discovery)
	cancel()
	if err != nil {
		return discoveryError(ctx, "list outputs", err)
	}
	if options.Command == cli.Outputs {
		for _, item := range outputs {
			if _, err := fmt.Fprintf(output, "%s\t%dx%d\tscale=%d\n", item.Name, item.Width, item.Height, item.Scale); err != nil {
				return err
			}
		}
		return nil
	}
	var selection ports.Selection
	interactive := options.Command == cli.Shot || options.Command == cli.Rec || options.Command == cli.AllInOne
	if interactive {
		// Fail on a bad config before anything is shown.
		settings, loadErr := config.Load()
		if loadErr != nil {
			return fmt.Errorf("load config: %w", loadErr)
		}
		if err := rejectCompetingSelector(ctx); err != nil {
			return err
		}
		var accepted bool
		selection, accepted, err = chooseInteractive(ctx, source, outputs, options, settings)
		if err != nil || !accepted {
			return err
		}
	} else {
		target, err := resolveOutput(outputs, options.Output)
		if err != nil {
			return err
		}
		selection = ports.Selection{Target: ports.Target{OutputID: target.ID, Region: options.Region}, Path: options.Path, Clipboard: options.Clipboard, Video: options.Video, Duration: options.Duration}
		switch options.Command {
		case cli.Screenshot:
			selection.Mode = ports.Screenshot
		case cli.Record:
			selection.Mode = ports.Record
		default:
			return fmt.Errorf("unsupported command %q", options.Command)
		}
	}
	log := logging.For(ctx, "app")
	log.Info().Str("mode", string(selection.Mode)).Msg("capture started")
	var saved bool
	if selection.Mode == ports.Record {
		var outcome recordingOutcome
		outcome, err = recordInteractive(ctx, output, source, outputs, selection)
		saved = outcome.Saved
	} else {
		var writer ports.ScreenshotWriter = png.New()
		var copied *clipboard.Writer
		if selection.Clipboard {
			copied = clipboard.NewWriter(writer, clipboard.New())
			writer = copied
		}
		err = core.New(source, writer, nil).Run(ctx, selection)
		saved = err == nil && selection.Path != ""
		if copied != nil {
			saved = copied.Saved
		}
	}
	if saved && err == nil {
		log.Info().Msg("capture complete")
	}
	err = finishCapture(output, selection.Path, saved, err)
	if err == nil && interactive && selection.Mode == ports.Screenshot {
		confirmScreenshot(ctx, source, outputs, selection.Target)
	}
	return err
}

// confirmScreenshot flashes the captured area once the screenshot is done. The
// screenshot already succeeded, so a failed flash is only logged.
func confirmScreenshot(ctx context.Context, source *wayland.Source, outputs []ports.Output, target ports.Target) {
	for _, o := range outputs {
		if o.ID != target.OutputID {
			continue
		}
		area := target.Region
		if target.WorkspaceID != 0 {
			area = recordedFrame(ctx, source, target)
		}
		if err := flash.Show(ctx, o, area); err != nil {
			log := logging.For(ctx, "app")
			log.Warn().Err(err).Msg("screenshot confirmation not shown")
		}
		return
	}
}

// finishCapture prints the path of a file that exists, whatever error the
// capture ended with, so a reported failure never hides where the file is.
// Nothing is printed when no file was saved. The original error and a failed
// write are both returned.
func finishCapture(output io.Writer, path string, saved bool, err error) error {
	if !saved {
		return err
	}
	if _, printErr := fmt.Fprintln(output, path); printErr != nil {
		return errors.Join(err, printErr)
	}
	return err
}

// discoveryError preserves a user interrupt, but reports local timeouts and
// actual protocol failures with the failing phase.
func discoveryError(ctx context.Context, phase string, err error) error {
	if ctx.Err() != nil && err == ctx.Err() {
		return err
	}
	return fmt.Errorf("%s: %w", phase, err)
}

func resolveOutput(outputs []ports.Output, name string) (ports.Output, error) {
	if name == "" {
		if len(outputs) == 1 {
			return outputs[0], nil
		}
		return ports.Output{}, fmt.Errorf("select an output with -output (use outputs to list names)")
	}
	for _, output := range outputs {
		if output.Name == name {
			return output, nil
		}
	}
	return ports.Output{}, fmt.Errorf("%w: %q", ports.ErrOutputNotFound, name)
}
