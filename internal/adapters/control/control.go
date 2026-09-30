// Package control implements the private recording control socket.
//
// A recording process calls Listen to own $XDG_RUNTIME_DIR/nefercap/record.sock
// (mode 0600, inside a 0700 directory owned by the caller). Another process
// asks it to stop or reports its state with Stop and Query. The wire protocol
// is two request lines, "stop\n" and "status\n", answered with "ok\n" and
// "recording\n". No path, pid or file name is ever sent.
//
// Ownership is enforced by an flock(2) on a private lock file held for the
// server's lifetime, so a leftover socket is removed only by the process that
// holds the lock and clients never unlink anything. Peers are checked with
// SO_PEERCRED and must run as the same user; this is not general Unix
// authentication, and processes of the same user are trusted.
package control

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	dirName    = "nefercap"
	sockName   = "record.sock"
	lockName   = "record.lock"
	maxPath    = 4096 // longest accepted runtime directory
	maxRequest = 64
	connLimit  = 4
	ioTimeout  = time.Second
	backlog    = connLimit

	stopLine   = "stop\n"
	statusLine = "status\n"
	okLine     = "ok\n"
	recLine    = "recording\n"
)

var (
	// ErrAlreadyRecording means another live process owns the control socket.
	ErrAlreadyRecording = errors.New("control: a recording is already active")
	// ErrNoRuntimeDir means XDG_RUNTIME_DIR is unset or unusable.
	ErrNoRuntimeDir = errors.New("control: XDG_RUNTIME_DIR is not an absolute path")
	// ErrInsecure means a control path has the wrong type, owner or mode.
	ErrInsecure = errors.New("control: insecure control path")
	// ErrProtocol means the peer answered with something unexpected.
	ErrProtocol = errors.New("control: protocol error")
)

// Request is a control request delivered to the recording owner. Status
// requests are answered by the server itself and are never delivered.
type Request uint8

// RequestStop asks the recording owner to cancel and finalize.
const RequestStop Request = iota + 1

// Server is the listening side owned by the recording process.
type Server struct {
	ln      *net.UnixListener
	dirFD   int
	lockFD  int
	dev     uint64
	ino     uint64
	peerUID uint32

	reqs chan Request
	sem  chan struct{}
	done chan struct{}
	wg   sync.WaitGroup

	mu     sync.Mutex
	closed bool
	conns  map[net.Conn]struct{}

	once     sync.Once
	closeErr error
}

// cleanRuntimeDir validates a raw runtime directory string.
func cleanRuntimeDir(rd string) (string, error) {
	if !filepath.IsAbs(rd) || len(rd) > maxPath || strings.IndexByte(rd, 0) >= 0 {
		return "", ErrNoRuntimeDir
	}
	return filepath.Clean(rd), nil
}

func runtimeDir() (string, error) { return cleanRuntimeDir(os.Getenv("XDG_RUNTIME_DIR")) }

