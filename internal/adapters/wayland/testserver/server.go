// Package testserver is a minimal in-process Wayland compositor for tests: a
// real libwayland server (purego-libwayland) that speaks wl_output, wl_shm,
// ext-image-capture-source-v1, ext-image-copy-capture-v1, ext-workspace-v1 and
// NeferWL's capture extension. It is a protocol peer, not a substitute for a
// project type: clients connect to its socket like to any compositor.
//
// All server state is touched only on the display goroutine: handlers run
// there, and tests reach it through Do.
package testserver

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bnema/purego-libwayland/protocol/extimagecapturesource"
	"github.com/bnema/purego-libwayland/protocol/extimagecopycapture"
	"github.com/bnema/purego-libwayland/protocol/extworkspace"
	"github.com/bnema/purego-libwayland/protocol/wayland"
	"github.com/bnema/purego-libwayland/server"
	"golang.org/x/sys/unix"

	"github.com/bnema/nefercap/internal/adapters/wayland/testserver/neferwl"
)

// Token is the exclusion token the server hands out.
const Token = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// wl_shm formats.
const (
	ARGB8888 = 0
	XRGB8888 = 1
)

// OutputSpec describes one wl_output. The capture buffer of an output source
// is its mode size.
type OutputSpec struct {
	Name          string
	Width, Height uint32
	Scale         int32
	Version       uint32 // 0 selects 4
	// LogicalWidth/Height, when set, are announced through xdg-output.
	LogicalWidth, LogicalHeight int32
}

// WorkspaceSpec describes one ext-workspace handle; all of them live in one
// group that holds every output.
type WorkspaceSpec struct {
	ID     string // empty: a temporary workspace, no id event
	Name   string
	Output string // the output whose buffer size a workspace source has
	Active bool
	Hidden bool
	// Frame is the output-local logical rectangle (x, y, width, height) that
	// neferwl_workspace_frame_v1 reports; zero reports nothing.
	Frame [4]int32
}

// Config selects what the server offers.
type Config struct {
	Outputs []OutputSpec // default: one 8x4 output "TEST-1"
	// NoCopyCapture leaves out ext-image-copy-capture and the output source.
	NoCopyCapture bool
	Formats       []uint32 // shm formats announced, default XRGB8888
	// NoShmFormat announces size and done but no shm_format.
	NoShmFormat bool
	// Hold keeps every captured frame pending until Release.
	Hold bool
	// HoldAfterFirst answers the first frame of each session at once and holds
	// the following ones until Release, like a compositor that waits for
	// damage on a static screen.
	HoldAfterFirst bool
	// Workspaces enables ext-workspace-v1 when non-nil (possibly empty).
	Workspaces    []WorkspaceSpec
	HasWorkspaces bool
	// NeferwlSource and NeferwlExclusion enable the extension's two globals.
	NeferwlSource    bool
	NeferwlExclusion bool
	// ExclusionFail answers get_exclusion with failed(ExclusionFailReason-1)
	// when non-zero.
	ExclusionFail uint32
	// AttachFail answers attach_surface with failed(AttachFail-1) when non-zero.
	AttachFail uint32
	// AttachSilent never answers attach_surface.
	AttachSilent bool
	// StopAtCreate answers every create_session with stopped and no
	// constraints, like a compositor that refuses the client.
	StopAtCreate bool
	// Transform is reported by every ready frame (wl_output.transform).
	Transform uint32
	// FailFrames answers that many capture requests with failed(FailReason).
	FailFrames int
	FailReason uint32
}

// Stats are counters observed by tests.
type Stats struct {
	Sessions, Frames, Captures, Ready int
	DestroyedSessions                 int
	Buffers, DestroyedBuffers         int
	LastSource                        string // "output", "region", "workspace"
	LastRegion                        [4]int32
	Exclusions                        int
	Attached                          []string
	DestroyedExclusions               int
	WorkspaceFrames                   int // neferwl_workspace_frame_v1 objects created
	DestroyedWorkspaceFrames          int
	DestroyedXdgOutputs               int
}

// Server is a running test compositor.
type Server struct {
	// Path is the socket path for wayland.New.
	Path string

	d   *server.Display
	cfg Config

	outputs    []*output
	bufs       map[*server.Resource]*buffer
	sources    map[*server.Resource]*source
	sessions   []*session
	workspaces []*workspace
	wsMgrs     []*wsManager
	layers     []*server.Resource
	frames     []*frameObject
	stats      Stats
	held       []*frame
	seq        uint32
}

type output struct {
	spec    OutputSpec
	global  *server.Global
	res     []*wayland.Output
	removed bool
}

