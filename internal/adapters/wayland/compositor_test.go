package wayland

import (
	"bytes"
	"encoding/binary"
	"net"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// This file implements the compositor side of the Wayland wire protocol over a
// real Unix socket: it decodes the requests the Source sends (including
// SCM_RIGHTS descriptors), maps the client's shm pools and answers with
// encoded events. It is a protocol peer, not a substitute for a project type.

const (
	kindDisplay = iota + 1
	kindRegistry
	kindOutput
	kindShm
	kindPool
	kindBuffer
	kindScreencopy
	kindFrame
)

const (
	globalShm        = 1000
	globalScreencopy = 1001
)

type outputSpec struct {
	name          string
	width, height uint32
	scale         int32
	version       uint32
}

// announce is one zwlr_screencopy_frame_v1.buffer event.
type announce struct{ format, width, height, stride uint32 }

type compositorConfig struct {
	outputs           []outputSpec
	screencopyVersion uint32 // 0 selects 3
	noScreencopy      bool
	announce          []announce // empty: derived from the output or region
	flags             uint32
	hold              bool // do not answer copy until completePending
	failCapture       bool
	removeOnCapture   bool // global_remove of the output, then failed
	silent            bool // accept the connection and never answer
	failCopies        int  // answer this many copy requests with failed
	readyEarly        bool // send ready right after the buffer events, before any copy
	noShmOffer        bool // send only buffer_done: no wl_shm buffer is offered
	duplicateGlobal   bool // announce the first output global twice
}

type poolMap struct{ data []byte }

type bufferInfo struct {
	pool                          *poolMap
	offset                        int
	width, height, stride, format uint32
}

type compositor struct {
	t    testing.TB
	path string
	ln   net.Listener
	cfg  compositorConfig

	mu               sync.Mutex
	conn             *net.UnixConn
	pools            int
	buffers          int
	destroyedBuffers int
	captures         int
	copies           int
	fdsReceived      int
	lastRegion       [4]int32
	lastRegionSet    bool
	seq              uint32
	pending          struct {
		frame  uint32
		buffer *bufferInfo
	}
	hasPending bool
	copyEvent  chan struct{}
	done       chan struct{}

	wmu sync.Mutex
	out [4096]byte
	reg uint32
}

func startCompositor(t testing.TB, cfg compositorConfig) *compositor {
	t.Helper()
	if len(cfg.outputs) == 0 {
		cfg.outputs = []outputSpec{{name: "TEST-1", width: 8, height: 4, scale: 1, version: 4}}
	}
	if cfg.screencopyVersion == 0 {
		cfg.screencopyVersion = 3
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(dir, "wl-test"))
	if err != nil {
		t.Fatal(err)
	}
	c := &compositor{t: t, path: ln.Addr().String(), ln: ln, cfg: cfg, copyEvent: make(chan struct{}, 64), done: make(chan struct{})}
	go c.serve()
	t.Cleanup(func() {
		ln.Close()
		c.mu.Lock()
		if c.conn != nil {
			c.conn.Close()
		}
		c.mu.Unlock()
		<-c.done
	})
	return c
}

func (c *compositor) stat(f func(*compositor) int) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return f(c)
}

func (c *compositor) serve() {
	defer close(c.done)
	nc, err := c.ln.Accept()
	if err != nil {
		return
	}
	conn := nc.(*net.UnixConn)
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	defer conn.Close()
	if c.cfg.silent {
		buf := make([]byte, 64)
		for {
			if _, err := conn.Read(buf); err != nil {
				return
			}
		}
	}
	c.loop(conn)
}

type objects struct {
	kinds   map[uint32]int
	outputs map[uint32]int
	pools   map[uint32]*poolMap
	buffers map[uint32]*bufferInfo
	frames  map[uint32]*frameState
}

type frameState struct {
	output int
	w, h   uint32
}

