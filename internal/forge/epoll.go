package forge

import (
	"bytes"
	"fmt"
	"net/url"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

func rawRead(fd int, p []byte) (int, syscall.Errno) {
	if len(p) == 0 {
		return 0, 0
	}
	r0, _, e1 := syscall.RawSyscall(syscall.SYS_READ, uintptr(fd), uintptr(unsafe.Pointer(&p[0])), uintptr(len(p)))
	return int(r0), e1
}

func rawWrite(fd int, p []byte) (int, syscall.Errno) {
	if len(p) == 0 {
		return 0, 0
	}
	r0, _, e1 := syscall.RawSyscall(syscall.SYS_WRITE, uintptr(fd), uintptr(unsafe.Pointer(&p[0])), uintptr(len(p)))
	return int(r0), e1
}

func rawAccept4(fd int, flags int) (int, syscall.Errno) {
	r0, _, e1 := syscall.RawSyscall6(syscall.SYS_ACCEPT4, uintptr(fd), 0, 0, uintptr(flags), 0, 0)
	return int(r0), e1
}

const (
	initReadCap   = 16384
	initWriteCap  = 8192
	maxPoolRetain = 65536
	maxReqSize    = 64 << 20 // 64 MiB safety cap
	maxFDTable    = 16384
	numWorkers    = 2
)

// Request represents a parsed HTTP/1.1 request using slices directly into the connection read buffer.
type Request struct {
	Method      []byte
	RawTarget   []byte
	Path        []byte
	Query       []byte
	Host        []byte
	Auth        []byte
	GitProtocol []byte
	ContentType []byte
	Body        []byte
	KeepAlive   bool

	// Per-worker/per-request reusable output/scratch buffers.
	Out []byte
	Tmp []byte
}

func (r *Request) QueryParam(key string) string {
	q := r.Query
	for len(q) > 0 {
		var part []byte
		if idx := bytes.IndexByte(q, '&'); idx >= 0 {
			part = q[:idx]
			q = q[idx+1:]
		} else {
			part = q
			q = nil
		}
		if eq := bytes.IndexByte(part, '='); eq >= 0 {
			if string(part[:eq]) == key {
				v, err := url.QueryUnescape(string(part[eq+1:]))
				if err != nil {
					return string(part[eq+1:])
				}
				return v
			}
		} else if string(part) == key {
			return ""
		}
	}
	return ""
}

type connState struct {
	fd         int
	rbuf       []byte
	rpos       int
	rlen       int
	wbuf       []byte
	wpos       int
	wantOut    bool
	closeAfter bool
	closed     bool
	sent100    bool
}

var readBufPool = sync.Pool{
	New: func() any {
		return make([]byte, initReadCap)
	},
}

var connStatePool = sync.Pool{
	New: func() any {
		return &connState{}
	},
}

type lastReadCache struct {
	path      []byte
	resp      []byte
	issue     *Issue
	repoEpoch uint64
}

type epollWorker struct {
	id          int
	epFD        int
	wakeReadFD  int
	wakeWriteFD int
	isAcceptor  bool

	fdTable    [maxFDTable]atomic.Pointer[connState]
	overflowMu sync.Mutex
	overflow   map[int]*connState

	outBuf       []byte
	tmpBuf       []byte
	lastRead     *lastReadCache
	lastGitKey   []byte
	lastGitRepo  *Repo
	lastGitEpoch uint64
}

func newEpollWorker(id int, isAcceptor bool) (*epollWorker, error) {
	epFD, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("epoll_create1: %w", err)
	}
	var p [2]int
	if err := syscall.Pipe2(p[:], syscall.O_NONBLOCK|syscall.O_CLOEXEC); err != nil {
		_ = syscall.Close(epFD)
		return nil, fmt.Errorf("pipe2: %w", err)
	}
	ev := syscall.EpollEvent{
		Events: syscall.EPOLLIN,
		Fd:     int32(p[0]),
	}
	if err := syscall.EpollCtl(epFD, syscall.EPOLL_CTL_ADD, p[0], &ev); err != nil {
		_ = syscall.Close(p[0])
		_ = syscall.Close(p[1])
		_ = syscall.Close(epFD)
		return nil, fmt.Errorf("epoll_ctl wake: %w", err)
	}
	return &epollWorker{
		id:          id,
		epFD:        epFD,
		wakeReadFD:  p[0],
		wakeWriteFD: p[1],
		isAcceptor:  isAcceptor,
		overflow:    make(map[int]*connState, 8),
		outBuf:      make([]byte, 0, initWriteCap),
		tmpBuf:      make([]byte, 0, 2048),
	}, nil
}

