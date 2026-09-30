package control

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

const helperEnv = "NEFERCAP_CONTROL_TEST_HELPER"

// TestMain doubles as a helper process: with helperEnv set, the test binary
// runs a real recording owner instead of the tests.
func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "":
		os.Exit(m.Run())
	case "owner":
		os.Exit(helperOwner())
	}
	os.Exit(64)
}

func helperOwner() int {
	s, err := Listen()
	if errors.Is(err, ErrAlreadyRecording) {
		return 3
	}
	if err != nil {
		return 4
	}
	os.Stdout.WriteString("ready\n")
	if os.Getenv(helperEnv+"_HANG") != "" {
		select {} // killed by the test
	}
	<-s.Requests()
	if s.Close() != nil {
		return 5
	}
	return 0
}

// setup points XDG_RUNTIME_DIR at a fresh private temp directory.
func setup(t *testing.T) string {
	t.Helper()
	rd := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", rd)
	return rd
}

func listenT(t *testing.T) *Server {
	t.Helper()
	s, err := Listen()
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

func sockPath(rd string) string { return filepath.Join(rd, dirName, sockName) }

func dial(t *testing.T, rd string) *net.UnixConn {
	t.Helper()
	c, err := net.Dial("unix", sockPath(rd))
	require.NoError(t, err)
	t.Cleanup(func() { c.Close() })
	return c.(*net.UnixConn)
}

func startHelper(t *testing.T, extra ...string) (*exec.Cmd, io.ReadCloser) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), append([]string{helperEnv + "=owner"}, extra...)...)
	out, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	return cmd, out
}

func waitReady(t *testing.T, out io.Reader) {
	t.Helper()
	buf := make([]byte, len("ready\n"))
	_, err := io.ReadFull(out, buf)
	require.NoError(t, err)
	require.Equal(t, "ready\n", string(buf))
}

func TestStopAndStatusRoundTrip(t *testing.T) {
	setup(t)
	s := listenT(t)

	ok, err := Query(ctx(t))
	require.NoError(t, err)
	assert.True(t, ok)
	select {
	case r := <-s.Requests():
		t.Fatalf("status was delivered as %v", r)
	default:
	}

	ok, err = Stop(ctx(t))
	require.NoError(t, err)
	assert.True(t, ok)
	select {
	case r := <-s.Requests():
		assert.Equal(t, RequestStop, r)
	case <-time.After(5 * time.Second):
		t.Fatal("stop request not delivered")
	}
}

func TestNoServer(t *testing.T) {
	rd := setup(t)
	ok, err := Stop(ctx(t))
	assert.NoError(t, err)
	assert.False(t, ok)
	ok, err = Query(ctx(t))
	assert.NoError(t, err)
	assert.False(t, ok)
	// Clients never create anything.
	_, err = os.Lstat(filepath.Join(rd, dirName))
	assert.True(t, os.IsNotExist(err))
}

func TestRuntimeDirValidation(t *testing.T) {
	for _, v := range []string{"", "relative/dir", strings.Repeat("/a", maxPath)} {
		t.Setenv("XDG_RUNTIME_DIR", v)
		_, err := Listen()
		assert.ErrorIs(t, err, ErrNoRuntimeDir, "%q", v)
		_, err = Stop(ctx(t))
		assert.ErrorIs(t, err, ErrNoRuntimeDir)
		_, err = Query(ctx(t))
		assert.ErrorIs(t, err, ErrNoRuntimeDir)
	}
}

func TestListenRejectsRawRuntimeDir(t *testing.T) {
	for _, v := range []string{"", "relative", "/tmp/a\x00b"} {
		_, err := listen(v, uint32(os.Geteuid()))
		assert.ErrorIs(t, err, ErrNoRuntimeDir, "%q", v)
	}
}

func TestPermissions(t *testing.T) {
	rd := setup(t)
	old := unix.Umask(0o000)
	defer unix.Umask(old)
	listenT(t)

	for path, mode := range map[string]os.FileMode{
		filepath.Join(rd, dirName):           0o700,
		sockPath(rd):                         0o600,
		filepath.Join(rd, dirName, lockName): 0o600,
	} {
		fi, err := os.Lstat(path)
		require.NoError(t, err)
		assert.Equal(t, mode, fi.Mode().Perm(), filepath.Base(path))
	}
	fi, err := os.Lstat(sockPath(rd))
	require.NoError(t, err)
	assert.NotZero(t, fi.Mode()&os.ModeSocket)
}

