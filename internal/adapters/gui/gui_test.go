package gui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bnema/nefercap/internal/ports"
)

func TestNewModelLabelsAndDefaults(t *testing.T) {
	m := newTestModel(t)
	require.Len(t, m.outputs, 3)
	assert.Equal(t, "DP-1  2560x1440", m.outputs[0].label)
	assert.Equal(t, "output 9  1920x1080  scale 2", m.outputs[1].label)
	assert.Equal(t, "output 11", m.outputs[2].label)
	assert.Equal(t, m.outputs[0].key, m.output)
	assert.Equal(t, "nefercap-20260102-030405.png", m.defaultShot)
	assert.Equal(t, "nefercap-20260102-030405.mp4", m.defaultRec)
	assert.Equal(t, "30", m.fps)
	assert.Equal(t, "0", m.duration)
	keys := map[string]bool{}
	for _, o := range m.outputs {
		assert.False(t, keys[o.key])
		keys[o.key] = true
	}
}

func TestNewModelNoOutputs(t *testing.T) {
	_, err := newModel(nil, time.Now())
	assert.ErrorIs(t, err, ports.ErrOutputNotFound)
}

func TestSelectRejectsBeforeOpeningWindow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, ok, err := New().Select(ctx, testOutputs)
	assert.False(t, ok)
	assert.ErrorIs(t, err, context.Canceled)

	_, ok, err = New().Select(context.Background(), nil)
	assert.False(t, ok)
	assert.ErrorIs(t, err, ports.ErrOutputNotFound)
}

func TestModeChangeSwapsOnlyUntouchedDefault(t *testing.T) {
	m := newTestModel(t)
	m.mode = modeRecord
	m.modeChanged()
	assert.Equal(t, m.defaultRec, m.path)
	m.mode = modeScreenshot
	m.modeChanged()
	assert.Equal(t, m.defaultShot, m.path)

	m.path = "mine.png"
	m.mode = modeRecord
	m.modeChanged()
	assert.Equal(t, "mine.png", m.path)
}

func TestSubmitAccepts(t *testing.T) {
	m := newTestModel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.cancel = cancel
	m.path = filepath.Join(t.TempDir(), "shot.png")
	m.submit()
	assert.True(t, m.accepted)
	assert.Error(t, ctx.Err())
	assert.Equal(t, m.path, m.sel.Path)
	assert.Equal(t, ports.Screenshot, m.sel.Mode)

	// Later dismissal cannot revoke an accepted selection.
	m.dismiss()
	assert.False(t, m.closed)
}

func TestSubmitInvalidStaysOpen(t *testing.T) {
	m := newTestModel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.cancel = cancel
	m.path = ""
	m.submit()
	assert.False(t, m.accepted)
	assert.NoError(t, ctx.Err())
}

func TestSubmitExistingPathStaysOpen(t *testing.T) {
	m := newTestModel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.cancel = cancel
	m.path = filepath.Join(t.TempDir(), "taken.png")
	require.NoError(t, os.WriteFile(m.path, nil, 0o600))
	m.submit()
	assert.False(t, m.accepted)
	assert.NoError(t, ctx.Err())
	text, submittable, ok := m.status()
	assert.Equal(t, noticeExists, text)
	assert.True(t, submittable)
	assert.False(t, ok)

	m.pathChanged()
	text, submittable, ok = m.status()
	assert.Equal(t, statusReady, text)
	assert.True(t, submittable)
	assert.True(t, ok)
}

func TestStatusInvalid(t *testing.T) {
	m := newTestModel(t)
	m.region = "bad"
	text, submittable, ok := m.status()
	assert.Equal(t, errRegion.Error(), text)
	assert.False(t, submittable)
	assert.False(t, ok)
}

func TestDismiss(t *testing.T) {
	m := newTestModel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.cancel = cancel
	m.dismiss()
	assert.True(t, m.closed)
	assert.Error(t, ctx.Err())
	m.submit()
	assert.False(t, m.accepted)
}

