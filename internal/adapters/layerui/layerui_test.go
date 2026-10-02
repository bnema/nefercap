package layerui

import (
	"testing"

	"github.com/bnema/neferclient"
	"github.com/bnema/nefergui"
	"github.com/stretchr/testify/assert"
)

func TestPointerInput(t *testing.T) {
	cases := []struct {
		ev   neferclient.PointerEvent
		want nefergui.Input
		ok   bool
	}{
		{neferclient.PointerEvent{Kind: neferclient.PointerEnter, X: 1, Y: 2}, nefergui.Input{Kind: nefergui.InputPointerMotion, X: 1, Y: 2}, true},
		{neferclient.PointerEvent{Kind: neferclient.PointerMotion, X: 3, Y: 4}, nefergui.Input{Kind: nefergui.InputPointerMotion, X: 3, Y: 4}, true},
		{neferclient.PointerEvent{Kind: neferclient.PointerLeave, X: 5, Y: 6}, nefergui.Input{Kind: nefergui.InputPointerLeave, X: 5, Y: 6}, true},
		{neferclient.PointerEvent{Kind: neferclient.PointerButton, X: 7, Y: 8, Button: 0x110, Pressed: true},
			nefergui.Input{Kind: nefergui.InputPointerPress, X: 7, Y: 8, Button: 0x110, Pressed: true}, true},
		{neferclient.PointerEvent{Kind: neferclient.PointerButton, X: 7, Y: 8, Button: 0x110},
			nefergui.Input{Kind: nefergui.InputPointerRelease, X: 7, Y: 8, Button: 0x110}, true},
		{neferclient.PointerEvent{Kind: neferclient.PointerAxis, DX: 1, DY: -2}, nefergui.Input{Kind: nefergui.InputPointerAxis, DX: 1, DY: -2}, true},
		{neferclient.PointerEvent{Kind: neferclient.PointerAxisStop}, nefergui.Input{}, false},
	}
	for _, c := range cases {
		got, ok := pointerInput(&c.ev)
		assert.Equal(t, c.ok, ok, "kind %d", c.ev.Kind)
		assert.Equal(t, c.want, got, "kind %d", c.ev.Kind)
	}
}

func TestKeyInputKeepsKnownModifiersAndText(t *testing.T) {
	text := []byte("é")
	ev := neferclient.KeyEvent{Keysym: 0xe9, Pressed: true, Repeat: true, Text: text,
		Modifiers: neferclient.ModShift | neferclient.ModCtrl | neferclient.ModCapsLock | 1<<7}
	got := keyInput(&ev)
	assert.Equal(t, nefergui.InputKey, got.Kind)
	assert.Equal(t, uint32(0xe9), got.Keysym)
	assert.True(t, got.Pressed)
	assert.True(t, got.Repeat)
	assert.Equal(t, nefergui.ModShift|nefergui.ModCtrl|nefergui.ModCapsLock, got.Modifiers, "unknown bits are dropped")
	assert.Equal(t, text, got.Text)
}

// Both libraries number the shared modifiers the same way, so the bits copy.
func TestModifierBitsMatch(t *testing.T) {
	pairs := [][2]uint8{
		{uint8(neferclient.ModShift), uint8(nefergui.ModShift)},
		{uint8(neferclient.ModCtrl), uint8(nefergui.ModCtrl)},
		{uint8(neferclient.ModAlt), uint8(nefergui.ModAlt)},
		{uint8(neferclient.ModSuper), uint8(nefergui.ModSuper)},
		{uint8(neferclient.ModCapsLock), uint8(nefergui.ModCapsLock)},
		{uint8(neferclient.ModNumLock), uint8(nefergui.ModNumLock)},
	}
	for _, p := range pairs {
		assert.Equal(t, p[0], p[1])
	}
}