func TestSingleInstance(t *testing.T) {
	rd := setup(t)
	first := listenT(t)

	second, err := Listen()
	require.ErrorIs(t, err, ErrAlreadyRecording)
	assert.Nil(t, second)

	// The failed attempt must not have disturbed the live socket.
	ok, err := Query(ctx(t))
	require.NoError(t, err)
	assert.True(t, ok)

	require.NoError(t, first.Close())
	_, err = os.Lstat(sockPath(rd))
	assert.True(t, os.IsNotExist(err))

	third, err := Listen()
	require.NoError(t, err)
	require.NoError(t, third.Close())
}

func TestSingleInstanceAcrossProcesses(t *testing.T) {
	rd := setup(t)
	cmd, out := startHelper(t)
	waitReady(t, out)

	// The other process holds the lock, so we neither serve nor unlink.
	_, err := Listen()
	require.ErrorIs(t, err, ErrAlreadyRecording)
	_, err = os.Lstat(sockPath(rd))
	require.NoError(t, err)

	second := exec.Command(os.Args[0], "-test.run=^$")
	second.Env = append(os.Environ(), helperEnv+"=owner")
	err = second.Run()
	var ee *exec.ExitError
	require.ErrorAs(t, err, &ee)
	assert.Equal(t, 3, ee.ExitCode())

	ok, err := Stop(ctx(t))
	require.NoError(t, err)
	assert.True(t, ok)
	require.NoError(t, cmd.Wait())
	_, err = os.Lstat(sockPath(rd))
	assert.True(t, os.IsNotExist(err), "owner removes its socket on close")
}

func TestStaleSocketAfterKilledOwner(t *testing.T) {
	rd := setup(t)
	cmd, out := startHelper(t, helperEnv+"_HANG=1")
	waitReady(t, out)
	require.NoError(t, cmd.Process.Kill())
	_ = cmd.Wait()

	_, err := os.Lstat(sockPath(rd))
	require.NoError(t, err, "killed owner leaves a stale socket")

	// Clients report "not recording" and leave the stale inode alone.
	ok, err := Query(ctx(t))
	assert.NoError(t, err)
	assert.False(t, ok)
	ok, err = Stop(ctx(t))
	assert.NoError(t, err)
	assert.False(t, ok)
	_, err = os.Lstat(sockPath(rd))
	require.NoError(t, err, "clients must not unlink")

	// The next lock holder replaces it.
	s := listenT(t)
	ok, err = Query(ctx(t))
	require.NoError(t, err)
	assert.True(t, ok)
	require.NoError(t, s.Close())
}

func TestStaleLeftoverWithoutLockedOwner(t *testing.T) {
	rd := setup(t)
	s, err := Listen()
	require.NoError(t, err)
	// Simulate a crash: drop the lock but leave the socket inode behind.
	require.NoError(t, unix.Close(s.lockFD))
	s.lockFD = -1
	require.NoError(t, s.ln.Close())
	_, err = os.Lstat(sockPath(rd))
	require.NoError(t, err)

	s2 := listenT(t)
	ok, err := Query(ctx(t))
	require.NoError(t, err)
	assert.True(t, ok)
	require.NoError(t, s2.Close())
}

func TestListenRefusesForeignObjects(t *testing.T) {
	t.Run("non-socket at socket path", func(t *testing.T) {
		rd := setup(t)
		require.NoError(t, os.Mkdir(filepath.Join(rd, dirName), 0o700))
		require.NoError(t, os.WriteFile(sockPath(rd), []byte("keep"), 0o600))
		_, err := Listen()
		require.ErrorIs(t, err, ErrInsecure)
		b, err := os.ReadFile(sockPath(rd))
		require.NoError(t, err)
		assert.Equal(t, "keep", string(b))
	})
	t.Run("symlink at socket path", func(t *testing.T) {
		rd := setup(t)
		require.NoError(t, os.Mkdir(filepath.Join(rd, dirName), 0o700))
		target := filepath.Join(rd, "target")
		require.NoError(t, os.WriteFile(target, []byte("keep"), 0o600))
		require.NoError(t, os.Symlink(target, sockPath(rd)))
		_, err := Listen()
		require.ErrorIs(t, err, ErrInsecure)
		_, err = os.Stat(target)
		assert.NoError(t, err)
	})
	t.Run("symlinked lock file", func(t *testing.T) {
		rd := setup(t)
		require.NoError(t, os.Mkdir(filepath.Join(rd, dirName), 0o700))
		target := filepath.Join(rd, "target")
		require.NoError(t, os.WriteFile(target, nil, 0o644))
		require.NoError(t, os.Symlink(target, filepath.Join(rd, dirName, lockName)))
		_, err := Listen()
		require.Error(t, err)
		fi, err := os.Stat(target)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o644), fi.Mode().Perm(), "target untouched")
	})
}

