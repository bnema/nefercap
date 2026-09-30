package wayland

import (
	"errors"
	"fmt"

	"github.com/bnema/wlturbo"
	"github.com/bnema/wlturbo/protocol/core"
	"golang.org/x/sys/unix"

	"github.com/bnema/nefercap/internal/ports"
)

// bufGeom is the buffer layout announced by the compositor. It is reused only
// while the compositor keeps announcing the same one.
type bufGeom struct {
	format, width, height, stride uint32
	size                          int
}

// validate bounds every dimension before any memory is created or mapped.
func (g *bufGeom) validate() error {
	if g.width < 1 || g.height < 1 || g.width > ports.MaxDimension || g.height > ports.MaxDimension {
		return fmt.Errorf("wayland: invalid buffer dimensions %dx%d", g.width, g.height)
	}
	if g.stride < g.width*ports.BytesPerPixel || g.stride%ports.BytesPerPixel != 0 || g.stride > ports.MaxFrameBytes/g.height {
		return fmt.Errorf("wayland: invalid buffer stride %d for %dx%d", g.stride, g.width, g.height)
	}
	g.size = int(g.stride) * int(g.height)
	return nil
}

// shmBuffer is one wl_shm mapping and wl_buffer reused between captures. A
// Source has two: one holds the last frame while the next is written. The pool
// proxy is destroyed right after buffer creation, which the protocol allows:
// the buffer keeps the compositor-side mapping alive.
type shmBuffer struct {
	geom   bufGeom
	data   []byte
	buffer *core.Buffer
}

// ensureBuffer makes s.bufs[i] match g, reusing it when unchanged.
func (s *Source) ensureBuffer(i int, g bufGeom) error {
	if s.bufs[i].buffer != nil && s.bufs[i].geom == g {
		return nil
	}
	if err := s.releaseBuffer(i, true); err != nil {
		s.terminate()
		return err
	}
	fd, err := wlturbo.CreateAnonymousFile(int64(g.size))
	if err != nil {
		return fmt.Errorf("wayland: create shm file: %w", err)
	}
	data, err := wlturbo.MapMemory(fd, g.size)
	if err != nil {
		_ = unix.Close(fd)
		return fmt.Errorf("wayland: map shm file: %w", err)
	}
	// Context.Request closes fd once the message is sent, so on success the
	// descriptor belongs to the transport (the mapping outlives it). On error
	// the caller still owns it.
	pool, err := s.shm.CreatePool(fd, int32(g.size))
	if err != nil {
		_ = unix.Close(fd)
		_ = wlturbo.UnmapMemory(data)
		s.terminate()
		return fmt.Errorf("wayland: create pool: %w", err)
	}
	buffer, err := pool.CreateBuffer(0, int32(g.width), int32(g.height), int32(g.stride), g.format)
	_ = pool.Destroy()
	if err != nil {
		_ = wlturbo.UnmapMemory(data)
		s.terminate()
		return fmt.Errorf("wayland: create buffer: %w", err)
	}
	s.bufs[i] = shmBuffer{geom: g, data: data, buffer: buffer}
	return nil
}

// releaseBuffer unmaps the storage of s.bufs[i]. With destroy set, and while
// connected, it also destroys the wl_buffer. The caller holds opMu.
func (s *Source) releaseBuffer(i int, destroy bool) error {
	b := s.bufs[i]
	s.bufs[i] = shmBuffer{}
	var err error
	if b.buffer != nil && destroy && !s.dead.Load() {
		err = b.buffer.Destroy()
	}
	if b.data != nil {
		err = errors.Join(err, wlturbo.UnmapMemory(b.data))
	}
	return err
}

// releaseBuffers releases both buffers without destroying the wl_buffers: the
// connection is ending.
func (s *Source) releaseBuffers() error {
	return errors.Join(s.releaseBuffer(0, false), s.releaseBuffer(1, false))
}
