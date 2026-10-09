package wayland

import (
	"context"
	"errors"
	"fmt"
	"slices"

	extworkspace "github.com/bnema/go-wayland-bindings/client/extworkspace"

	"github.com/bnema/nefercap/internal/ports"
)

// Freeze captures each output in ids once and keeps a private copy. Until
// Thaw or Close, Capture serves those copies instead of the live screen:
// whole outputs, regions and workspaces are cut from what the screen showed at
// Freeze. An output that cannot be captured as is (a rotated one) is left out
// and stays live. The returned frames, by output ID, own their storage and
// stay valid until Thaw or Close; the caller must not modify them.
func (s *Source) Freeze(ctx context.Context, ids []uint32) (map[uint32]ports.Frame, error) {
	frozen := make(map[uint32]ports.Frame, len(ids))
	for _, id := range ids {
		f, err := s.Capture(ctx, ports.Target{OutputID: id})
		if errors.Is(err, ports.ErrUnsupportedTransform) {
			s.log.Info().Uint32("output", id).Msg("rotated output stays live")
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("freeze output %d: %w", id, err)
		}
		frozen[id] = ownedCopy(f)
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if s.dead.Load() {
		return nil, ports.ErrClosed
	}
	// Nothing is captured live until Thaw, unless an output stayed live: let
	// the compositor drop the session and free the shared buffers, which a
	// later live capture creates again.
	s.dropSession()
	if err := errors.Join(s.releaseBuffer(0, true), s.releaseBuffer(1, true)); err != nil {
		s.terminate()
		return nil, err
	}
	s.frozen = frozen
	return frozen, nil
}

// Thaw makes Capture read the live screen again and drops the frozen copies,
// except the one of each output in keep.
func (s *Source) Thaw(keep ...uint32) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	for id := range s.frozen {
		if !slices.Contains(keep, id) {
			delete(s.frozen, id)
		}
	}
	if len(s.frozen) == 0 {
		s.frozen = nil
	}
}

// ownedCopy packs f into new storage, without stride padding.
func ownedCopy(f ports.Frame) ports.Frame {
	row := f.Width * ports.BytesPerPixel
	pixels := make([]byte, row*f.Height)
	for y := range f.Height {
		copy(pixels[y*row:], f.Row(y))
	}
	return ports.Frame{Pixels: pixels, Width: f.Width, Height: f.Height, Stride: row, Format: f.Format}
}

// frozenTarget cuts t from the frozen frame of its output; ok is false when
// that output is not frozen and is captured live. The caller holds opMu.
func (s *Source) frozenTarget(t ports.Target) (_ ports.Frame, ok bool, _ error) {
	f, ok := s.frozen[t.OutputID]
	out := s.outputs[t.OutputID]
	if !ok || out == nil {
		return ports.Frame{}, false, nil
	}
	region := t.Region
	if t.WorkspaceID != 0 {
		if t.Region != (ports.Region{}) {
			return ports.Frame{}, true, ports.ErrInvalidRegion
		}
		// As live without NeferWL's extension: only the workspace shown on
		// this output at Freeze is what the copy shows.
		w := s.ws.byID[t.WorkspaceID]
		if w == nil || w.removed || w.state&extworkspace.STATE_ACTIVE == 0 || w.group == nil || !w.group.outputs[out.proxy.ID()] {
			return ports.Frame{}, true, ports.ErrWorkspaceUnavailable
		}
		region = workspaceRegion(w, out)
		if lw, lh := out.logicalSize(); region == (ports.Region{Width: lw, Height: lh}) {
			region = ports.Region{}
		}
	}
	if region == (ports.Region{}) {
		return f, true, nil
	}
	f, err := cropFrame(f, region, out)
	return f, true, err
}
