package forge

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"net/url"
	"runtime"
	"strconv"
	"sync/atomic"
	"syscall"
	"unsafe"
)

const (
	tcpQuickack = 12 // Linux TCP_QUICKACK
	initReadCap = 16384
	maxReqSize  = 64 << 20 // 64 MiB safety cap
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
	sent100    bool
}

type EpollServer struct {
	store    *Store
	listenFD int
	epFD     int
	port     int
	closed   atomic.Bool
	conns    map[int]*connState
	scratch  []byte // reusable per-request response scratch buffer (single-threaded)
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
	if err := syscall.Listen(fd, 1024); err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("listen: %w", err)
	}

	sockname, err := syscall.Getsockname(fd)
	if err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("getsockname: %w", err)
	}
	actualPort := sockname.(*syscall.SockaddrInet4).Port

	epFD, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
	if err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("epoll_create1: %w", err)
	}

	ev := syscall.EpollEvent{
		Events: syscall.EPOLLIN,
		Fd:     int32(fd),
	}
	if err := syscall.EpollCtl(epFD, syscall.EPOLL_CTL_ADD, fd, &ev); err != nil {
		_ = syscall.Close(epFD)
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("epoll_ctl listen: %w", err)
	}

	return &EpollServer{
		store:    store,
		listenFD: fd,
		epFD:     epFD,
		port:     actualPort,
		conns:    make(map[int]*connState, 128),
		scratch:  make([]byte, 0, 32768),
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
	_ = syscall.Close(s.epFD)
}

func (s *EpollServer) Serve() error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	events := make([]syscall.EpollEvent, 64)
	for !s.closed.Load() {
		n, err := syscall.EpollWait(s.epFD, events, -1)
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
			if fd == s.listenFD {
				s.acceptAll()
				continue
			}
			c, ok := s.conns[fd]
			if !ok {
				continue
			}
			if ev.Events&(syscall.EPOLLERR|syscall.EPOLLHUP) != 0 {
				if ev.Events&syscall.EPOLLIN != 0 {
					s.handleRead(c)
				}
				if _, still := s.conns[fd]; still {
					s.closeConn(c)
				}
				continue
			}
			if ev.Events&syscall.EPOLLOUT != 0 {
				s.handleWrite(c)
				if _, still := s.conns[fd]; !still {
					continue
				}
			}
			if ev.Events&syscall.EPOLLIN != 0 {
				s.handleRead(c)
			}
		}
	}
	return nil
}

func (s *EpollServer) acceptAll() {
	for {
		nfd, _, err := syscall.Accept4(s.listenFD, syscall.SOCK_NONBLOCK|syscall.SOCK_CLOEXEC)
		if err != nil {
			if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK || err == syscall.EINTR {
				return
			}
			return
		}
		_ = syscall.SetsockoptInt(nfd, syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
		ev := syscall.EpollEvent{
			Events: syscall.EPOLLIN,
			Fd:     int32(nfd),
		}
		if err := syscall.EpollCtl(s.epFD, syscall.EPOLL_CTL_ADD, nfd, &ev); err != nil {
			_ = syscall.Close(nfd)
			continue
		}
		s.conns[nfd] = &connState{
			fd:   nfd,
			rbuf: make([]byte, initReadCap),
		}
	}
}

func (s *EpollServer) closeConn(c *connState) {
	delete(s.conns, c.fd)
	_ = syscall.EpollCtl(s.epFD, syscall.EPOLL_CTL_DEL, c.fd, nil)
	_ = syscall.Close(c.fd)
}

func (s *EpollServer) setEpollOut(c *connState, want bool) {
	if c.wantOut == want {
		return
	}
	c.wantOut = want
	mask := uint32(syscall.EPOLLIN)
	if want {
		mask |= syscall.EPOLLOUT
	}
	ev := syscall.EpollEvent{Events: mask, Fd: int32(c.fd)}
	_ = syscall.EpollCtl(s.epFD, syscall.EPOLL_CTL_MOD, c.fd, &ev)
}

func (s *EpollServer) handleWrite(c *connState) {
	for c.wpos < len(c.wbuf) {
		n, err := syscall.Write(c.fd, c.wbuf[c.wpos:])
		if n > 0 {
			c.wpos += n
		}
		if err != nil {
			if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
				s.setEpollOut(c, true)
				return
			}
			if err == syscall.EINTR {
				continue
			}
			s.closeConn(c)
			return
		}
	}
	c.wbuf = c.wbuf[:0]
	c.wpos = 0
	s.setEpollOut(c, false)
	if c.closeAfter {
		s.closeConn(c)
	}
}

func (s *EpollServer) writeResponse(c *connState, resp []byte, closeConn bool) {
	if closeConn {
		c.closeAfter = true
	}
	if len(c.wbuf) == 0 {
		off := 0
		for off < len(resp) {
			n, err := syscall.Write(c.fd, resp[off:])
			if n > 0 {
				off += n
			}
			if err != nil {
				if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
					c.wbuf = append(c.wbuf[:0], resp[off:]...)
					c.wpos = 0
					s.setEpollOut(c, true)
					return
				}
				if err == syscall.EINTR {
					continue
				}
				s.closeConn(c)
				return
			}
		}
		if c.closeAfter {
			s.closeConn(c)
		}
		return
	}
	c.wbuf = append(c.wbuf, resp...)
	s.handleWrite(c)
}

