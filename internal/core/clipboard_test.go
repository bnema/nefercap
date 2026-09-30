package core_test

import (
	"context"
	"github.com/bnema/nefercap/internal/core"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestClipboardSelectionValidation(t *testing.T) {
	h := newHarness(t)
	sel := shot()
	sel.Clipboard = true
	sel.Path = ""
	h.src.EXPECT().Capture(mock.Anything, sel.Target).Return(frame, nil).Once()
	h.png.EXPECT().Save(mock.Anything, frame, "").Return(nil).Once()
	require.NoError(t, h.svc.Run(context.Background(), sel))
	h = newHarness(t)
	sel = rec(0)
	sel.Clipboard = true
	require.ErrorIs(t, h.svc.Run(context.Background(), sel), core.ErrInvalidSelection)
	h = newHarness(t)
	sel = shot()
	sel.Clipboard = true
	sel.Path = ""
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, h.svc.Run(ctx, sel), context.Canceled)
}