func (c *compositor) loop(conn *net.UnixConn) {
	objs := &objects{
		kinds: map[uint32]int{1: kindDisplay}, outputs: map[uint32]int{}, pools: map[uint32]*poolMap{},
		buffers: map[uint32]*bufferInfo{}, frames: map[uint32]*frameState{},
	}
	buf := make([]byte, 0, 1<<16)
	chunk := make([]byte, 1<<15)
	oob := make([]byte, unix.CmsgSpace(4*16))
	var fds []int
	defer func() {
		for _, fd := range fds {
			unix.Close(fd)
		}
	}()
	for {
		n, oobn, _, _, err := conn.ReadMsgUnix(chunk, oob)
		if oobn > 0 {
			msgs, perr := syscall.ParseSocketControlMessage(oob[:oobn])
			if perr != nil {
				c.t.Errorf("control message: %v", perr)
				return
			}
			for i := range msgs {
				got, perr := syscall.ParseUnixRights(&msgs[i])
				if perr != nil {
					c.t.Errorf("rights: %v", perr)
					return
				}
				fds = append(fds, got...)
				c.mu.Lock()
				c.fdsReceived += len(got)
				c.mu.Unlock()
			}
		}
		buf = append(buf, chunk[:max(n, 0)]...)
		for len(buf) >= 8 {
			size := int(binary.LittleEndian.Uint32(buf[4:]) >> 16)
			if size < 8 || len(buf) < size {
				break
			}
			id := binary.LittleEndian.Uint32(buf)
			op := uint16(binary.LittleEndian.Uint32(buf[4:]))
			if !c.handle(objs, id, op, buf[8:size], &fds) {
				return
			}
			buf = buf[:copy(buf, buf[size:])]
		}
		if err != nil {
			return
		}
	}
}

func word(b []byte, i int) uint32 { return binary.LittleEndian.Uint32(b[i*4:]) }

