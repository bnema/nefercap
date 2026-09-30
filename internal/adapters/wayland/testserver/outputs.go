package testserver

import (
	"github.com/bnema/purego-libwayland/protocol/extimagecapturesource"
	"github.com/bnema/purego-libwayland/protocol/wayland"
	"github.com/bnema/purego-libwayland/server"
	"golang.org/x/sys/unix"
)

func (s *Server) addOutput(spec OutputSpec, must func(error)) {
	o := &output{spec: spec}
	v := spec.Version
	if v == 0 {
		v = 4
	}
	g, err := s.d.AddGlobal(wayland.OutputInterface, int32(v), func(c server.Client, ver, id uint32) {
		res, err := wayland.NewOutput(c, int32(ver), id, outputHandler{})
		if err != nil {
			must(err)
			return
		}
		o.res = append(o.res, res)
		res.SendGeometry(0, 0, 0, 0, 0, "test", "test", 0)
		res.SendMode(1, int32(spec.Width), int32(spec.Height), 60000)
		if ver >= 2 {
			scale := spec.Scale
			if scale == 0 {
				scale = 1
			}
			res.SendScale(scale)
		}
		if ver >= 4 {
			res.SendName(spec.Name)
		}
		if ver >= 2 {
			res.SendDone()
		}
	})
	must(err)
	o.global = g
	s.outputs = append(s.outputs, o)
}

type outputHandler struct{}

func (outputHandler) Release(*wayland.Output) {}

func (s *Server) outputOf(r *server.Resource) *output {
	for _, o := range s.outputs {
		for _, res := range o.res {
			if res.Resource == r {
				return o
			}
		}
	}
	return nil
}

// RemoveOutput withdraws the named output's global and stops its sessions.
func (s *Server) RemoveOutput(name string) {
	s.Do(func() {
		for _, o := range s.outputs {
			if o.spec.Name == name && !o.removed {
				o.removed = true
				o.global.Remove()
				for _, c := range s.sessions {
					if c.out == o {
						c.stop()
					}
				}
			}
		}
	})
}

// pool is one wl_shm_pool mapping, shared by the buffers made from it.
type pool struct {
	data      []byte
	refs      int
	destroyed bool
}

func (p *pool) release() {
	if p.refs == 0 && p.destroyed && p.data != nil {
		_ = unix.Munmap(p.data)
		p.data = nil
	}
}

type buffer struct {
	p                             *pool
	offset                        int
	width, height, stride, format uint32
}

func (b *buffer) bytes() []byte {
	n := int(b.stride) * int(b.height)
	if b.p.data == nil || b.offset+n > len(b.p.data) {
		return nil
	}
	return b.p.data[b.offset : b.offset+n]
}

type shmHandler struct{ s *Server }

func (h shmHandler) CreatePool(self *wayland.Shm, id uint32, fd int, size int32) {
	defer unix.Close(fd)
	data, err := unix.Mmap(fd, 0, int(size), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		self.PostError(0, "mmap failed")
		return
	}
	_, _ = wayland.NewShmPool(self.Client(), 1, id, &poolHandler{s: h.s, p: &pool{data: data}})
}
func (shmHandler) Release(*wayland.Shm) {}

type poolHandler struct {
	s *Server
	p *pool
}

func (h *poolHandler) CreateBuffer(self *wayland.ShmPool, id uint32, offset, width, height, stride int32, format uint32) {
	b := &buffer{p: h.p, offset: int(offset), width: uint32(width), height: uint32(height), stride: uint32(stride), format: format}
	res, err := wayland.NewBuffer(self.Client(), 1, id, bufferHandler{h.s, b})
	if err != nil {
		return
	}
	h.p.refs++
	h.s.bufs[res.Resource] = b
	h.s.stats.Buffers++
	res.OnDestroy = func() {
		delete(h.s.bufs, res.Resource)
		h.s.stats.DestroyedBuffers++
		h.p.refs--
		h.p.release()
	}
}
func (h *poolHandler) Destroy(*wayland.ShmPool)       { h.p.destroyed = true; h.p.release() }
func (h *poolHandler) Resize(*wayland.ShmPool, int32) {}

type bufferHandler struct {
	s *Server
	b *buffer
}

func (bufferHandler) Destroy(*wayland.Buffer) {}

type outputSourceManager struct{ s *Server }

func (m outputSourceManager) CreateSource(self *extimagecapturesource.ExtOutputImageCaptureSourceManagerV1, id uint32, out *wayland.Output) {
	m.s.newSource(self.Client(), id, &source{kind: "output", out: m.s.outputOf(out.Resource)})
}
func (outputSourceManager) Destroy(*extimagecapturesource.ExtOutputImageCaptureSourceManagerV1) {}

type source struct {
	kind   string // output, region, workspace
	out    *output
	region [4]int32
	ws     *workspace
	gone   bool // the workspace handle was already removed
}

func (s *Server) newSource(c server.Client, id uint32, src *source) {
	res, err := extimagecapturesource.NewExtImageCaptureSourceV1(c, 1, id, sourceHandler{})
	if err != nil {
		return
	}
	s.sources[res.Resource] = src
	res.OnDestroy = func() { delete(s.sources, res.Resource) } // sessions keep their own pointer
}

type sourceHandler struct{}

func (sourceHandler) Destroy(*extimagecapturesource.ExtImageCaptureSourceV1) {}
