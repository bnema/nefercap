package app

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/bnema/nefercap/internal/adapters/cli"
	"github.com/bnema/nefercap/internal/adapters/control"
)

// controlCommand resolves commands that must work without opening a GUI or
// connecting to Wayland. A rec or all-in-one invocation is a stop toggle when
// already active.
func controlCommand(ctx context.Context, command cli.Command, output io.Writer) (handled bool, err error) {
	switch command {
	case cli.Rec, cli.AllInOne, cli.Stop:
		stopped, err := control.Stop(ctx)
		if err != nil {
			return true, err
		}
		if command == cli.Stop {
			if !stopped {
				return true, errors.New("no recording is active")
			}
			return true, nil
		}
		return stopped, nil
	case cli.Status:
		active, err := control.Query(ctx)
		if err != nil {
			return true, err
		}
		state := "idle"
		if active {
			state = "recording"
		}
		_, err = fmt.Fprintln(output, state)
		return true, err
	default:
		return false, nil
	}
}

// stopOnRequest has one bounded lifetime per recording; the completion channel
// is joined before closing the control server. No request starts a new worker.
func stopOnRequest(ctx context.Context, requests <-chan control.Request, cancel context.CancelFunc) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
		case request := <-requests:
			if request == control.RequestStop {
				cancel()
			}
		}
	}()
	return done
}
