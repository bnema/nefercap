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
func chooseInteractive(ctx context.Context, source *wayland.Source, outputs []ports.Output, options cli.Options) (ports.Selection, bool, error) {
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
	settings, err := config.Load()
	if err != nil {
		return ports.Selection{}, false, fmt.Errorf("load selector config: %w", err)
	}
	sel, accepted, err := selection.New(mode, options.Video, workspaces, settings.Grid).Select(ctx, outputs)
	if err != nil || !accepted {
		return ports.Selection{}, false, err
	}
	sel.Duration = options.Duration
	sel.Path = options.Path
	sel.Clipboard = options.Clipboard
	if sel.Path == "" && !sel.Clipboard {
		if mode == ports.Record {
			sel.Path, err = destination.Video()
		} else {
			sel.Path, err = destination.Screenshot()
		}
		if err != nil {
			return ports.Selection{}, false, err
		}
	}
	return sel, true, nil
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
