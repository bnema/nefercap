package wayland

import (
	"bytes"
	"encoding/binary"
	"net"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

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
	kindPrivManager
	kindPrivSession
	kindPrivLayer
	kindCompositor
	kindSurface
)

const (
	globalShm        = 1000
	globalScreencopy = 1001
	globalPrivate    = 1002
	globalWlComp     = 1003
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

	private privateConfig
}

// workspaceSpec is one private-protocol workspace advertisement.
type workspaceSpec struct {
	id     uint64
	output string
	name   string
	region [4]int32
	active bool
}

// privateConfig configures the private capture protocol of the peer. The wire
// layout is the client's provisional one, see capture_session.go.
type privateConfig struct {
	enabled         bool
	token           string // empty selects a default
	workspaces      []workspaceSpec
	refuse          bool          // answer begin_session with stopped
	inactive        bool          // send an inactive state before the active one
	stopOnPing      bool          // answer ping with stopped
	swapToken       bool          // send a second state carrying a different token
	badRevision     bool          // send a state whose revision goes back
	zeroRevision    bool          // send a state with revision 0
	pausedStart     bool          // the first state is paused (active 0) and no active follows
	attachFail      int           // non-zero: answer attach_surface with attach_failed(value-1)
	attachSilent    bool          // never answer attach_surface
	attachDelay     time.Duration // delay before attached
	attachScript    [][2]uint32   // layer events (opcode, argument) sent instead of attached
	moveEachCapture bool          // push a new region with every capture request
	attachError     bool          // answer attach_surface with wl_display.error(code 2)
	stateRegion     *[4]int32
}