func (c *compositor) handle(o *objects, id uint32, op uint16, body []byte, fds *[]int) bool {
	switch o.kinds[id] {
	case kindDisplay:
		switch op {
		case 0: // sync
			cb := word(body, 0)
			c.send(cb, 0, 0)
			c.send(1, 1, cb)
		case 1: // get_registry
			c.wmu.Lock()
			c.reg = word(body, 0)
			c.wmu.Unlock()
			o.kinds[word(body, 0)] = kindRegistry
			c.advertise()
		}
	case kindRegistry:
		name := word(body, 0)
		n := int(word(body, 1))
		iface := string(body[8 : 8+n-1])
		i := 8 + (n+3)&^3
		version, newID := word(body, i/4), word(body, i/4+1)
		c.bind(o, name, iface, version, newID)
	case kindOutput:
		if op == 0 {
			c.send(1, 1, id)
			delete(o.kinds, id)
		}
	case kindShm:
		if op == 0 {
			if len(*fds) == 0 {
				c.t.Error("create_pool without descriptor")
				return false
			}
			fd := (*fds)[0]
			*fds = (*fds)[1:]
			size := int(word(body, 1))
			data, err := unix.Mmap(fd, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
			unix.Close(fd)
			if err != nil {
				c.t.Errorf("server mmap: %v", err)
				return false
			}
			pid := word(body, 0)
			o.kinds[pid] = kindPool
			o.pools[pid] = &poolMap{data: data}
			c.mu.Lock()
			c.pools++
			c.mu.Unlock()
		}
	case kindPool:
		switch op {
		case 0:
			bid := word(body, 0)
			o.kinds[bid] = kindBuffer
			o.buffers[bid] = &bufferInfo{pool: o.pools[id], offset: int(word(body, 1)), width: word(body, 2), height: word(body, 3), stride: word(body, 4), format: word(body, 5)}
			c.mu.Lock()
			c.buffers++
			c.mu.Unlock()
		case 1:
			c.send(1, 1, id)
			delete(o.kinds, id)
		}
	case kindBuffer:
		if op == 0 {
			c.mu.Lock()
			c.destroyedBuffers++
			c.mu.Unlock()
			c.send(1, 1, id)
			delete(o.kinds, id)
			delete(o.buffers, id)
		}
	case kindScreencopy:
		if op <= 1 {
			c.capture(o, op, body)
		}
	case kindFrame:
		switch op {
		case 0:
			c.copy(o, id, o.buffers[word(body, 0)])
		case 1:
			c.send(1, 1, id)
			delete(o.kinds, id)
			delete(o.frames, id)
		}
	}
	return true
}

func (c *compositor) advertise() {
	for i, spec := range c.cfg.outputs {
		c.sendGlobal(uint32(i+1), "wl_output", spec.version)
	}
	if c.cfg.duplicateGlobal {
		c.sendGlobal(1, "wl_output", c.cfg.outputs[0].version)
	}
	c.sendGlobal(globalShm, "wl_shm", 1)
	if !c.cfg.noScreencopy {
		c.sendGlobal(globalScreencopy, "zwlr_screencopy_manager_v1", c.cfg.screencopyVersion)
	}
}

func (c *compositor) bind(o *objects, name uint32, iface string, version, id uint32) {
	switch iface {
	case "wl_output":
		o.kinds[id] = kindOutput
		idx := int(name) - 1
		o.outputs[id] = idx
		spec := c.cfg.outputs[idx]
		c.wmu.Lock()
		m := c.begin(id, 0)
		m = c.words(m, 0, 0, uint32(spec.width), uint32(spec.height), 0)
		m = c.str(m, "Test")
		m = c.str(m, "Model")
		m = c.words(m, 0)
		c.flush(m)
		c.wmu.Unlock()
		c.send(id, 1, 1, spec.width, spec.height, 60000)
		if version >= 2 {
			c.send(id, 3, uint32(spec.scale))
		}
		if version >= 4 {
			c.wmu.Lock()
			m := c.begin(id, 4)
			m = c.str(m, spec.name)
			c.flush(m)
			c.wmu.Unlock()
		}
		if version >= 2 {
			c.send(id, 2)
		}
	case "wl_shm":
		o.kinds[id] = kindShm
		c.send(id, 0, 0)
		c.send(id, 0, 1)
	case "zwlr_screencopy_manager_v1":
		o.kinds[id] = kindScreencopy
	default:
		c.t.Errorf("bind of unexpected interface %q", iface)
	}
}

func (c *compositor) capture(o *objects, op uint16, body []byte) {
	frame := word(body, 0)
	out := o.outputs[word(body, 2)]
	spec := c.cfg.outputs[out]
	w, h := spec.width, spec.height
	c.mu.Lock()
	c.captures++
	if op == 1 {
		c.lastRegion = [4]int32{int32(word(body, 3)), int32(word(body, 4)), int32(word(body, 5)), int32(word(body, 6))}
		c.lastRegionSet = true
		w, h = word(body, 5), word(body, 6)
	}
	c.mu.Unlock()
	o.kinds[frame] = kindFrame
	o.frames[frame] = &frameState{output: out, w: w, h: h}
	if c.cfg.removeOnCapture {
		c.removeOutput(uint32(out + 1))
		c.send(frame, 3)
		return
	}
	if c.cfg.failCapture {
		c.send(frame, 3)
		return
	}
	if c.cfg.readyEarly {
		c.send(frame, 2, 0, 1, 500)
		return
	}
	list := c.cfg.announce
	if c.cfg.noShmOffer {
		c.send(frame, 6)
		return
	}
	if len(list) == 0 {
		list = []announce{{format: 1, width: w, height: h, stride: w * 4}}
	}
	for _, a := range list {
		c.send(frame, 0, a.format, a.width, a.height, a.stride)
	}
	if c.cfg.screencopyVersion >= 3 {
		c.send(frame, 6)
	}
}

func (c *compositor) copy(o *objects, frame uint32, b *bufferInfo) {
	if b == nil {
		c.t.Error("copy with unknown buffer")
		return
	}
	c.mu.Lock()
	c.copies++
	if c.cfg.failCopies > 0 {
		c.cfg.failCopies--
		c.mu.Unlock()
		c.send(frame, 3)
		return
	}
	hold := c.cfg.hold
	if hold {
		c.pending.frame, c.pending.buffer, c.hasPending = frame, b, true
	}
	c.mu.Unlock()
	select {
	case c.copyEvent <- struct{}{}:
	default:
	}
	if !hold {
		c.finish(frame, b)
	}
}

// completePending answers a copy that was held.
func (c *compositor) completePending() {
	c.mu.Lock()
	frame, b, ok := c.pending.frame, c.pending.buffer, c.hasPending
	c.hasPending = false
	c.mu.Unlock()
	if !ok {
		c.t.Error("no pending copy")
		return
	}
	c.finish(frame, b)
}

// pixel is the deterministic content written for (x, y) of frame number seq.
func pixel(x, y int, seq uint32) [4]byte {
	return [4]byte{byte(x), byte(y), byte(seq), 0xff}
}

func (c *compositor) finish(frame uint32, b *bufferInfo) {
	c.mu.Lock()
	c.seq++
	seq := c.seq
	c.mu.Unlock()
	data := b.pool.data[b.offset:]
	for y := 0; y < int(b.height); y++ {
		row := data[y*int(b.stride) : (y+1)*int(b.stride)]
		for x := 0; x < int(b.width); x++ {
			p := pixel(x, y, seq)
			copy(row[x*4:], p[:])
		}
		for i := int(b.width) * 4; i < len(row); i++ {
			row[i] = 0xEE // stride padding
		}
	}
	c.send(frame, 1, c.cfg.flags)
	c.send(frame, 2, 0, 1, 500)
}

// removeOutput announces the removal of output global name (1-based).
func (c *compositor) removeOutput(name uint32) {
	c.wmu.Lock()
	m := c.begin(c.reg, 1)
	m = c.words(m, name)
	c.flush(m)
	c.wmu.Unlock()
}

// Encoding helpers. Every message is built in c.out under wmu.

func (c *compositor) send(obj uint32, op uint16, args ...uint32) {
	c.wmu.Lock()
	m := c.begin(obj, op)
	m = c.words(m, args...)
	c.flush(m)
	c.wmu.Unlock()
}

func (c *compositor) sendGlobal(name uint32, iface string, version uint32) {
	c.wmu.Lock()
	m := c.begin(c.reg, 0)
	m = c.words(m, name)
	m = c.str(m, iface)
	m = c.words(m, version)
	c.flush(m)
	c.wmu.Unlock()
}

func (c *compositor) begin(obj uint32, op uint16) []byte {
	m := c.out[:8]
	binary.LittleEndian.PutUint32(m, obj)
	binary.LittleEndian.PutUint32(m[4:], uint32(op))
	return m
}

func (c *compositor) words(m []byte, args ...uint32) []byte {
	for _, a := range args {
		m = binary.LittleEndian.AppendUint32(m, a)
	}
	return m
}

func (c *compositor) str(m []byte, s string) []byte {
	m = binary.LittleEndian.AppendUint32(m, uint32(len(s)+1))
	m = append(m, s...)
	m = append(m, 0)
	return append(m, bytes.Repeat([]byte{0}, (4-(len(s)+1)%4)%4)...)
}

func (c *compositor) flush(m []byte) {
	binary.LittleEndian.PutUint32(m[4:], uint32(len(m))<<16|binary.LittleEndian.Uint32(m[4:])&0xffff)
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if _, err := conn.Write(m); err != nil {
		// The client may have gone away during teardown.
		return
	}
}