func TestResult(t *testing.T) {
	closeErr := errors.New("close surface")
	canceledParent, cancelParent := context.WithCancel(context.Background())
	cancelParent()
	tests := []struct {
		name       string
		parent     context.Context
		accepted   bool
		closed     bool
		runErr     error
		wantOK     bool
		wantErr    error
		wantNoErr  bool
		wantNotErr error
	}{
		{name: "accepted canceled child", parent: context.Background(), accepted: true, runErr: context.Canceled, wantOK: true, wantNoErr: true},
		{name: "accepted nil run", parent: context.Background(), accepted: true, wantOK: true, wantNoErr: true},
		{name: "closed by button", parent: context.Background(), closed: true, runErr: context.Canceled, wantNoErr: true},
		{name: "closed by window", parent: context.Background(), wantNoErr: true},
		{name: "accepted but close failed", parent: context.Background(), accepted: true, runErr: errors.Join(context.Canceled, closeErr), wantErr: closeErr, wantNotErr: context.Canceled},
		{name: "accepted but run failed", parent: context.Background(), accepted: true, runErr: closeErr, wantErr: closeErr},
		{name: "closed with failure", parent: context.Background(), closed: true, runErr: fmt.Errorf("x: %w", closeErr), wantErr: closeErr},
		{name: "setup failure", parent: context.Background(), runErr: closeErr, wantErr: closeErr},
		{name: "unexplained cancel", parent: context.Background(), runErr: context.Canceled, wantErr: context.Canceled},
		{name: "parent canceled", parent: canceledParent, runErr: context.Canceled, wantErr: context.Canceled},
		{name: "parent canceled after accept", parent: canceledParent, accepted: true, runErr: context.Canceled, wantErr: context.Canceled},
		{name: "parent canceled with close failure", parent: canceledParent, accepted: true, runErr: errors.Join(context.Canceled, closeErr), wantErr: closeErr},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestModel(t)
			m.accepted, m.closed = tt.accepted, tt.closed
			m.sel = ports.Selection{Mode: ports.Record, Path: "x.mp4"}
			sel, ok, err := m.result(tt.parent, tt.runErr)
			assert.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				assert.Equal(t, m.sel, sel)
			} else {
				assert.Equal(t, ports.Selection{}, sel)
			}
			if tt.wantNoErr {
				assert.NoError(t, err)
			}
			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
			}
			if tt.wantNotErr != nil {
				assert.NotErrorIs(t, err, tt.wantNotErr)
			}
		})
	}
}

func TestDropCanceled(t *testing.T) {
	real := errors.New("real")
	assert.NoError(t, dropCanceled(nil))
	assert.NoError(t, dropCanceled(context.Canceled))
	assert.NoError(t, dropCanceled(errors.Join(context.Canceled, context.Canceled)))
	assert.Same(t, real, dropCanceled(errors.Join(context.Canceled, real)))
	wrapped := fmt.Errorf("wrapped: %w", context.Canceled)
	assert.Equal(t, wrapped, dropCanceled(wrapped), "wrapped cancellation is not ours")
	both := dropCanceled(errors.Join(real, errors.New("second")))
	assert.ErrorIs(t, both, real)
}

func TestWriteStyleSheet(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	path, remove, err := writeStyleSheet()
	require.NoError(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, styleSheet, string(data))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	remove()
	_, err = os.Stat(path)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestStyleSheetUsesMonospace(t *testing.T) {
	assert.Contains(t, styleSheet, "monospace")
}

func TestWriteStyleSheetFailure(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	_, _, err := writeStyleSheet()
	assert.Error(t, err)
}

func TestWindowHeightIsBounded(t *testing.T) {
	m := newTestModel(t)
	assert.Equal(t, 600+26*3, m.windowHeight())
	m.outputs = make([]outputChoice, 100)
	assert.Equal(t, 1000, m.windowHeight())
}

func TestModeChangeNoop(t *testing.T) {
	m := newTestModel(t)
	m.notice = "keep"
	m.modeChanged()
	assert.Equal(t, "keep", m.notice)
	m.mode = modeRecord
	m.modeChanged()
	assert.Empty(t, m.notice)
}