func TestDirectoryChecks(t *testing.T) {
	t.Run("loose mode", func(t *testing.T) {
		rd := setup(t)
		d := filepath.Join(rd, dirName)
		require.NoError(t, os.Mkdir(d, 0o700))
		require.NoError(t, os.Chmod(d, 0o755))
		_, err := Listen()
		assert.ErrorIs(t, err, ErrInsecure)
		_, err = Stop(ctx(t))
		assert.ErrorIs(t, err, ErrInsecure)
		_, err = Query(ctx(t))
		assert.ErrorIs(t, err, ErrInsecure)
	})
	t.Run("symlink", func(t *testing.T) {
		rd := setup(t)
		real := filepath.Join(rd, "elsewhere")
		require.NoError(t, os.Mkdir(real, 0o700))
		require.NoError(t, os.Symlink(real, filepath.Join(rd, dirName)))
		_, err := Listen()
		assert.ErrorIs(t, err, ErrInsecure)
		_, err = Stop(ctx(t))
		assert.ErrorIs(t, err, ErrInsecure)
		entries, err := os.ReadDir(real)
		require.NoError(t, err)
		assert.Empty(t, entries)
	})
	t.Run("regular file", func(t *testing.T) {
		rd := setup(t)
		require.NoError(t, os.WriteFile(filepath.Join(rd, dirName), nil, 0o600))
		_, err := Listen()
		assert.ErrorIs(t, err, ErrInsecure)
	})
	t.Run("other owner", func(t *testing.T) {
		rd := setup(t)
		require.NoError(t, os.Mkdir(filepath.Join(rd, dirName), 0o700))
		fd, err := openSecureDir(rd, false, uint32(os.Geteuid())+1)
		if err == nil {
			unix.Close(fd)
		}
		assert.ErrorIs(t, err, ErrInsecure)
	})
}

func TestClientRejectsLooseSocket(t *testing.T) {
	rd := setup(t)
	listenT(t)
	require.NoError(t, os.Chmod(sockPath(rd), 0o666))
	_, err := Stop(ctx(t))
	assert.ErrorIs(t, err, ErrInsecure)
}

