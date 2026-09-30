package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/bnema/nefercap/internal/adapters/cli"
	"github.com/bnema/nefercap/internal/app"
	"github.com/bnema/nefercap/internal/logging"
)

func main() { os.Exit(run()) }

// Only the invocation's bare signal cancellation is a successful interruption.
// Wrapped or joined errors retain their failure so cleanup errors are visible.
func interrupted(ctx context.Context, err error) bool {
	return err == context.Canceled && ctx.Err() != nil
}

func run() int {
	options, err := cli.Parse(os.Args[1:], os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx = logging.With(ctx, os.Stderr, options.Debug)
	if err := app.Run(ctx, options, os.Stdout); err != nil {
		if interrupted(ctx, err) {
			return 0
		}
		log := logging.For(ctx, "app")
		log.Error().Err(err).Msg("capture failed")
		return 1
	}
	return 0
}