// openSecureDir opens runtime/nefercap without following symlinks and checks
// that it is a directory owned by uid with mode exactly 0700. All later
// operations are relative to the returned descriptor.
func openSecureDir(runtime string, create bool, uid uint32) (int, error) {
	dir := filepath.Join(runtime, dirName)
	if create {
		err := unix.Mkdir(dir, 0o700)
		if err == nil {
			err = unix.Chmod(dir, 0o700) // independent of the umask
		}
		if err != nil && err != unix.EEXIST {
			return -1, fmt.Errorf("control: create %s: %w", dirName, err)
		}
	}
	fd, err := unix.Open(dir, unix.O_PATH|unix.O_NOFOLLOW|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		if err == unix.ELOOP || err == unix.ENOTDIR {
			return -1, fmt.Errorf("%w: %s is not a directory", ErrInsecure, dirName)
		}
		return -1, fmt.Errorf("control: open %s: %w", dirName, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("control: stat %s: %w", dirName, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Uid != uid || st.Mode&0o777 != 0o700 {
		unix.Close(fd)
		return -1, fmt.Errorf("%w: %s must be a 0700 directory owned by the current user", ErrInsecure, dirName)
	}
	return fd, nil
}

// Listen becomes the single control server. It returns ErrAlreadyRecording
// when another live process holds the lock. The caller must Close the server.
func Listen() (*Server, error) {
	rd, err := runtimeDir()
	if err != nil {
		return nil, err
	}
	return listen(rd, uint32(os.Geteuid()))
}

// listen is Listen with an explicit accepted peer uid, so tests can exercise
// the foreign-peer rejection with real sockets.
func listen(runtime string, peerUID uint32) (s *Server, err error) {
	if runtime, err = cleanRuntimeDir(runtime); err != nil {
		return nil, err
	}
	euid := uint32(os.Geteuid())
	dirFD, err := openSecureDir(runtime, true, euid)
	if err != nil {
		return nil, err
	}
	lockFD := -1
	sockFD := -1
	bound := false
	var dev, ino uint64
	defer func() {
		if err == nil {
			return
		}
		if sockFD >= 0 {
			unix.Close(sockFD)
		}
		if bound && sameInode(dirFD, dev, ino) {
			unix.Unlinkat(dirFD, sockName, 0)
		}
		if lockFD >= 0 {
			unix.Close(lockFD)
		}
		unix.Close(dirFD)
	}()

	lockFD, err = unix.Openat(dirFD, lockName, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("control: open lock: %w", err)
	}
	var st unix.Stat_t
	if err = unix.Fstat(lockFD, &st); err != nil {
		return nil, fmt.Errorf("control: stat lock: %w", err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != euid {
		return nil, fmt.Errorf("%w: lock file", ErrInsecure)
	}
	if err = unix.Fchmod(lockFD, 0o600); err != nil {
		return nil, fmt.Errorf("control: chmod lock: %w", err)
	}
	if err = unix.Flock(lockFD, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if err == unix.EWOULDBLOCK {
			err = ErrAlreadyRecording
		} else {
			err = fmt.Errorf("control: lock: %w", err)
		}
		return nil, err
	}

	// The lock is held, so no live server owns the socket path: a leftover
	// socket of ours is stale. Anything else is left alone.
	if err = unix.Fstatat(dirFD, sockName, &st, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		if st.Mode&unix.S_IFMT != unix.S_IFSOCK || st.Uid != euid {
			err = fmt.Errorf("%w: %s is not a socket owned by the current user", ErrInsecure, sockName)
			return nil, err
		}
		if err = unix.Unlinkat(dirFD, sockName, 0); err != nil {
			return nil, fmt.Errorf("control: remove stale socket: %w", err)
		}
	} else if err != unix.ENOENT {
		return nil, fmt.Errorf("control: stat socket: %w", err)
	}

	sockFD, err = unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("control: socket: %w", err)
	}
	if err = unix.Bind(sockFD, &unix.SockaddrUnix{Name: dirPath(dirFD)}); err != nil {
		return nil, fmt.Errorf("control: bind: %w", err)
	}
	bound = true
	if err = unix.Fstatat(dirFD, sockName, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		bound = false
		return nil, fmt.Errorf("control: stat socket: %w", err)
	}
	dev, ino = uint64(st.Dev), uint64(st.Ino)
	// Not yet listening, so nobody can connect while the mode is tightened.
	if err = unix.Fchmodat(dirFD, sockName, 0o600, 0); err != nil {
		return nil, fmt.Errorf("control: chmod socket: %w", err)
	}
	if err = unix.Fstatat(dirFD, sockName, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, fmt.Errorf("control: stat socket: %w", err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFSOCK || st.Uid != euid || st.Mode&0o777 != 0o600 ||
		uint64(st.Dev) != dev || uint64(st.Ino) != ino {
		err = fmt.Errorf("%w: created socket", ErrInsecure)
		return nil, err
	}
	if err = unix.Listen(sockFD, backlog); err != nil {
		return nil, fmt.Errorf("control: listen: %w", err)
	}
	f := os.NewFile(uintptr(sockFD), sockName)
	sockFD = -1
	l, lerr := net.FileListener(f)
	f.Close()
	if lerr != nil {
		err = fmt.Errorf("control: listener: %w", lerr)
		return nil, err
	}
	ln := l.(*net.UnixListener)
	ln.SetUnlinkOnClose(false)

	s = &Server{
		ln:      ln,
		dirFD:   dirFD,
		lockFD:  lockFD,
		dev:     dev,
		ino:     ino,
		peerUID: peerUID,
		reqs:    make(chan Request, 1),
		sem:     make(chan struct{}, connLimit),
		done:    make(chan struct{}),
		conns:   make(map[net.Conn]struct{}, connLimit),
	}
	s.wg.Add(1)
	go s.acceptLoop()
	return s, nil
}

// dirPath names the verified directory through its descriptor, so bind and
// connect cannot be redirected by swapping path components afterwards. It is
// short, which also avoids the sun_path length limit.
func dirPath(dirFD int) string {
	return "/proc/self/fd/" + strconv.Itoa(dirFD) + "/" + sockName
}

func sameInode(dirFD int, dev, ino uint64) bool {
	var st unix.Stat_t
	if unix.Fstatat(dirFD, sockName, &st, unix.AT_SYMLINK_NOFOLLOW) != nil {
		return false
	}
	return st.Mode&unix.S_IFMT == unix.S_IFSOCK && uint64(st.Dev) == dev && uint64(st.Ino) == ino
}

// Requests delivers RequestStop. Repeated stops coalesce into one pending
// request. The channel is never closed; select on it together with the
// recording's own completion.
func (s *Server) Requests() <-chan Request { return s.reqs }

// Close stops serving, removes the socket only if it is still the inode this
// server created, then releases the lock. It is idempotent and safe for
// concurrent use.
func (s *Server) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		close(s.done)
		for c := range s.conns {
			c.Close()
		}
		s.mu.Unlock()
		s.closeErr = s.ln.Close()
		s.wg.Wait()
		if sameInode(s.dirFD, s.dev, s.ino) {
			if err := unix.Unlinkat(s.dirFD, sockName, 0); err != nil && err != unix.ENOENT && s.closeErr == nil {
				s.closeErr = fmt.Errorf("control: remove socket: %w", err)
			}
		}
		unix.Close(s.dirFD)
		unix.Close(s.lockFD) // releases the flock last
	})
	return s.closeErr
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		select {
		case s.sem <- struct{}{}:
		case <-s.done:
			return
		}
		c, err := s.ln.AcceptUnix()
		if err != nil {
			<-s.sem
			select {
			case <-s.done:
				return
			default:
			}
			// Transient failure (for example fd exhaustion): back off.
			t := time.NewTimer(50 * time.Millisecond)
			select {
			case <-t.C:
			case <-s.done:
				t.Stop()
				return
			}
			continue
		}
		s.wg.Add(1)
		go s.serve(c)
	}
}

func (s *Server) serve(c *net.UnixConn) {
	defer s.wg.Done()
	defer func() { <-s.sem }()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		c.Close()
		return
	}
	s.conns[c] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
		c.Close()
	}()

	if c.SetDeadline(time.Now().Add(ioTimeout)) != nil {
		return
	}
	if uid, err := peerUID(c); err != nil || uid != s.peerUID {
		return
	}
	line, ok := readLine(c)
	if !ok {
		return
	}
	var reply string
	switch {
	case bytes.Equal(line, []byte(stopLine)):
		// Coalescing, non-blocking: the ack means the stop was requested,
		// not that the capture file is finalized.
		select {
		case s.reqs <- RequestStop:
		default:
		}
		reply = okLine
	case bytes.Equal(line, []byte(statusLine)):
		reply = recLine
	default:
		return
	}
	io.WriteString(c, reply)
}

