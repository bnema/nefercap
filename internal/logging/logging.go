// Package logging configures component-scoped zerowrap logging.
package logging

import (
	"context"
	"io"

	"github.com/bnema/zerowrap"
)

// With attaches a JSON logger at info level, or debug when requested.
// A nil output uses zerowrap's default stderr output.
func With(ctx context.Context, output io.Writer, debug bool) context.Context {
	level := "info"
	if debug {
		level = "debug"
	}
	return zerowrap.WithCtx(ctx, zerowrap.New(zerowrap.Config{Level: level, Format: "json", Output: output}))
}

// For derives a component logger. Cache it outside steady-state frame loops.
func For(ctx context.Context, component string) zerowrap.Logger {
	return zerowrap.FromCtxWithField(ctx, zerowrap.FieldComponent, component)
}