func TestForeignPeerRejected(t *testing.T) {
	setup(t)
	s, err := listen(os.Getenv("XDG_RUNTIME_DIR"), uint32(os.Geteuid())+1)
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })

	ok, err := Stop(ctx(t))
	assert.Error(t, err)
	assert.False(t, ok)
	select {
	case <-s.Requests():
		t.Fatal("foreign peer delivered a request")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestRequestLimitsAndProtocol(t *testing.T) {
	rd := setup(t)
	s := listenT(t)

	expectClosedSilently := func(t *testing.T, payload string) {
		t.Helper()
		c := dial(t, rd)
		_, _ = c.Write([]byte(payload))
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		b, err := io.ReadAll(c)
		if err != nil {
			// Unread request bytes make close report a reset; still no reply.
			require.ErrorIs(t, err, unix.ECONNRESET)
		}
		assert.Empty(t, b)
	}
	t.Run("oversized without newline", func(t *testing.T) {
		expectClosedSilently(t, strings.Repeat("x", 4096))
	})
	t.Run("oversized with newline late", func(t *testing.T) {
		expectClosedSilently(t, strings.Repeat("x", maxRequest)+"stop\n")
	})
	t.Run("unknown", func(t *testing.T) { expectClosedSilently(t, "quit\n") })
	t.Run("case sensitive", func(t *testing.T) { expectClosedSilently(t, "STOP\n") })
	t.Run("crlf", func(t *testing.T) { expectClosedSilently(t, "stop\r\n") })
	t.Run("stop in oversized junk", func(t *testing.T) {
		expectClosedSilently(t, "status\nstop\n"+strings.Repeat("x", 100))
	})

	select {
	case r := <-s.Requests():
		t.Fatalf("invalid request delivered: %v", r)
	default:
	}

	// Replies are exactly the two fixed lines.
	c := dial(t, rd)
	_, err := c.Write([]byte("status\n"))
	require.NoError(t, err)
	b, err := io.ReadAll(c)
	require.NoError(t, err)
	assert.Equal(t, "recording\n", string(b))
}

func TestDeadlineAndConnectionBound(t *testing.T) {
	rd := setup(t)
	listenT(t)

	// Fill every slot with silent clients.
	start := time.Now()
	var idle []*net.UnixConn
	for i := 0; i < connLimit; i++ {
		idle = append(idle, dial(t, rd))
	}
	// While all slots are taken nothing is answered.
	short, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	ok, err := Query(short)
	cancel()
	assert.Error(t, err)
	assert.False(t, ok)

	// The server drops the silent clients at its 1s deadline.
	for _, c := range idle {
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		b, err := io.ReadAll(c)
		require.NoError(t, err, "server closes timed-out clients")
		assert.Empty(t, b)
	}
	assert.GreaterOrEqual(t, time.Since(start), ioTimeout-100*time.Millisecond)
	assert.Less(t, time.Since(start), 3*time.Second)

	// Slots are free again.
	ok, err = Query(ctx(t))
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestClientContext(t *testing.T) {
	rd := setup(t)
	listenT(t)
	// Occupy every slot so the client's request stays unanswered.
	for i := 0; i < connLimit; i++ {
		dial(t, rd)
	}
	c, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	ok, err := Query(c)
	assert.Error(t, err)
	assert.False(t, ok)
	assert.Less(t, time.Since(start), 900*time.Millisecond)

	cancelled, cancel2 := context.WithCancel(context.Background())
	cancel2()
	_, err = Stop(cancelled)
	assert.Error(t, err)
}

func TestConcurrentStopAndStatus(t *testing.T) {
	setup(t)
	s := listenT(t)
	base := runtime.NumGoroutine()

	const n = 32
	var wg sync.WaitGroup
	errs := make(chan error, 2*n)
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			ok, err := Stop(ctx(t))
			if err == nil && !ok {
				err = errors.New("stop not acknowledged")
			}
			errs <- err
		}()
		go func() {
			defer wg.Done()
			ok, err := Query(ctx(t))
			if err == nil && !ok {
				err = errors.New("status false")
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		assert.NoError(t, err)
	}

	// Stops coalesce into a single pending request.
	require.Equal(t, RequestStop, <-s.Requests())
	select {
	case <-s.Requests():
		t.Fatal("stops must coalesce")
	default:
	}

	require.NoError(t, s.Close())
	assert.Eventually(t, func() bool { return runtime.NumGoroutine() <= base }, 3*time.Second, 20*time.Millisecond,
		"server goroutines must exit on Close")
}

func TestStopAckDoesNotBlockOnUnreadRequests(t *testing.T) {
	setup(t)
	listenT(t) // nobody reads Requests
	for i := 0; i < 3; i++ {
		ok, err := Stop(ctx(t))
		require.NoError(t, err)
		assert.True(t, ok)
	}
}

func TestCloseLifecycle(t *testing.T) {
	rd := setup(t)
	s, err := Listen()
	require.NoError(t, err)

	idle := dial(t, rd) // an in-flight connection must not delay Close
	done := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); assert.NoError(t, s.Close()) }()
	}
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close blocked")
	}
	assert.NoError(t, s.Close(), "idempotent")

	idle.SetReadDeadline(time.Now().Add(time.Second))
	_, err = io.ReadAll(idle)
	if err != nil { // a not-yet-accepted connection is reset by the listener close
		assert.ErrorIs(t, err, unix.ECONNRESET)
	}

	_, err = os.Lstat(sockPath(rd))
	assert.True(t, os.IsNotExist(err))
	fi, err := os.Lstat(filepath.Join(rd, dirName, lockName))
	require.NoError(t, err, "lock file persists")
	assert.Equal(t, int64(0), fi.Size(), "lock file stays empty")

	ok, err := Stop(ctx(t))
	assert.NoError(t, err)
	assert.False(t, ok)
}

func TestCloseKeepsReplacedSocketPath(t *testing.T) {
	rd := setup(t)
	s, err := Listen()
	require.NoError(t, err)
	// Someone swaps our inode for something else; Close must not remove it.
	require.NoError(t, os.Remove(sockPath(rd)))
	require.NoError(t, os.WriteFile(sockPath(rd), []byte("other"), 0o600))
	require.NoError(t, s.Close())
	b, err := os.ReadFile(sockPath(rd))
	require.NoError(t, err)
	assert.Equal(t, "other", string(b))
}

func TestListenFailureLeavesNothingBehind(t *testing.T) {
	rd := setup(t)
	require.NoError(t, os.Mkdir(filepath.Join(rd, dirName), 0o700))
	require.NoError(t, os.WriteFile(sockPath(rd), nil, 0o600))
	_, err := Listen()
	require.Error(t, err)
	// The lock must have been released by the failed attempt.
	require.NoError(t, os.Remove(sockPath(rd)))
	s, err := Listen()
	require.NoError(t, err)
	require.NoError(t, s.Close())
}

func TestLongRuntimeDir(t *testing.T) {
	long := filepath.Join(t.TempDir(), strings.Repeat("d", 120), strings.Repeat("e", 120))
	require.NoError(t, os.MkdirAll(long, 0o700))
	t.Setenv("XDG_RUNTIME_DIR", long)
	s := listenT(t) // longer than sun_path, works through the directory fd
	ok, err := Query(ctx(t))
	require.NoError(t, err)
	assert.True(t, ok)
	require.NoError(t, s.Close())
}
