package forge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
)

const (
	initReadCap    = 16384
	initWriteCap   = 8192
	maxPoolRetain  = 65536
	maxReqSize     = 64 << 20 // 64 MiB safety cap
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

	// Per-connection reusable output/scratch buffers (owned by the connection goroutine).
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

type connScratch struct {
	rbuf []byte
	wbuf []byte
	tmp  []byte
}

var connScratchPool = sync.Pool{
	New: func() any {
		return &connScratch{
			rbuf: make([]byte, initReadCap),
			wbuf: make([]byte, 0, initWriteCap),
			tmp:  make([]byte, 0, 2048),
		}
	},
}

type lastReadCache struct {
	path      []byte
	resp      []byte
	issue     *Issue
	repoEpoch uint64
}

// EpollServer is a multi-goroutine raw TCP HTTP/1.1 server powered by Go's runtime netpoller
// with lock-free atomic hot paths across both container CPUs.
type EpollServer struct {
	store    *Store
	ln       net.Listener
	port     int
	closed   atomic.Bool
	lastRead atomic.Pointer[lastReadCache]
}

func NewEpollServer(store *Store, bindPort int) (*EpollServer, error) {
	lc := net.ListenConfig{KeepAlive: -1}
	addr := fmt.Sprintf("0.0.0.0:%d", bindPort)
	ln, err := lc.Listen(context.Background(), "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	actualPort := ln.Addr().(*net.TCPAddr).Port
	return &EpollServer{
		store: store,
		ln:    ln,
		port:  actualPort,
	}, nil
}

func (s *EpollServer) Port() int {
	return s.port
}

func (s *EpollServer) Close() {
	if s.closed.Swap(true) {
		return
	}
	if s.ln != nil {
		_ = s.ln.Close()
	}
}

func (s *EpollServer) Serve() error {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if s.closed.Load() || errors.Is(err, net.ErrClosed) {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		go s.serveConn(conn)
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

func writeAll(conn net.Conn, data []byte) error {
	for len(data) > 0 {
		n, err := conn.Write(data)
		if n > 0 {
			data = data[n:]
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *EpollServer) serveConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	sc := connScratchPool.Get().(*connScratch)
	defer func() {
		if cap(sc.rbuf) > maxPoolRetain {
			sc.rbuf = make([]byte, initReadCap)
		}
		if cap(sc.wbuf) > maxPoolRetain {
			sc.wbuf = make([]byte, 0, initWriteCap)
		}
		if cap(sc.tmp) > maxPoolRetain {
			sc.tmp = make([]byte, 0, 2048)
		}
		connScratchPool.Put(sc)
	}()

	rbuf := sc.rbuf
	rpos := 0
	rlen := 0
	sent100 := false
	var req Request

	for {
		// Parse and dispatch all complete HTTP/1.1 requests already buffered.
		for rpos < rlen {
			avail := rbuf[rpos:rlen]
			hdrEnd := bytes.Index(avail, crlf2)
			if hdrEnd < 0 {
				break
			}
			hdrBytes := avail[:hdrEnd]
			bodyStart := hdrEnd + 4

			lineEnd := bytes.IndexByte(hdrBytes, '\r')
			if lineEnd < 0 {
				_ = writeAll(conn, staticBadReqClose)
				return
			}
			reqLine := hdrBytes[:lineEnd]
			sp1 := bytes.IndexByte(reqLine, ' ')
			if sp1 <= 0 {
				_ = writeAll(conn, staticBadReqClose)
				return
			}
			method := reqLine[:sp1]
			rest := reqLine[sp1+1:]
			sp2 := bytes.IndexByte(rest, ' ')
			if sp2 <= 0 {
				_ = writeAll(conn, staticBadReqClose)
				return
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
							_ = writeAll(conn, staticBadReqClose)
							return
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

			if expect100 && !sent100 && len(avail)-bodyStart == 0 {
				sent100 = true
				if err := writeAll(conn, continue100); err != nil {
					return
				}
			}

			var body []byte
			var totalConsumed int
			if chunked {
				decoded, consumed, done, bad := decodeChunked(avail[bodyStart:])
				if bad {
					_ = writeAll(conn, staticBadReqClose)
					return
				}
				if !done {
					break
				}
				body = decoded
				totalConsumed = bodyStart + consumed
			} else if hasContentLen {
				if len(avail)-bodyStart < contentLen {
					// Pre-grow read buffer to fit the full body in minimal reads.
					needed := rpos + bodyStart + contentLen
					if needed > len(rbuf) {
						if rpos > 0 {
							copy(rbuf, rbuf[rpos:rlen])
							rlen -= rpos
							rpos = 0
							needed = bodyStart + contentLen
						}
						if needed > len(rbuf) {
							newCap := len(rbuf) * 2
							for newCap < needed {
								newCap *= 2
							}
							if newCap > maxReqSize {
								newCap = maxReqSize
							}
							nb := make([]byte, newCap)
							copy(nb, rbuf[:rlen])
							rbuf = nb
							sc.rbuf = nb
						}
					}
					break
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
				Out:         sc.wbuf[:0],
				Tmp:         sc.tmp[:0],
			}

			resp := s.dispatch(&req)
			sc.wbuf = req.Out
			sc.tmp = req.Tmp

			sent100 = false
			rpos += totalConsumed
			if rpos == rlen {
				rpos = 0
				rlen = 0
			}

			if err := writeAll(conn, resp); err != nil {
				return
			}
			if !keepAlive {
				return
			}
		}

		// Need more bytes from the socket.
		if rlen == len(rbuf) {
			if rpos > 0 {
				copy(rbuf, rbuf[rpos:rlen])
				rlen -= rpos
				rpos = 0
			} else {
				newCap := len(rbuf) * 2
				if newCap > maxReqSize {
					return
				}
				nb := make([]byte, newCap)
				copy(nb, rbuf[:rlen])
				rbuf = nb
				sc.rbuf = nb
			}
		}

		n, err := conn.Read(rbuf[rlen:])
		if n > 0 {
			rlen += n
		}
		if err != nil {
			if err == io.EOF && rpos < rlen {
				// Process any remaining buffered request before closing.
				continue
			}
			return
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

// dispatchFast handles the hot read/clone/push-adv endpoints completely lock-free.
// Returns nil on cache miss or non-fast-path requests.
func (s *EpollServer) dispatchFast(req *Request) []byte {
	if !req.KeepAlive {
		return nil
	}
	p := req.Path
	m := req.Method

	// Fast path 1: GET /api/v3/repos/{owner}/{repo}/issues/{n} (bench.read)
	if len(m) == 3 && m[0] == 'G' && len(p) > 22 && string(p[:14]) == "/api/v3/repos/" {
		if !s.store.CheckAuth(req.Auth) {
			return staticUnauthKeep
		}
		if lr := s.lastRead.Load(); lr != nil && lr.repoEpoch == s.store.repoEpoch.Load() && bytes.Equal(p, lr.path) {
			if lr.issue.cachedGetResp.Load() != nil {
				return lr.resp
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
								return staticNotFoundKeep
							}
							if snap := repo.issuesSnap.Load(); snap != nil && num <= len(*snap) {
								is := (*snap)[num-1]
								if cached := is.cachedGetResp.Load(); cached != nil {
									pathCopy := make([]byte, len(p))
									copy(pathCopy, p)
									s.lastRead.Store(&lastReadCache{
										path:      pathCopy,
										resp:      *cached,
										issue:     is,
										repoEpoch: s.store.repoEpoch.Load(),
									})
									return *cached
								}
							}
						}
					}
				}
			}
		}
		return nil
	}

	// Fast path 2: /healthz
	if len(p) == 8 && string(p) == "/healthz" && len(m) == 3 && m[0] == 'G' {
		return staticHealthzHTTP
	}

	// Fast path 3: Git Smart HTTP /{owner}/{repo}.git/... (bench.clone & bench.push GET)
	if dotGit := bytes.Index(p, dotGitSlash); dotGit > 2 {
		rest := p[1:dotGit]
		if slash1 := bytes.IndexByte(rest, '/'); slash1 > 0 {
			if !s.store.CheckAuth(req.Auth) {
				return staticUnauthKeep
			}
			owner := rest[:slash1]
			repoName := rest[slash1+1:]
			subPath := p[dotGit+5:]
			repo := s.store.GetRepoLockFree(owner, repoName)
			if repo == nil {
				return staticNotFoundKeep
			}
			if len(m) == 3 && m[0] == 'G' && string(subPath) == "info/refs" {
				if bytes.Equal(req.Query, qUploadPack) {
					if bytes.Contains(req.GitProtocol, v2ProtoBytes) {
						return staticUploadV2HTTP
					}
					if cached := repo.upV0Cache.Load(); cached != nil {
						return *cached
					}
				} else if bytes.Equal(req.Query, qReceivePack) {
					if cached := repo.recvCache.Load(); cached != nil {
						return *cached
					}
				}
				return nil
			}
			if len(m) == 4 && m[0] == 'P' && string(subPath) == "git-upload-pack" {
				if bytes.Contains(req.GitProtocol, v2ProtoBytes) {
					if fc := repo.fetchCache.Load(); fc != nil {
						entries := *fc
						for i := range entries {
							if bytes.Equal(entries[i].key, req.Body) {
								return entries[i].resp
							}
						}
					}
				}
			}
		}
	}
	return nil
}

// dispatch routes every request across /healthz, /api/v3, /api/graphql, /mcp, and /{owner}/{repo}.git/*.
func (s *EpollServer) dispatch(req *Request) []byte {
	if resp := s.dispatchFast(req); resp != nil {
		return resp
	}

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
	if bytes.Contains(p, []byte(".git/")) {
		return s.handleGitHTTP(req)
	}
	return s.buildJSONResp(404, "{\"message\":\"Not Found\"}\n", nil, req.KeepAlive)
}