func (s *EpollServer) handleRead(c *connState) {
	for {
		if c.rlen == len(c.rbuf) {
			if c.rpos > 0 {
				copy(c.rbuf, c.rbuf[c.rpos:c.rlen])
				c.rlen -= c.rpos
				c.rpos = 0
			} else {
				newCap := len(c.rbuf) * 2
				if newCap > maxReqSize {
					s.closeConn(c)
					return
				}
				nb := make([]byte, newCap)
				copy(nb, c.rbuf[:c.rlen])
				c.rbuf = nb
			}
		}
		availSpace := len(c.rbuf) - c.rlen
		n, err := syscall.Read(c.fd, c.rbuf[c.rlen:])
		if n > 0 {
			c.rlen += n
			if !s.processBuffer(c) {
				return
			}
			// If kernel returned less than buffer space and we consumed all complete
			// requests (c.rlen == 0), return directly to epoll_wait to avoid a wasted
			// EAGAIN read syscall on every sequential keep-alive request.
			if n < availSpace && c.rlen == 0 {
				return
			}
			continue
		}
		if n == 0 && err == nil {
			s.closeConn(c)
			return
		}
		if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
			return
		}
		if err == syscall.EINTR {
			continue
		}
		s.closeConn(c)
		return
	}
}

var (
	crlf2       = []byte("\r\n\r\n")
	continue100 = []byte("HTTP/1.1 100 Continue\r\n\r\n")
)