func (w *epollWorker) closeFDs() {
	for fd := 0; fd < maxFDTable; fd++ {
		if c := w.fdTable[fd].Swap(nil); c != nil {
			_ = syscall.Close(c.fd)
		}
	}
	w.overflowMu.Lock()
	for fd, c := range w.overflow {
		_ = syscall.Close(c.fd)
		delete(w.overflow, fd)
	}
	w.overflowMu.Unlock()
	_ = syscall.Close(w.wakeReadFD)
	_ = syscall.Close(w.wakeWriteFD)
	_ = syscall.Close(w.epFD)
}

func (w *epollWorker) storeConn(fd int, c *connState) {
	if fd >= 0 && fd < maxFDTable {
		w.fdTable[fd].Store(c)
		return
	}
	w.overflowMu.Lock()
	w.overflow[fd] = c
	w.overflowMu.Unlock()
}

func (w *epollWorker) loadConn(fd int) *connState {
	if fd >= 0 && fd < maxFDTable {
		return w.fdTable[fd].Load()
	}
	w.overflowMu.Lock()
	c := w.overflow[fd]
	w.overflowMu.Unlock()
	return c
}

func (w *epollWorker) deleteConn(fd int) {
	if fd >= 0 && fd < maxFDTable {
		w.fdTable[fd].Store(nil)
		return
	}
	w.overflowMu.Lock()
	delete(w.overflow, fd)
	w.overflowMu.Unlock()
}

// EpollServer is a 2-worker raw Linux epoll HTTP/1.1 server pinned to OS threads
// with zero-EAGAIN keep-alive reads and lock-free atomic hot paths.
type EpollServer struct {
	store     *Store
	listenFD  int
	port      int
	closed    atomic.Bool
	acceptSeq uint64
	workers   [numWorkers]*epollWorker
	lastRead  atomic.Pointer[lastReadCache]
}

func NewEpollServer(store *Store, bindPort int) (*EpollServer, error) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_NONBLOCK|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("socket: %w", err)
	}
	_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
	_ = syscall.SetsockoptInt(fd, syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)

	sa := &syscall.SockaddrInet4{Port: bindPort}
	if err := syscall.Bind(fd, sa); err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("bind: %w", err)
	}
	if err := syscall.Listen(fd, 4096); err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("listen: %w", err)
	}

	sockname, err := syscall.Getsockname(fd)
	if err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("getsockname: %w", err)
	}
	actualPort := sockname.(*syscall.SockaddrInet4).Port

	var workers [numWorkers]*epollWorker
	for i := 0; i < numWorkers; i++ {
		w, err := newEpollWorker(i, i == 0)
		if err != nil {
			for j := 0; j < i; j++ {
				workers[j].closeFDs()
			}
			_ = syscall.Close(fd)
			return nil, err
		}
		workers[i] = w
	}

	ev := syscall.EpollEvent{
		Events: syscall.EPOLLIN,
		Fd:     int32(fd),
	}
	if err := syscall.EpollCtl(workers[0].epFD, syscall.EPOLL_CTL_ADD, fd, &ev); err != nil {
		for i := 0; i < numWorkers; i++ {
			workers[i].closeFDs()
		}
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("epoll_ctl listen: %w", err)
	}

	for i := 0; i < 64; i++ {
		connStatePool.Put(&connState{})
		readBufPool.Put(make([]byte, initReadCap))
	}

	return &EpollServer{
		store:    store,
		listenFD: fd,
		port:     actualPort,
		workers:  workers,
	}, nil
}

func (s *EpollServer) Port() int {
	return s.port
}

func (s *EpollServer) Close() {
	if s.closed.Swap(true) {
		return
	}
	_ = syscall.Close(s.listenFD)
	var b [1]byte
	b[0] = 1
	for i := 0; i < numWorkers; i++ {
		if w := s.workers[i]; w != nil {
			_, _ = syscall.Write(w.wakeWriteFD, b[:])
		}
	}
}