func (p privateConfig) tok() string {
	if p.token == "" {
		return testToken
	}
	return p.token
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

	// private protocol observations
	privSessions  int
	privPings     int
	privDestroyed int
	privRecord    uint32
	privRequested [4]int32
	privWorkspace uint64
	privAttached  []string
	privSessionID uint32
	privRev       uint64
	privLayers    []uint32
	privManager   uint32
	privLayerGone int
	copyEvent     chan struct{}
	done          chan struct{}

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
	case kindPrivManager:
		c.handlePrivManager(o, id, op, body)
	case kindPrivSession:
		c.handlePrivSession(o, id, op)
	case kindPrivLayer:
		if op == 0 { // destroy
			c.mu.Lock()
			c.privLayerGone++
			c.mu.Unlock()
			c.send(1, 1, id)
			delete(o.kinds, id)
		}
	case kindCompositor:
		if op == 0 {
			o.kinds[word(body, 0)] = kindSurface
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
	if c.cfg.private.enabled {
		c.sendGlobal(globalPrivate, captureManagerIface, 1)
		c.sendGlobal(globalWlComp, "wl_compositor", 4)
	}
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
	case "wl_compositor":
		o.kinds[id] = kindCompositor
	case captureManagerIface:
		o.kinds[id] = kindPrivManager
		c.mu.Lock()
		c.privManager = id
		c.mu.Unlock()
		for _, w := range c.cfg.private.workspaces {
			c.sendWorkspace(id, w)
		}
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
	// cfg fields may be changed by tests between captures: read them under mu.
	removeOnCapture, failCapture := c.cfg.removeOnCapture, c.cfg.failCapture
	readyEarly, noShmOffer := c.cfg.readyEarly, c.cfg.noShmOffer
	list, version := c.cfg.announce, c.cfg.screencopyVersion
	c.captures++
	move, moveWS := c.cfg.private.moveEachCapture, c.privWorkspace
	moveN := int32(c.captures)
	if op == 1 {
		c.lastRegion = [4]int32{int32(word(body, 3)), int32(word(body, 4)), int32(word(body, 5)), int32(word(body, 6))}
		c.lastRegionSet = true
		w, h = word(body, 5), word(body, 6)
	}
	c.mu.Unlock()
	if move {
		c.pushState([4]int32{moveN, 0, 8, 4}, moveWS)
	}
	o.kinds[frame] = kindFrame
	o.frames[frame] = &frameState{output: out, w: w, h: h}
	if removeOnCapture {
		c.removeOutput(uint32(out + 1))
		c.send(frame, 3)
		return
	}
	if failCapture {
		c.send(frame, 3)
		return
	}
	if readyEarly {
		c.send(frame, 2, 0, 1, 500)
		return
	}
	if noShmOffer {
		c.send(frame, 6)
		return
	}
	if len(list) == 0 {
		list = []announce{{format: 1, width: w, height: h, stride: w * 4}}
	}
	for _, a := range list {
		c.send(frame, 0, a.format, a.width, a.height, a.stride)
	}
	if version >= 3 {
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

// Private capture protocol peer (provisional layout, see capture_session.go).

func (c *compositor) sendWorkspace(mgr uint32, w workspaceSpec) {
	c.wmu.Lock()
	m := c.begin(mgr, 0)
	m = c.words(m, uint32(w.id>>32), uint32(w.id))
	m = c.str(m, w.output)
	m = c.str(m, w.name)
	m = c.words(m, uint32(w.region[0]), uint32(w.region[1]), uint32(w.region[2]), uint32(w.region[3]))
	act := uint32(0)
	if w.active {
		act = 1
	}
	m = c.words(m, act)
	c.flush(m)
	c.wmu.Unlock()
}

func (c *compositor) nextRevision() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.privRev++
	return c.privRev
}

func (c *compositor) sendState(session uint32, token string, active bool, r [4]int32, ws uint64) {
	rev := c.nextRevision()
	if c.cfg.private.zeroRevision {
		rev = 0
	}
	if c.cfg.private.badRevision {
		rev = 10 - rev // 9, 8: the revision goes back
	}
	c.wmu.Lock()
	m := c.begin(session, 0)
	m = c.str(m, token)
	act := uint32(0)
	if active {
		act = 1
	}
	m = c.words(m, act, uint32(r[0]), uint32(r[1]), uint32(r[2]), uint32(r[3]), uint32(ws>>32), uint32(ws), uint32(rev>>32), uint32(rev))
	c.flush(m)
	c.wmu.Unlock()
}

// pushState sends a state event on the current session, as a live geometry
// change would.
func (c *compositor) pushState(r [4]int32, ws uint64) {
	c.mu.Lock()
	id := c.privSessionID
	c.mu.Unlock()
	c.sendState(id, c.cfg.private.tok(), true, r, ws)
}

func (c *compositor) stopSession(reason uint32) {
	c.mu.Lock()
	id := c.privSessionID
	c.mu.Unlock()
	c.send(id, 1, reason)
}

func (c *compositor) handlePrivManager(o *objects, mgrID uint32, op uint16, body []byte) {
	switch op {
	case 1: // begin_session
		session := word(body, 0)
		out := c.cfg.outputs[o.outputs[word(body, 1)]]
		r := [4]int32{int32(word(body, 2)), int32(word(body, 3)), int32(word(body, 4)), int32(word(body, 5))}
		ws := uint64(word(body, 6))<<32 | uint64(word(body, 7))
		o.kinds[session] = kindPrivSession
		c.mu.Lock()
		c.privSessions++
		c.privSessionID = session
		c.privRecord = word(body, 8)
		c.privRequested = r
		c.privWorkspace = ws
		p := c.cfg.private
		c.mu.Unlock()
		if p.refuse {
			c.send(session, 1, 6) // busy
			return
		}
		if r == [4]int32{} {
			r = [4]int32{0, 0, int32(out.width), int32(out.height)}
		}
		for _, w := range c.cfg.private.workspaces {
			if w.id == ws && ws != 0 {
				r = w.region // a workspace target is its whole frame
			}
		}
		if p.stateRegion != nil {
			r = *p.stateRegion
		}
		if p.inactive || p.pausedStart {
			c.sendState(session, p.tok(), false, r, ws)
		}
		if !p.pausedStart {
			c.sendState(session, p.tok(), true, r, ws)
		}
		if p.swapToken {
			c.sendState(session, p.tok()+"x", true, r, ws)
		}
	case 2: // attach_surface(new_id layer, token, surface)
		layer := word(body, 0)
		n := int(word(body, 1))
		tok := string(body[8 : 8+n-1])
		o.kinds[layer] = kindPrivLayer
		c.mu.Lock()
		c.privAttached = append(c.privAttached, tok)
		c.privLayers = append(c.privLayers, layer)
		p := c.cfg.private
		c.mu.Unlock()
		switch {
		case p.attachSilent:
		case p.attachError:
			c.wmu.Lock()
			m := c.begin(1, 0)
			m = c.words(m, mgrID, 2)
			m = c.str(m, "surface_role")
			c.flush(m)
			c.wmu.Unlock()
		case p.attachScript != nil:
			for _, ev := range p.attachScript {
				if ev[0] == 0 {
					c.send(layer, 0)
				} else {
					c.send(layer, uint16(ev[0]), ev[1])
				}
			}
		case p.attachFail != 0:
			c.send(layer, 1, uint32(p.attachFail-1))
		default:
			time.Sleep(p.attachDelay)
			c.send(layer, 0)
		}
	case 0: // destroy
		c.send(1, 1, mgrID)
		delete(o.kinds, mgrID)
	}
}

func (c *compositor) handlePrivSession(o *objects, id uint32, op uint16) {
	switch op {
	case 0:
		c.mu.Lock()
		c.privDestroyed++
		c.mu.Unlock()
		c.send(1, 1, id)
		delete(o.kinds, id)
	case 1:
		c.mu.Lock()
		c.privPings++
		stop := c.cfg.private.stopOnPing
		c.mu.Unlock()
		if stop {
			c.send(id, 1, 2)
		}
	}
}

func (c *compositor) privAttachedCopy() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.privAttached...)
}

type privSnap struct {
	requested  [4]int32
	record     uint32
	workspace  uint64
	lastRegion [4]int32
}

func (c *compositor) snap() privSnap {
	c.mu.Lock()
	defer c.mu.Unlock()
	return privSnap{c.privRequested, c.privRecord, c.privWorkspace, c.lastRegion}
}

// detachLayer sends neferwl_capture_layer_v1.detached on the n-th attachment.
func (c *compositor) detachLayer(n int, reason uint32) {
	c.mu.Lock()
	id := c.privLayers[n]
	c.mu.Unlock()
	c.send(id, 2, reason)
}

func (c *compositor) announceWorkspace(w workspaceSpec) {
	c.mu.Lock()
	id := c.privManager
	c.mu.Unlock()
	c.sendWorkspace(id, w)
}

func (c *compositor) removeWorkspace(id uint64) {
	c.mu.Lock()
	m := c.privManager
	c.mu.Unlock()
	c.send(m, 1, uint32(id>>32), uint32(id))
}