// processBuffer parses and dispatches all complete HTTP/1.1 requests in c.rbuf.
// Returns false if the connection was closed.
func (s *EpollServer) processBuffer(c *connState) bool {
	for {
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

		// Parse request line: METHOD SP TARGET SP HTTP/1.x
		lineEnd := bytes.IndexByte(hdrBytes, '\r')
		if lineEnd < 0 {
			s.writeResponse(c, s.buildJSONResp(400, `{"message":"Bad Request"}`+"\n", nil, false), true)
			return false
		}
		reqLine := hdrBytes[:lineEnd]
		sp1 := bytes.IndexByte(reqLine, ' ')
		if sp1 <= 0 {
			s.writeResponse(c, s.buildJSONResp(400, `{"message":"Bad Request"}`+"\n", nil, false), true)
			return false
		}
		method := reqLine[:sp1]
		rest := reqLine[sp1+1:]
		sp2 := bytes.IndexByte(rest, ' ')
		if sp2 <= 0 {
			s.writeResponse(c, s.buildJSONResp(400, `{"message":"Bad Request"}`+"\n", nil, false), true)
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

		// Parse headers without allocating
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
			switch len(name) {
			case 4:
				if eqFoldASCII(name, "host") {
					host = val
				}
			case 6:
				if eqFoldASCII(name, "expect") && eqFoldASCII(val, "100-continue") {
					expect100 = true
				}
			case 10:
				if eqFoldASCII(name, "connection") {
					if eqFoldASCII(val, "close") {
						keepAlive = false
					} else if eqFoldASCII(val, "keep-alive") {
						keepAlive = true
					}
				}
			case 12:
				if eqFoldASCII(name, "content-type") {
					contentType = val
				} else if eqFoldASCII(name, "git-protocol") {
					gitProto = val
				}
			case 13:
				if eqFoldASCII(name, "authorization") {
					auth = val
				}
			case 14:
				if eqFoldASCII(name, "content-length") {
					n, ok := parseUintASCII(val)
					if !ok || n > maxReqSize {
						s.writeResponse(c, s.buildJSONResp(400, `{"message":"Invalid Content-Length"}`+"\n", nil, false), true)
						return false
					}
					contentLen = n
					hasContentLen = true
				}
			case 17:
				if eqFoldASCII(name, "transfer-encoding") {
					if containsFoldASCII(val, "chunked") {
						chunked = true
					}
				}
			}
		}

		if expect100 && !c.sent100 && len(avail)-bodyStart == 0 {
			c.sent100 = true
			s.writeResponse(c, continue100, false)
			if _, still := s.conns[c.fd]; !still {
				return false
			}
		}

		var body []byte
		var totalConsumed int
		if chunked {
			decoded, consumed, done, bad := decodeChunked(avail[bodyStart:])
			if bad {
				s.writeResponse(c, s.buildJSONResp(400, `{"message":"Bad chunked encoding"}`+"\n", nil, false), true)
				return false
			}
			if !done {
				return true
			}
			body = decoded
			totalConsumed = bodyStart + consumed
		} else if hasContentLen {
			if len(avail)-bodyStart < contentLen {
				// Grow buffer upfront to fit the whole body in minimal reads
				needed := c.rpos + bodyStart + contentLen
				if needed > len(c.rbuf) {
					if c.rpos > 0 {
						copy(c.rbuf, c.rbuf[c.rpos:c.rlen])
						c.rlen -= c.rpos
						c.rpos = 0
						needed = bodyStart + contentLen
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

		req := Request{
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
		}

		resp := s.dispatch(&req)
		c.sent100 = false
		c.rpos += totalConsumed
		if c.rpos == c.rlen {
			c.rpos = 0
			c.rlen = 0
		}

		s.writeResponse(c, resp, !keepAlive)
		if !keepAlive {
			return false
		}
		if _, still := s.conns[c.fd]; !still {
			return false
		}
	}
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
			// Consume trailer lines ending with CRLF
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

func b2s(b []byte) string {
	return unsafe.String(unsafe.SliceData(b), len(b))
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

func (s *EpollServer) nextReqID(dst []byte) []byte {
	s.store.ReqSeq++
	var raw [8]byte
	v := s.store.ReqSeq
	raw[0] = byte(v >> 56)
	raw[1] = byte(v >> 48)
	raw[2] = byte(v >> 40)
	raw[3] = byte(v >> 32)
	raw[4] = byte(v >> 24)
	raw[5] = byte(v >> 16)
	raw[6] = byte(v >> 8)
	raw[7] = byte(v)
	var enc [16]byte
	hex.Encode(enc[:], raw[:])
	return append(dst, enc[:]...)
}

func (s *EpollServer) buildResp(status int, contentType string, body []byte, extraHeaders [][2]string, keepAlive bool) []byte {
	buf := s.scratch[:0]
	buf = append(buf, "HTTP/1.1 "...)
	buf = append(buf, statusText(status)...)
	buf = append(buf, "\r\nContent-Type: "...)
	buf = append(buf, contentType...)
	buf = append(buf, "\r\nX-GitHub-Request-Id: "...)
	buf = s.nextReqID(buf)
	for _, h := range extraHeaders {
		buf = append(buf, "\r\n"...)
		buf = append(buf, h[0]...)
		buf = append(buf, ": "...)
		buf = append(buf, h[1]...)
	}
	buf = append(buf, "\r\nContent-Length: "...)
	buf = strconv.AppendInt(buf, int64(len(body)), 10)
	if keepAlive {
		buf = append(buf, "\r\nConnection: keep-alive\r\n\r\n"...)
	} else {
		buf = append(buf, "\r\nConnection: close\r\n\r\n"...)
	}
	buf = append(buf, body...)
	s.scratch = buf
	return buf
}

func (s *EpollServer) buildJSONResp(status int, body string, extraHeaders [][2]string, keepAlive bool) []byte {
	return s.buildResp(status, "application/json", []byte(body), extraHeaders, keepAlive)
}

func (s *EpollServer) buildJSONBytesResp(status int, body []byte, extraHeaders [][2]string, keepAlive bool) []byte {
	return s.buildResp(status, "application/json", body, extraHeaders, keepAlive)
}

func buildImmutableHTTPResp(status int, contentType string, body []byte) []byte {
	out := make([]byte, 0, len(body)+144)
	out = append(out, "HTTP/1.1 "...)
	out = append(out, statusText(status)...)
	out = append(out, "\r\nContent-Type: "...)
	out = append(out, contentType...)
	out = append(out, "\r\nX-GitHub-Request-Id: 1\r\nContent-Length: "...)
	out = strconv.AppendInt(out, int64(len(body)), 10)
	out = append(out, "\r\nConnection: keep-alive\r\n\r\n"...)
	out = append(out, body...)
	return out
}

// dispatch routes every request across /healthz, /api/v3, /api/graphql, /mcp, and /{owner}/{repo}.git/*.
func (s *EpollServer) dispatch(req *Request) []byte {
	p := req.Path
	// Fast path 1: REST API (/api/v3...) and GraphQL (/api/graphql)
	if len(p) >= 7 && p[0] == '/' && p[1] == 'a' && p[2] == 'p' && p[3] == 'i' && p[4] == '/' {
		if p[5] == 'v' && p[6] == '3' && (len(p) == 7 || p[7] == '/') {
			return s.handleREST(req)
		}
		if string(p) == "/api/graphql" {
			return s.handleGraphQL(req)
		}
	}
	// Fast path 2: /healthz (no auth required)
	if len(p) == 8 && string(p) == "/healthz" {
		if string(req.Method) == "GET" {
			return s.buildJSONResp(200, "{\"ok\":true}\n", nil, req.KeepAlive)
		}
		return s.buildJSONResp(405, "{\"message\":\"Method Not Allowed\"}\n", nil, req.KeepAlive)
	}
	// Fast path 3: /mcp
	if len(p) == 4 && string(p) == "/mcp" {
		return s.handleMCP(req)
	}
	// Fast path 4: Git Smart HTTP /{owner}/{repo}.git/...
	if bytes.Contains(p, []byte(".git/")) {
		return s.handleGitHTTP(req)
	}
	return s.buildJSONResp(404, "{\"message\":\"Not Found\"}\n", nil, req.KeepAlive)
}