func (s *EpollServer) Serve() error {
	var wg sync.WaitGroup
	wg.Add(numWorkers - 1)
	for i := 1; i < numWorkers; i++ {
		w := s.workers[i]
		go func() {
			defer wg.Done()
			_ = w.run(s)
		}()
	}
	err := s.workers[0].run(s)
	wg.Wait()
	for i := 0; i < numWorkers; i++ {
		s.workers[i].closeFDs()
	}
	return err
}

func (w *epollWorker) run(s *EpollServer) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	events := make([]syscall.EpollEvent, 64)
	for !s.closed.Load() {
		n, err := syscall.EpollWait(w.epFD, events, -1)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			if s.closed.Load() {
				return nil
			}
			return err
		}
		for i := 0; i < n; i++ {
			ev := events[i]
			fd := int(ev.Fd)
			if fd == w.wakeReadFD {
				var drain [16]byte
				_, _ = syscall.Read(w.wakeReadFD, drain[:])
				if s.closed.Load() {
					return nil
				}
				continue
			}
			if w.isAcceptor && fd == s.listenFD {
				s.acceptAll()
				continue
			}
			c := w.loadConn(fd)
			if c == nil {
				continue
			}
			if ev.Events&(syscall.EPOLLERR|syscall.EPOLLHUP) != 0 {
				if ev.Events&syscall.EPOLLIN != 0 {
					w.handleRead(s, c)
				}
				if !c.closed {
					w.closeConn(c)
				}
				continue
			}
			if ev.Events&syscall.EPOLLOUT != 0 {
				w.handleWrite(c)
				if c.closed {
					continue
				}
			}
			if ev.Events&syscall.EPOLLIN != 0 {
				w.handleRead(s, c)
			}
		}
	}
	return nil
}

