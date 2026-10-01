package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bnema/nefercap/internal/adapters/wayland"
	"github.com/bnema/nefercap/internal/adapters/wayland/testserver"
	"github.com/bnema/nefercap/internal/logging"
	"github.com/bnema/nefercap/internal/ports"
)

func sourceFor(t *testing.T, cfg testserver.Config) (context.Context, *bytes.Buffer, *wayland.Source, ports.Target) {
	t.Helper()
	var logs bytes.Buffer
	ctx := logging.With(context.Background(), &logs, false)
	srv := testserver.Start(t, cfg)
	source, err := wayland.New(ctx, srv.Path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })
	outs, err := source.Outputs(ctx)
	require.NoError(t, err)
	return ctx, &logs, source, ports.Target{OutputID: outs[0].ID}
}

func TestStartExclusionWithExtension(t *testing.T) {
	ctx, _, source, target := sourceFor(t, testserver.Config{NeferwlExclusion: true})
	token, withHUD, err := startExclusion(ctx, io.Discard, source, target)
	require.NoError(t, err)
	require.True(t, withHUD)
	require.Equal(t, testserver.Token, token)
}

func TestStartExclusionWithoutExtensionHasNoHUD(t *testing.T) {
	ctx, _, source, target := sourceFor(t, testserver.Config{})
	token, withHUD, err := startExclusion(ctx, io.Discard, source, target)
	require.NoError(t, err)
	require.False(t, withHUD)
	require.Empty(t, token)
}

// A busy exclusion records without HUD and logs it at warn.
func TestStartExclusionBusyFallsBackWithoutHUD(t *testing.T) {
	ctx, logs, source, target := sourceFor(t, testserver.Config{NeferwlExclusion: true, ExclusionFail: 1})
	var notices bytes.Buffer
	token, withHUD, err := startExclusion(ctx, &notices, source, target)
	require.NoError(t, err)
	require.False(t, withHUD)
	require.Empty(t, token)
	require.Contains(t, notices.String(), "recording without indicator")
	require.Contains(t, logs.String(), `"level":"warn"`)
	require.Contains(t, logs.String(), "recording without indicator")
}

// Other failures stay fatal.
func TestStartExclusionOtherErrorsAreFatal(t *testing.T) {
	ctx, _, source, _ := sourceFor(t, testserver.Config{NeferwlExclusion: true})
	_, _, err := startExclusion(ctx, io.Discard, source, ports.Target{OutputID: 9999})
	require.ErrorIs(t, err, ports.ErrOutputNotFound)
	require.False(t, errors.Is(err, wayland.ErrExclusionUnavailable))
}

// Any other refusal reason is fatal too, never a fallback.
func TestStartExclusionOtherReasonIsFatal(t *testing.T) {
	ctx, _, source, target := sourceFor(t, testserver.Config{NeferwlExclusion: true, ExclusionFail: 4})
	var notices bytes.Buffer
	_, withHUD, err := startExclusion(ctx, &notices, source, target)
	require.ErrorIs(t, err, wayland.ErrExclusionUnavailable)
	require.False(t, errors.Is(err, wayland.ErrExclusionBusy))
	require.False(t, withHUD)
	require.Empty(t, notices.String())
}

// A session the compositor stops is fatal, not a fallback: no second session.
func TestStartExclusionStoppedSessionIsFatal(t *testing.T) {
	ctx, _, source, target := sourceFor(t, testserver.Config{NeferwlExclusion: true, ExclusionFail: 2})
	_, _, err := startExclusion(ctx, io.Discard, source, target)
	require.ErrorIs(t, err, ports.ErrCaptureStopped)
	require.False(t, errors.Is(err, wayland.ErrExclusionUnavailable))
}

// A compositor that refuses the client stops the session at creation: the
// error says so and nothing retries.
func TestStartExclusionRefusedClientIsFatal(t *testing.T) {
	ctx, _, source, target := sourceFor(t, testserver.Config{NeferwlExclusion: true, StopAtCreate: true})
	_, _, err := startExclusion(ctx, io.Discard, source, target)
	require.ErrorIs(t, err, ports.ErrCaptureStopped)
	require.ErrorContains(t, err, "/etc/neferwl/capture-allow")
}
