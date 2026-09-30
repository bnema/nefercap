package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/bnema/nefercap/internal/adapters/cli"
	"github.com/bnema/nefercap/internal/adapters/config"
	"github.com/bnema/nefercap/internal/adapters/control"
	"github.com/bnema/nefercap/internal/adapters/destination"
	"github.com/bnema/nefercap/internal/adapters/selection"
	"github.com/bnema/nefercap/internal/adapters/uierrors"
	"github.com/bnema/nefercap/internal/adapters/wayland"
	"github.com/bnema/nefercap/internal/ports"
)

// chooseInteractive uses native workspace metadata only when the compositor
// exposes it. Monitor and region screenshots still work with standard capture.
func chooseInteractive(ctx context.Context, source *wayland.Source, outputs []ports.Output, options cli.Options, settings config.Settings) (ports.Selection, bool, error) {
	mode := ports.Screenshot
	if options.Command == cli.Rec {
		mode = ports.Record
	}
	if options.Output != "" {
		chosen, err := resolveOutput(outputs, options.Output)
		if err != nil {
			return ports.Selection{}, false, err
		}
		outputs = []ports.Output{chosen}
	}
	workspaces, err := source.Workspaces(ctx)
	if err != nil && !errors.Is(err, wayland.ErrSessionUnsupported) {
		if ctx.Err() != nil && uierrors.IsCancellation(err) {
			return ports.Selection{}, false, ctx.Err()
		}
		return ports.Selection{}, false, fmt.Errorf("list workspace capture targets: %w", err)
	}
	if err := checkDirs(options, settings); err != nil {
		return ports.Selection{}, false, err
	}
	selector := selection.New(mode, options.Video, workspaces, settings.Grid)
	if options.Command == cli.AllInOne {
		selector.AllowToggle()
	}
	sel, accepted, err := selector.Select(ctx, outputs)
	if err != nil || !accepted {
		return ports.Selection{}, false, err
	}
	sel.Duration = options.Duration
	if err := assignDestination(&sel, options, settings); err != nil {
		return ports.Selection{}, false, err
	}
	return sel, true, nil
}

// checkDirs creates and checks the default directories a confirmed capture
// can use, so a bad screenshot.dir or video.dir fails before the selector
// opens. All-in-one can reach both modes.
func checkDirs(options cli.Options, settings config.Settings) error {
	if options.Path != "" {
		return nil
	}
	all := options.Command == cli.AllInOne
	if all || options.Command == cli.Rec {
		if _, err := destination.VideoIn(settings.VideoDir); err != nil {
			return err
		}
	}
	if (all || options.Command == cli.Shot) && !options.Clipboard && settings.Output != config.OutputClipboard {
		if _, err := destination.ScreenshotIn(settings.ScreenshotDir); err != nil {
			return err
		}
	}
	return nil
}

// assignDestination resolves where an interactive capture goes. Flags win over
// the config: -file alone is a file only, -clipboard alone is clipboard only,
// both save and copy. Without flags a screenshot follows screenshot.output.
// Recordings always write a file; only screenshots are copied.
func assignDestination(sel *ports.Selection, options cli.Options, settings config.Settings) (err error) {
	if sel.Mode == ports.Record {
		if sel.Path = options.Path; sel.Path == "" {
			sel.Path, err = destination.VideoIn(settings.VideoDir)
		}
		return err
	}
	if options.Path != "" || options.Clipboard {
		sel.Path, sel.Clipboard = options.Path, options.Clipboard
		return nil
	}
	sel.Clipboard = settings.Output != config.OutputFile
	if settings.Output != config.OutputClipboard {
		sel.Path, err = destination.ScreenshotIn(settings.ScreenshotDir)
	}
	return err
}

func rejectCompetingSelector(ctx context.Context) error {
	active, err := control.Query(ctx)
	if err != nil {
		return err
	}
	if active {
		// A selector on another capture connection is not automatically part
		// of the recording's excluded set. Refuse rather than leak it into
		// an existing recording until cross-session selection is authorized.
		return errors.New("recording is active; stop it before opening a capture selector")
	}
	return nil
}