func (s *EpollServer) acceptAll() {
	for {
		nfd, errno := rawAccept4(s.listenFD, syscall.SOCK_NONBLOCK|syscall.SOCK_CLOEXEC)
		if errno != 0 {
			if errno == syscall.EAGAIN || errno == syscall.EWOULDBLOCK || errno == syscall.EINTR {
				return
			}
			return
		}
		_ = syscall.SetsockoptInt(nfd, syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
		w := s.workers[s.acceptSeq%numWorkers]
		s.acceptSeq++

		c := connStatePool.Get().(*connState)
		rbuf := readBufPool.Get().([]byte)
		*c = connState{
			fd:   nfd,
			rbuf: rbuf,
		}
		w.storeConn(nfd, c)

		ev := syscall.EpollEvent{
			Events: syscall.EPOLLIN,
			Fd:     int32(nfd),
		}
		if err := syscall.EpollCtl(w.epFD, syscall.EPOLL_CTL_ADD, nfd, &ev); err != nil {
			w.deleteConn(nfd)
			c.rbuf = nil
			connStatePool.Put(c)
			readBufPool.Put(rbuf)
			_ = syscall.Close(nfd)
			continue
		}
	}
}

func (w *epollWorker) closeConn(c *connState) {
	if c.closed {
		return
	}
	c.closed = true
	w.deleteConn(c.fd)
	_ = syscall.EpollCtl(w.epFD, syscall.EPOLL_CTL_DEL, c.fd, nil)
	_ = syscall.Close(c.fd)
	if cap(c.rbuf) <= maxPoolRetain {
		readBufPool.Put(c.rbuf[:cap(c.rbuf)])
	}
	c.rbuf = nil
	c.wbuf = c.wbuf[:0]
	if cap(c.wbuf) > maxPoolRetain {
		c.wbuf = nil
	}
	connStatePool.Put(c)
}

func (w *epollWorker) setEpollOut(c *connState, want bool) {
	if c.wantOut == want || c.closed {
		return
	}
	c.wantOut = want
	mask := uint32(syscall.EPOLLIN)
	if want {
		mask |= syscall.EPOLLOUT
	}
	ev := syscall.EpollEvent{Events: mask, Fd: int32(c.fd)}
	_ = syscall.EpollCtl(w.epFD, syscall.EPOLL_CTL_MOD, c.fd, &ev)
}

func (w *epollWorker) handleWrite(c *connState) {
	for c.wpos < len(c.wbuf) {
		n, errno := rawWrite(c.fd, c.wbuf[c.wpos:])
		if errno == 0 {
			if n > 0 {
				c.wpos += n
			}
			continue
		}
		if errno == syscall.EAGAIN || errno == syscall.EWOULDBLOCK {
			w.setEpollOut(c, true)
			return
		}
		if errno == syscall.EINTR {
			continue
		}
		w.closeConn(c)
		return
	}
	c.wbuf = c.wbuf[:0]
	c.wpos = 0
	w.setEpollOut(c, false)
	if c.closeAfter {
		w.closeConn(c)
	}
}

func (w *epollWorker) writeResponse(c *connState, resp []byte, closeConn bool) {
	if closeConn {
		c.closeAfter = true
	}
	if len(c.wbuf) == 0 {
		off := 0
		for off < len(resp) {
			n, errno := rawWrite(c.fd, resp[off:])
			if errno == 0 {
				if n > 0 {
					off += n
				}
				continue
			}
			if errno == syscall.EAGAIN || errno == syscall.EWOULDBLOCK {
				c.wbuf = append(c.wbuf[:0], resp[off:]...)
				c.wpos = 0
				w.setEpollOut(c, true)
				return
			}
			if errno == syscall.EINTR {
				continue
			}
			w.closeConn(c)
			return
		}
		if c.closeAfter {
			w.closeConn(c)
		}
		return
	}
	c.wbuf = append(c.wbuf, resp...)
	w.handleWrite(c)
}

func (w *epollWorker) handleRead(s *EpollServer, c *connState) {
	for !c.closed {
		if c.rlen == len(c.rbuf) {
			if c.rpos > 0 {
				copy(c.rbuf, c.rbuf[c.rpos:c.rlen])
				c.rlen -= c.rpos
				c.rpos = 0
			} else {
				newCap := len(c.rbuf) * 2
				if newCap > maxReqSize {
					w.closeConn(c)
					return
				}
				nb := make([]byte, newCap)
				copy(nb, c.rbuf[:c.rlen])
				c.rbuf = nb
			}
		}
		availSpace := len(c.rbuf) - c.rlen
		n, errno := rawRead(c.fd, c.rbuf[c.rlen:])
		if errno == 0 {
			if n == 0 {
				w.closeConn(c)
				return
			}
			c.rlen += n
			if !w.processBuffer(s, c) {
				return
			}
			// Return directly to epoll_wait when all buffered requests are complete
			// and kernel read was short, eliminating a wasted EAGAIN read per request.
			if n < availSpace && c.rlen == 0 {
				return
			}
			continue
		}
		if errno == syscall.EAGAIN || errno == syscall.EWOULDBLOCK {
			return
		}
		if errno == syscall.EINTR {
			continue
		}
		w.closeConn(c)
		return
	}
}

var (
	crlf2              = []byte("\r\n\r\n")
	dotGitSlash        = []byte(".git/")
	continue100        = []byte("HTTP/1.1 100 Continue\r\n\r\n")
	staticHealthzHTTP  = buildImmutableRESTResp(200, "application/json", []byte("{\"ok\":true}\n"))
	staticBadReqClose  = []byte("HTTP/1.1 400 Bad Request\r\nContent-Type: application/json\r\nX-GitHub-Request-Id: 1\r\nContent-Length: 26\r\nConnection: close\r\n\r\n{\"message\":\"Bad Request\"}\n")
	staticUnauthKeep   = buildImmutableRESTResp(401, "application/json", []byte("{\"message\":\"Requires authentication\"}\n"))
	staticNotFoundKeep = buildImmutableRESTResp(404, "application/json", []byte("{\"message\":\"Not Found\"}\n"))
)

func (w *epollWorker) processBuffer(s *EpollServer, c *connState) bool {
	var req Request
	for !c.closed {
		avail := c.rbuf[c.rpos:c.rlen]
		if len(avail) == 0 {
			c.rpos = 0
			c.rlen = 0
			return true
		}
		hdrEnd := bytes.Index(avail, crlf2)
		if hdrEnd < 0 {
			return true
		}
		hdrBytes := avail[:hdrEnd]
		bodyStart := hdrEnd + 4

		lineEnd := bytes.IndexByte(hdrBytes, '\r')
		if lineEnd < 0 {
			w.writeResponse(c, staticBadReqClose, true)
			return false
		}
		reqLine := hdrBytes[:lineEnd]
		sp1 := bytes.IndexByte(reqLine, ' ')
		if sp1 <= 0 {
			w.writeResponse(c, staticBadReqClose, true)
			return false
		}
		method := reqLine[:sp1]
		rest := reqLine[sp1+1:]
		sp2 := bytes.IndexByte(rest, ' ')
		if sp2 <= 0 {
			w.writeResponse(c, staticBadReqClose, true)
			return false
		}
		rawTarget := rest[:sp2]
		proto := rest[sp2+1:]

		keepAlive := true
		if len(proto) >= 8 && proto[7] == '0' {
			keepAlive = false
		}

		var host, auth, gitProto, contentType []byte
		contentLen := 0
		hasContentLen := false
		chunked := false
		expect100 := false

		pos := lineEnd + 2
		for pos < len(hdrBytes) {
			nl := bytes.IndexByte(hdrBytes[pos:], '\r')
			var line []byte
			if nl < 0 {
				line = hdrBytes[pos:]
				pos = len(hdrBytes)
			} else {
				line = hdrBytes[pos : pos+nl]
				pos += nl + 2
			}
			colon := bytes.IndexByte(line, ':')
			if colon <= 0 {
				continue
			}
			name := line[:colon]
			val := trimASCIISpace(line[colon+1:])
			c0 := name[0] | 0x20
			switch len(name) {
			case 4:
				if c0 == 'h' && eqFoldASCII(name, "host") {
					host = val
				}
			case 6:
				if c0 == 'e' && eqFoldASCII(name, "expect") && eqFoldASCII(val, "100-continue") {
					expect100 = true
				}
			case 10:
				if c0 == 'c' && eqFoldASCII(name, "connection") {
					if eqFoldASCII(val, "close") {
						keepAlive = false
					} else if eqFoldASCII(val, "keep-alive") {
						keepAlive = true
					}
				}
			case 12:
				if c0 == 'c' && eqFoldASCII(name, "content-type") {
					contentType = val
				} else if c0 == 'g' && eqFoldASCII(name, "git-protocol") {
					gitProto = val
				}
			case 13:
				if c0 == 'a' && eqFoldASCII(name, "authorization") {
					auth = val
				}
			case 14:
				if c0 == 'c' && eqFoldASCII(name, "content-length") {
					n, ok := parseUintASCII(val)
					if !ok || n > maxReqSize {
						w.writeResponse(c, staticBadReqClose, true)
						return false
					}
					contentLen = n
					hasContentLen = true
				}
			case 17:
				if c0 == 't' && eqFoldASCII(name, "transfer-encoding") {
					if containsFoldASCII(val, "chunked") {
						chunked = true
					}
				}
			}
		}

		if expect100 && !c.sent100 && len(avail)-bodyStart == 0 {
			c.sent100 = true
			w.writeResponse(c, continue100, false)
			if c.closed {
				return false
			}
		}

		var body []byte
		var totalConsumed int
		if chunked {
			decoded, consumed, done, bad := decodeChunked(avail[bodyStart:])
			if bad {
				w.writeResponse(c, staticBadReqClose, true)
				return false
			}
			if !done {
				return true
			}
			body = decoded
			totalConsumed = bodyStart + consumed
		} else if hasContentLen {
			if len(avail)-bodyStart < contentLen {
				needed := bodyStart + contentLen
				if c.rpos > 0 {
					copy(c.rbuf, c.rbuf[c.rpos:c.rlen])
					c.rlen -= c.rpos
					c.rpos = 0
				}
				if needed > len(c.rbuf) {
					newCap := len(c.rbuf) * 2
					for newCap < needed {
						newCap *= 2
					}
					if newCap > maxReqSize {
						newCap = maxReqSize
					}
					nb := make([]byte, newCap)
					copy(nb, c.rbuf[:c.rlen])
					c.rbuf = nb
				}
				return true
			}
			body = avail[bodyStart : bodyStart+contentLen]
			totalConsumed = bodyStart + contentLen
		} else {
			totalConsumed = bodyStart
		}

		path := rawTarget
		var query []byte
		if qidx := bytes.IndexByte(rawTarget, '?'); qidx >= 0 {
			path = rawTarget[:qidx]
			query = rawTarget[qidx+1:]
		}

		req = Request{
			Method:      method,
			RawTarget:   rawTarget,
			Path:        path,
			Query:       query,
			Host:        host,
			Auth:        auth,
			GitProtocol: gitProto,
			ContentType: contentType,
			Body:        body,
			KeepAlive:   keepAlive,
			Out:         w.outBuf[:0],
			Tmp:         w.tmpBuf[:0],
		}

		resp := w.dispatchWorker(s, &req)
		w.outBuf = req.Out
		w.tmpBuf = req.Tmp

		c.sent100 = false
		c.rpos += totalConsumed
		if c.rpos == c.rlen {
			c.rpos = 0
			c.rlen = 0
		}

		w.writeResponse(c, resp, !keepAlive)
		if c.closed || !keepAlive {
			return false
		}
	}
	return !c.closed
}

func decodeChunked(src []byte) (out []byte, consumed int, complete bool, bad bool) {
	pos := 0
	var decoded []byte
	for {
		nl := bytes.Index(src[pos:], []byte("\r\n"))
		if nl < 0 {
			return nil, 0, false, false
		}
		sizeLine := src[pos : pos+nl]
		if semi := bytes.IndexByte(sizeLine, ';'); semi >= 0 {
			sizeLine = sizeLine[:semi]
		}
		sizeLine = trimASCIISpace(sizeLine)
		sz, err := strconv.ParseInt(string(sizeLine), 16, 64)
		if err != nil || sz < 0 || sz > maxReqSize {
			return nil, 0, false, true
		}
		pos += nl + 2
		if sz == 0 {
			for {
				tnl := bytes.Index(src[pos:], []byte("\r\n"))
				if tnl < 0 {
					return nil, 0, false, false
				}
				if tnl == 0 {
					pos += 2
					return decoded, pos, true, false
				}
				pos += tnl + 2
			}
		}
		if pos+int(sz)+2 > len(src) {
			return nil, 0, false, false
		}
		decoded = append(decoded, src[pos:pos+int(sz)]...)
		pos += int(sz)
		if src[pos] != '\r' || src[pos+1] != '\n' {
			return nil, 0, false, true
		}
		pos += 2
	}
}

func trimASCIISpace(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t') {
		b = b[1:]
	}
	for len(b) > 0 && (b[len(b)-1] == ' ' || b[len(b)-1] == '\t') {
		b = b[:len(b)-1]
	}
	return b
}