// readLine reads one newline-terminated request of at most maxRequest bytes.
func readLine(r io.Reader) ([]byte, bool) {
	var buf [maxRequest]byte
	b := buf[:]
	n := 0
	for n < len(b) {
		m, err := r.Read(b[n:])
		n += m
		if i := bytes.IndexByte(b[:n], '\n'); i >= 0 {
			// The request is exactly one line; trailing bytes are junk.
			return b[:i+1], i+1 == n
		}
		if err != nil {
			return nil, false
		}
	}
	return nil, false
}

func peerUID(c *net.UnixConn) (uint32, error) {
	rc, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var uc *unix.Ucred
	var serr error
	if err := rc.Control(func(fd uintptr) {
		uc, serr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if serr != nil {
		return 0, serr
	}
	return uc.Uid, nil
}

// Stop asks the active recording to stop. It returns false with a nil error
// when no recording is active. A true result means the request was accepted,
// not that the capture has been finalized.
func Stop(ctx context.Context) (bool, error) {
	rd, err := runtimeDir()
	if err != nil {
		return false, err
	}
	return exchange(ctx, rd, stopLine, okLine)
}

// Query reports whether a recording is active.
func Query(ctx context.Context) (bool, error) {
	rd, err := runtimeDir()
	if err != nil {
		return false, err
	}
	return exchange(ctx, rd, statusLine, recLine)
}

func exchange(ctx context.Context, runtime, req, want string) (bool, error) {
	euid := uint32(os.Geteuid())
	dirFD, err := openSecureDir(runtime, false, euid)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, err
	}
	defer unix.Close(dirFD)
	var st unix.Stat_t
	if err := unix.Fstatat(dirFD, sockName, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if err == unix.ENOENT {
			return false, nil
		}
		return false, fmt.Errorf("control: stat socket: %w", err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFSOCK || st.Uid != euid || st.Mode&0o077 != 0 {
		return false, fmt.Errorf("%w: %s", ErrInsecure, sockName)
	}

	ctx, cancel := context.WithTimeout(ctx, ioTimeout)
	defer cancel()
	conn, err := dialRetry(ctx, dirPath(dirFD))
	if err != nil {
		// A dead owner leaves the socket behind; only the next server, which
		// holds the lock, removes it.
		if errors.Is(err, unix.ECONNREFUSED) || errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, err
	}
	defer conn.Close()
	uc := conn.(*net.UnixConn)
	stop := context.AfterFunc(ctx, func() { uc.Close() })
	defer stop()
	if dl, ok := ctx.Deadline(); ok {
		uc.SetDeadline(dl)
	}
	if uid, err := peerUID(uc); err != nil {
		return false, fmt.Errorf("control: peer credentials: %w", err)
	} else if uid != euid {
		return false, fmt.Errorf("%w: socket served by another user", ErrInsecure)
	}
	if _, err := io.WriteString(uc, req); err != nil {
		return false, ctxErr(ctx, err)
	}
	var buf [len(recLine)]byte // the longest reply
	n, err := io.ReadFull(uc, buf[:len(want)])
	if err != nil {
		return false, ctxErr(ctx, err)
	}
	if string(buf[:n]) != want {
		return false, ErrProtocol
	}
	return true, nil
}

// dialRetry connects, retrying while the server's small accept backlog is
// full (EAGAIN) until ctx ends.
func dialRetry(ctx context.Context, path string) (net.Conn, error) {
	var d net.Dialer
	for {
		conn, err := d.DialContext(ctx, "unix", path)
		if !errors.Is(err, unix.EAGAIN) {
			return conn, err
		}
		t := time.NewTimer(5 * time.Millisecond)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		}
	}
}

func ctxErr(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	return err
}