// Start runs a compositor on a fresh socket until the test ends.
func Start(t testing.TB, cfg Config) *Server {
	t.Helper()
	if len(cfg.Outputs) == 0 {
		cfg.Outputs = []OutputSpec{{Name: "TEST-1", Width: 8, Height: 4, Scale: 1}}
	}
	if len(cfg.Formats) == 0 {
		cfg.Formats = []uint32{XRGB8888}
	}
	d, err := server.NewDisplay()
	if err != nil {
		// NEFERCAP_REQUIRE_WAYLAND=1 (set by make test and make race) turns a
		// missing libwayland-server into a failure, so the suite cannot pass
		// without its protocol tests.
		if os.Getenv("NEFERCAP_REQUIRE_WAYLAND") == "1" {
			t.Fatalf("libwayland-server unavailable: %v", err)
		}
		t.Skipf("libwayland-server unavailable: %v", err)
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "wl-test")
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Bind(fd, &unix.SockaddrUnix{Name: path}); err != nil {
		t.Fatal(err)
	}
	if err := unix.Listen(fd, 16); err != nil {
		t.Fatal(err)
	}
	if err := d.AddSocketFD(fd); err != nil { // libwayland owns fd now
		t.Fatal(err)
	}
	s := &Server{Path: path, d: d, cfg: cfg, bufs: map[*server.Resource]*buffer{}, sources: map[*server.Resource]*source{}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	if !d.Do(func() { s.setup(t) }) {
		t.Fatal("test compositor stopped")
	}
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return s
}

// Do runs f on the display goroutine, where server state may be touched.
func (s *Server) Do(f func()) {
	if !s.d.Do(f) {
		// stopped: nothing to observe
		return
	}
}

// Stats returns a snapshot of the counters.
func (s *Server) Stats() Stats {
	var st Stats
	s.Do(func() {
		st = s.stats
		st.Attached = append([]string(nil), s.stats.Attached...)
	})
	return st
}

func (s *Server) setup(t testing.TB) {
	must := func(err error) {
		if err != nil {
			t.Error(err)
		}
	}
	for _, spec := range s.cfg.Outputs {
		s.addOutput(spec, must)
	}
	must(wayland.NewShmGlobal(s.d, 1, func(c server.Client, v, id uint32) {
		shm, err := wayland.NewShm(c, int32(v), id, shmHandler{s})
		if err != nil {
			t.Error(err)
			return
		}
		for _, f := range []uint32{ARGB8888, XRGB8888} {
			shm.SendFormat(f)
		}
	}))
	if s.xdgOutputWanted() {
		s.addXdgOutput(must)
	}
	must(wayland.NewCompositorGlobal(s.d, 4, func(c server.Client, v, id uint32) {
		if _, err := wayland.NewCompositor(c, int32(v), id, compositorHandler{}); err != nil {
			t.Error(err)
		}
	}))
	if !s.cfg.NoCopyCapture {
		must(extimagecapturesource.NewExtOutputImageCaptureSourceManagerV1Global(s.d, 1, func(c server.Client, v, id uint32) {
			if _, err := extimagecapturesource.NewExtOutputImageCaptureSourceManagerV1(c, int32(v), id, outputSourceManager{s}); err != nil {
				t.Error(err)
			}
		}))
		must(extimagecopycapture.NewExtImageCopyCaptureManagerV1Global(s.d, 1, func(c server.Client, v, id uint32) {
			if _, err := extimagecopycapture.NewExtImageCopyCaptureManagerV1(c, int32(v), id, copyManager{s}); err != nil {
				t.Error(err)
			}
		}))
	}
	if s.cfg.HasWorkspaces {
		for _, w := range s.cfg.Workspaces {
			s.workspaces = append(s.workspaces, &workspace{spec: w})
		}
		must(extworkspace.NewExtWorkspaceManagerV1Global(s.d, 1, func(c server.Client, v, id uint32) {
			m := &wsManager{s: s}
			res, err := extworkspace.NewExtWorkspaceManagerV1(c, int32(v), id, m)
			if err != nil {
				t.Error(err)
				return
			}
			m.res = res
			s.wsMgrs = append(s.wsMgrs, m)
			m.announce(c)
		}))
	}
	if s.cfg.NeferwlSource {
		must(neferwl.NewNeferwlImageCaptureSourceManagerV1Global(s.d, 1, func(c server.Client, v, id uint32) {
			if _, err := neferwl.NewNeferwlImageCaptureSourceManagerV1(c, int32(v), id, neferwlSources{s}); err != nil {
				t.Error(err)
			}
		}))
	}
	if s.cfg.NeferwlExclusion {
		must(neferwl.NewNeferwlCaptureExclusionManagerV1Global(s.d, 1, func(c server.Client, v, id uint32) {
			if _, err := neferwl.NewNeferwlCaptureExclusionManagerV1(c, int32(v), id, exclusionManager{s}); err != nil {
				t.Error(err)
			}
		}))
	}
}

type compositorHandler struct{}

func (compositorHandler) CreateSurface(self *wayland.Compositor, id uint32) {
	_, _ = wayland.NewSurface(self.Client(), 4, id, surfaceHandler{})
}
func (compositorHandler) CreateRegion(*wayland.Compositor, uint32) {}
func (compositorHandler) Release(*wayland.Compositor)              {}

type surfaceHandler struct{}

func (surfaceHandler) Destroy(*wayland.Surface)                                  {}
func (surfaceHandler) Attach(*wayland.Surface, *wayland.Buffer, int32, int32)    {}
func (surfaceHandler) Damage(*wayland.Surface, int32, int32, int32, int32)       {}
func (surfaceHandler) Frame(*wayland.Surface, uint32)                            {}
func (surfaceHandler) SetOpaqueRegion(*wayland.Surface, *wayland.Region)         {}
func (surfaceHandler) SetInputRegion(*wayland.Surface, *wayland.Region)          {}
func (surfaceHandler) Commit(*wayland.Surface)                                   {}
func (surfaceHandler) SetBufferTransform(*wayland.Surface, int32)                {}
func (surfaceHandler) SetBufferScale(*wayland.Surface, int32)                    {}
func (surfaceHandler) DamageBuffer(*wayland.Surface, int32, int32, int32, int32) {}
func (surfaceHandler) Offset(*wayland.Surface, int32, int32)                     {}
func (surfaceHandler) GetRelease(*wayland.Surface, uint32)                       {}