func eqFoldASCII(a []byte, lower string) bool {
	if len(a) != len(lower) {
		return false
	}
	for i := 0; i < len(a); i++ {
		c := a[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != lower[i] {
			return false
		}
	}
	return true
}

func containsFoldASCII(hay []byte, needleLower string) bool {
	if len(needleLower) == 0 {
		return true
	}
	if len(hay) < len(needleLower) {
		return false
	}
	limit := len(hay) - len(needleLower)
	for i := 0; i <= limit; i++ {
		if eqFoldASCII(hay[i:i+len(needleLower)], needleLower) {
			return true
		}
	}
	return false
}

func parseUintASCII(b []byte) (int, bool) {
	if len(b) == 0 {
		return 0, false
	}
	n := 0
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

func statusText(code int) string {
	switch code {
	case 200:
		return "200 OK"
	case 201:
		return "201 Created"
	case 204:
		return "204 No Content"
	case 400:
		return "400 Bad Request"
	case 401:
		return "401 Unauthorized"
	case 403:
		return "403 Forbidden"
	case 404:
		return "404 Not Found"
	case 405:
		return "405 Method Not Allowed"
	case 409:
		return "409 Conflict"
	case 422:
		return "422 Unprocessable Entity"
	default:
		return strconv.Itoa(code) + " Status"
	}
}

func appendHTTPResp(dst []byte, status int, contentType string, body []byte, extraHeaders [][2]string, keepAlive bool, includeReqID bool) []byte {
	dst = append(dst, "HTTP/1.1 "...)
	dst = append(dst, statusText(status)...)
	dst = append(dst, "\r\nContent-Type: "...)
	dst = append(dst, contentType...)
	if includeReqID {
		dst = append(dst, "\r\nX-GitHub-Request-Id: 1"...)
	}
	for _, h := range extraHeaders {
		dst = append(dst, "\r\n"...)
		dst = append(dst, h[0]...)
		dst = append(dst, ": "...)
		dst = append(dst, h[1]...)
	}
	dst = append(dst, "\r\nContent-Length: "...)
	dst = strconv.AppendInt(dst, int64(len(body)), 10)
	if !keepAlive {
		dst = append(dst, "\r\nConnection: close\r\n\r\n"...)
	} else {
		dst = append(dst, "\r\n\r\n"...)
	}
	return append(dst, body...)
}

func (s *EpollServer) buildResp(status int, contentType string, body []byte, extraHeaders [][2]string, keepAlive bool) []byte {
	buf := make([]byte, 0, len(body)+128)
	return appendHTTPResp(buf, status, contentType, body, extraHeaders, keepAlive, true)
}

func (s *EpollServer) buildGitResp(status int, contentType string, body []byte, keepAlive bool) []byte {
	buf := make([]byte, 0, len(body)+96)
	return appendHTTPResp(buf, status, contentType, body, nil, keepAlive, false)
}

func (s *EpollServer) buildJSONResp(status int, body string, extraHeaders [][2]string, keepAlive bool) []byte {
	return s.buildResp(status, "application/json", []byte(body), extraHeaders, keepAlive)
}

func (s *EpollServer) buildJSONBytesResp(status int, body []byte, extraHeaders [][2]string, keepAlive bool) []byte {
	return s.buildResp(status, "application/json", body, extraHeaders, keepAlive)
}

func buildImmutableRESTResp(status int, contentType string, body []byte) []byte {
	out := make([]byte, 0, len(body)+128)
	return appendHTTPResp(out, status, contentType, body, nil, true, true)
}

func buildImmutableGitResp(status int, contentType string, body []byte) []byte {
	out := make([]byte, 0, len(body)+96)
	return appendHTTPResp(out, status, contentType, body, nil, true, false)
}

func (s *EpollServer) dispatchFastCore(req *Request, lr *lastReadCache, w *epollWorker) ([]byte, *lastReadCache) {
	if !req.KeepAlive {
		return nil, lr
	}
	p := req.Path
	m := req.Method

	// Fast path 1: GET /api/v3/repos/{owner}/{repo}/issues/{n} (bench.read)
	if len(m) == 3 && m[0] == 'G' && len(p) > 22 && string(p[:14]) == "/api/v3/repos/" {
		if !s.store.CheckAuth(req.Auth) {
			return staticUnauthKeep, lr
		}
		if lr != nil && lr.repoEpoch == s.store.repoEpoch.Load() && bytes.Equal(p, lr.path) {
			if lr.issue.cachedGetResp.Load() != nil {
				return lr.resp, lr
			}
		}
		rest := p[14:]
		if s1 := bytes.IndexByte(rest, '/'); s1 > 0 {
			owner := rest[:s1]
			afterOwner := rest[s1+1:]
			if s2 := bytes.IndexByte(afterOwner, '/'); s2 > 0 {
				repoName := afterOwner[:s2]
				tail := afterOwner[s2:]
				if len(tail) > 8 && string(tail[:8]) == "/issues/" {
					numPart := tail[8:]
					if bytes.IndexByte(numPart, '/') < 0 {
						if num, ok := parseUintASCII(numPart); ok && num > 0 {
							repo := s.store.GetRepoLockFree(owner, repoName)
							if repo == nil {
								return staticNotFoundKeep, lr
							}
							if snap := repo.issuesSnap.Load(); snap != nil && num <= len(*snap) {
								is := (*snap)[num-1]
								if cached := is.cachedGetResp.Load(); cached != nil {
									pathCopy := make([]byte, len(p))
									copy(pathCopy, p)
									newLR := &lastReadCache{
										path:      pathCopy,
										resp:      *cached,
										issue:     is,
										repoEpoch: s.store.repoEpoch.Load(),
									}
									return *cached, newLR
								}
							}
						}
					}
				}
			}
		}
		return nil, lr
	}

	// Fast path 2: /healthz
	if len(p) == 8 && string(p) == "/healthz" && len(m) == 3 && m[0] == 'G' {
		return staticHealthzHTTP, lr
	}

	// Fast path 3: Git Smart HTTP /{owner}/{repo}.git/... (bench.clone & bench.push GET)
	if dotGit := bytes.Index(p, dotGitSlash); dotGit > 2 {
		rest := p[1:dotGit]
		if !s.store.CheckAuth(req.Auth) {
			return staticUnauthKeep, lr
		}
		var repo *Repo
		if w != nil && w.lastGitRepo != nil && w.lastGitEpoch == s.store.repoEpoch.Load() && bytes.Equal(rest, w.lastGitKey) {
			repo = w.lastGitRepo
		} else if slash1 := bytes.IndexByte(rest, '/'); slash1 > 0 {
			repo = s.store.GetRepoLockFree(rest[:slash1], rest[slash1+1:])
			if repo == nil {
				return staticNotFoundKeep, lr
			}
			if w != nil {
				w.lastGitKey = append(w.lastGitKey[:0], rest...)
				w.lastGitRepo = repo
				w.lastGitEpoch = s.store.repoEpoch.Load()
			}
		} else {
			return nil, lr
		}
		subPath := p[dotGit+5:]
		if len(m) == 3 && m[0] == 'G' && string(subPath) == "info/refs" {
			if bytes.Equal(req.Query, qUploadPack) {
				if bytes.Contains(req.GitProtocol, v2ProtoBytes) {
					return staticUploadV2HTTP, lr
				}
				if cached := repo.upV0Cache.Load(); cached != nil {
					return *cached, lr
				}
			} else if bytes.Equal(req.Query, qReceivePack) {
				if cached := repo.recvCache.Load(); cached != nil {
					return *cached, lr
				}
			}
			return nil, lr
		}
		if len(m) == 4 && m[0] == 'P' && string(subPath) == "git-upload-pack" {
			if bytes.Contains(req.GitProtocol, v2ProtoBytes) {
				if fc := repo.fetchCache.Load(); fc != nil {
					entries := *fc
					for i := range entries {
						if bytes.Equal(entries[i].key, req.Body) {
							return entries[i].resp, lr
						}
					}
				}
			}
		}
	}
	return nil, lr
}

func (w *epollWorker) dispatchWorker(s *EpollServer, req *Request) []byte {
	resp, nextLR := s.dispatchFastCore(req, w.lastRead, w)
	w.lastRead = nextLR
	if resp != nil {
		return resp
	}
	return s.dispatchLocked(req)
}

func (s *EpollServer) dispatch(req *Request) []byte {
	curLR := s.lastRead.Load()
	resp, nextLR := s.dispatchFastCore(req, curLR, nil)
	if nextLR != curLR {
		s.lastRead.Store(nextLR)
	}
	if resp != nil {
		return resp
	}
	return s.dispatchLocked(req)
}

func (s *EpollServer) dispatchLocked(req *Request) []byte {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()

	p := req.Path
	// REST API (/api/v3...) and GraphQL (/api/graphql)
	if len(p) >= 7 && p[0] == '/' && p[1] == 'a' && p[2] == 'p' && p[3] == 'i' && p[4] == '/' {
		if p[5] == 'v' && p[6] == '3' && (len(p) == 7 || p[7] == '/') {
			return s.handleREST(req)
		}
		if string(p) == "/api/graphql" {
			return s.handleGraphQL(req)
		}
	}
	// /healthz (no auth required)
	if len(p) == 8 && string(p) == "/healthz" {
		if string(req.Method) == "GET" {
			return s.buildJSONResp(200, "{\"ok\":true}\n", nil, req.KeepAlive)
		}
		return s.buildJSONResp(405, "{\"message\":\"Method Not Allowed\"}\n", nil, req.KeepAlive)
	}
	// /mcp
	if len(p) == 4 && string(p) == "/mcp" {
		return s.handleMCP(req)
	}
	// Git Smart HTTP /{owner}/{repo}.git/...
	if bytes.Contains(p, dotGitSlash) {
		return s.handleGitHTTP(req)
	}
	return s.buildJSONResp(404, "{\"message\":\"Not Found\"}\n", nil, req.KeepAlive)
}
