package forge

import (
	"bytes"
	"sort"
	"strconv"
	"strings"
)

const (
	pktFlush = "0000"
	pktDelim = "0001"
)

func appendPktLine(dst []byte, payload []byte) []byte {
	n := len(payload) + 4
	var hdr [4]byte
	hdr[0] = hexDigits[(n>>12)&0xf]
	hdr[1] = hexDigits[(n>>8)&0xf]
	hdr[2] = hexDigits[(n>>4)&0xf]
	hdr[3] = hexDigits[n&0xf]
	dst = append(dst, hdr[:]...)
	return append(dst, payload...)
}

func appendPktLineStr(dst []byte, payload string) []byte {
	n := len(payload) + 4
	var hdr [4]byte
	hdr[0] = hexDigits[(n>>12)&0xf]
	hdr[1] = hexDigits[(n>>8)&0xf]
	hdr[2] = hexDigits[(n>>4)&0xf]
	hdr[3] = hexDigits[n&0xf]
	dst = append(dst, hdr[:]...)
	return append(dst, payload...)
}

func appendSideband(dst []byte, band byte, data []byte) []byte {
	const maxChunk = 65500
	for len(data) > 0 {
		chunk := data
		if len(chunk) > maxChunk {
			chunk = data[:maxChunk]
		}
		n := len(chunk) + 5
		var hdr [5]byte
		hdr[0] = hexDigits[(n>>12)&0xf]
		hdr[1] = hexDigits[(n>>8)&0xf]
		hdr[2] = hexDigits[(n>>4)&0xf]
		hdr[3] = hexDigits[n&0xf]
		hdr[4] = band
		dst = append(dst, hdr[:]...)
		dst = append(dst, chunk...)
		data = data[len(chunk):]
	}
	return dst
}

// Static pre-built HTTP/1.1 200 OK responses for Git Smart HTTP hot paths.
var (
	staticUploadV2HTTP = func() []byte {
		var body []byte
		body = appendPktLineStr(body, "version 2\n")
		body = appendPktLineStr(body, "agent=hyperforge/1\n")
		body = appendPktLineStr(body, "ls-refs=unborn\n")
		body = appendPktLineStr(body, "fetch=shallow\n")
		body = appendPktLineStr(body, "object-format=sha1\n")
		body = append(body, pktFlush...)
		return buildImmutableGitResp(200, "application/x-git-upload-pack-advertisement", body)
	}()

	staticPushMainPlainResp = func() []byte {
		var report []byte
		report = appendPktLineStr(report, "unpack ok\n")
		report = appendPktLineStr(report, "ok refs/heads/main\n")
		report = append(report, pktFlush...)
		return buildImmutableGitResp(200, "application/x-git-receive-pack-result", report)
	}()

	staticPushMainSidebandResp = func() []byte {
		var report []byte
		report = appendPktLineStr(report, "unpack ok\n")
		report = appendPktLineStr(report, "ok refs/heads/main\n")
		report = append(report, pktFlush...)
		var body []byte
		body = appendSideband(body, 1, report)
		body = append(body, pktFlush...)
		return buildImmutableGitResp(200, "application/x-git-receive-pack-result", body)
	}()
)

func (s *EpollServer) handleGitHTTP(req *Request) []byte {
	if !s.store.CheckAuth(req.Auth) {
		return s.buildJSONResp(401, "{\"message\":\"Requires authentication\"}\n", nil, req.KeepAlive)
	}
	// Parse /{owner}/{repo}.git/{rest}
	p := req.Path
	if len(p) == 0 || p[0] != '/' {
		return s.buildJSONResp(404, "{\"message\":\"Not Found\"}\n", nil, req.KeepAlive)
	}
	rest := p[1:]
	slash1 := bytes.IndexByte(rest, '/')
	if slash1 <= 0 {
		return s.buildJSONResp(404, "{\"message\":\"Not Found\"}\n", nil, req.KeepAlive)
	}
	owner := rest[:slash1]
	afterOwner := rest[slash1+1:]
	dotGit := bytes.Index(afterOwner, []byte(".git/"))
	if dotGit <= 0 {
		return s.buildJSONResp(404, "{\"message\":\"Not Found\"}\n", nil, req.KeepAlive)
	}
	repoName := afterOwner[:dotGit]
	subPath := afterOwner[dotGit+5:]

	repo := s.store.GetRepoBytes(owner, repoName)
	if repo == nil {
		return s.buildJSONResp(404, "{\"message\":\"Not Found\"}\n", nil, req.KeepAlive)
	}

	if string(req.Method) == "GET" && string(subPath) == "info/refs" {
		return s.handleGitInfoRefs(req, repo)
	}
	if string(req.Method) == "POST" {
		switch string(subPath) {
		case "git-upload-pack":
			return s.handleGitUploadPack(req, repo)
		case "git-receive-pack":
			return s.handleGitReceivePack(req, repo)
		}
	}
	return s.buildJSONResp(404, "{\"message\":\"Not Found\"}\n", nil, req.KeepAlive)
}

var (
	qUploadPack  = []byte("service=git-upload-pack")
	qReceivePack = []byte("service=git-receive-pack")
	v2ProtoBytes = []byte("version=2")
)

func (s *EpollServer) handleGitInfoRefs(req *Request, repo *Repo) []byte {
	isUpload := bytes.Equal(req.Query, qUploadPack)
	isReceive := !isUpload && bytes.Equal(req.Query, qReceivePack)
	if !isUpload && !isReceive {
		svc := req.QueryParam("service")
		isUpload = svc == "git-upload-pack"
		isReceive = svc == "git-receive-pack"
	}

	if isUpload {
		if bytes.Contains(req.GitProtocol, v2ProtoBytes) {
			if req.KeepAlive {
				return staticUploadV2HTTP
			}
			var body []byte
			body = appendPktLineStr(body, "version 2\n")
			body = appendPktLineStr(body, "agent=hyperforge/1\n")
			body = appendPktLineStr(body, "ls-refs=unborn\n")
			body = appendPktLineStr(body, "fetch=shallow\n")
			body = appendPktLineStr(body, "object-format=sha1\n")
			body = append(body, pktFlush...)
			return s.buildResp(200, "application/x-git-upload-pack-advertisement", body, nil, false)
		}
		if req.KeepAlive {
			if cached := repo.upV0Cache.Load(); cached != nil {
				return *cached
			}
		}
		body := buildV0RefsBody(repo, "git-upload-pack", "multi_ack thin-pack side-band side-band-64k ofs-delta shallow no-progress include-tag symref=HEAD:"+repo.HeadRef+" agent=hyperforge/1", true)
		if req.KeepAlive {
			httpBytes := buildImmutableGitResp(200, "application/x-git-upload-pack-advertisement", body)
			repo.upV0Cache.Store(&httpBytes)
			return httpBytes
		}
		return s.buildGitResp(200, "application/x-git-upload-pack-advertisement", body, false)
	}

	if isReceive {
		if req.KeepAlive {
			if cached := repo.recvCache.Load(); cached != nil {
				return *cached
			}
		}
		body := buildV0RefsBody(repo, "git-receive-pack", "report-status report-status-v2 delete-refs side-band-64k ofs-delta agent=hyperforge/1", false)
		if req.KeepAlive {
			httpBytes := buildImmutableGitResp(200, "application/x-git-receive-pack-advertisement", body)
			singleRef := ""
			oidOff := 0
			if len(repo.Refs) == 1 {
				for refName, oid := range repo.Refs {
					needle := oid.Hex() + " " + refName + "\x00"
					if idx := bytes.Index(httpBytes, []byte(needle)); idx >= 0 {
						singleRef = refName
						oidOff = idx
					}
				}
			}
			repo.setRecvCache(httpBytes, singleRef, oidOff)
			return httpBytes
		}
		return s.buildGitResp(200, "application/x-git-receive-pack-advertisement", body, false)
	}
	return s.buildJSONResp(400, "{\"message\":\"unknown service\"}\n", nil, req.KeepAlive)
}

func peelTag(repo *Repo, oid OID) (OID, bool) {
	cur := oid
	peeled := false
	for i := 0; i < 8; i++ {
		obj, ok := repo.Objects[cur]
		if !ok || obj.Type != ObjTag {
			break
		}
		parseTagInto(obj)
		cur = obj.TagTarget
		peeled = true
	}
	return cur, peeled
}

func buildV0RefsBody(repo *Repo, service, caps string, includeHEADAndPeel bool) []byte {
	var body []byte
	body = appendPktLineStr(body, "# service="+service+"\n")
	body = append(body, pktFlush...)

	names := make([]string, 0, len(repo.Refs))
	for name := range repo.Refs {
		names = append(names, name)
	}
	sort.Strings(names)

	first := true
	if includeHEADAndPeel {
		if headOID, ok := repo.Refs[repo.HeadRef]; ok {
			line := headOID.Hex() + " HEAD\x00" + caps + "\n"
			body = appendPktLineStr(body, line)
			first = false
		}
	}

	if len(names) == 0 && first {
		line := ZeroOIDHex + " capabilities^{}\x00" + caps + "\n"
		body = appendPktLineStr(body, line)
	} else {
		for _, name := range names {
			oid := repo.Refs[name]
			if first {
				body = appendPktLineStr(body, oid.Hex()+" "+name+"\x00"+caps+"\n")
				first = false
			} else {
				body = appendPktLineStr(body, oid.Hex()+" "+name+"\n")
			}
			if includeHEADAndPeel {
				if target, peeled := peelTag(repo, oid); peeled {
					body = appendPktLineStr(body, target.Hex()+" "+name+"^{}\n")
				}
			}
		}
	}
	body = append(body, pktFlush...)
	return body
}

// parsePktLines decodes pkt-lines from body. Stops at end of buffer or when stopAtFirstFlush is true.
// Returns packets ([]byte nil for flush "0000", special sentinel for delim "0001"), trailing offset, and error flag.
type pktItem struct {
	kind byte // 0=data, 1=flush, 2=delim
	data []byte
}

func parsePktLines(src []byte, stopAtFlush bool) ([]pktItem, int, bool) {
	return parsePktLinesInto(src, stopAtFlush, nil)
}

func parsePktLinesInto(src []byte, stopAtFlush bool, out []pktItem) ([]pktItem, int, bool) {
	pos := 0
	for pos < len(src) {
		if pos+4 > len(src) {
			return nil, pos, false
		}
		h0 := hexValTable[src[pos]]
		h1 := hexValTable[src[pos+1]]
		h2 := hexValTable[src[pos+2]]
		h3 := hexValTable[src[pos+3]]
		if (h0|h1|h2|h3)&0x80 != 0 {
			return nil, pos, false
		}
		n := (int(h0) << 12) | (int(h1) << 8) | (int(h2) << 4) | int(h3)
		pos += 4
		if n == 0 {
			out = append(out, pktItem{kind: 1})
			if stopAtFlush {
				return out, pos, true
			}
			continue
		}
		if n == 1 {
			out = append(out, pktItem{kind: 2})
			continue
		}
		if n == 2 {
			continue
		}
		if n < 4 || pos+n-4 > len(src) {
			return nil, pos, false
		}
		out = append(out, pktItem{kind: 0, data: src[pos : pos+n-4]})
		pos += n - 4
	}
	return out, pos, true
}

func stripLF(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\n' {
		return b[:len(b)-1]
	}
	return b
}

func (s *EpollServer) handleGitUploadPack(req *Request, repo *Repo) []byte {
	isV2 := bytes.Contains(req.GitProtocol, v2ProtoBytes)
	if isV2 && req.KeepAlive {
		if fc := repo.fetchCache.Load(); fc != nil {
			for i := range *fc {
				if bytes.Equal((*fc)[i].key, req.Body) {
					return (*fc)[i].resp
				}
			}
		}
	}

	pkts, _, ok := parsePktLines(req.Body, false)
	if !ok || len(pkts) == 0 {
		return s.buildJSONResp(400, "{\"message\":\"malformed pkt-line\"}\n", nil, req.KeepAlive)
	}

	if isV2 {
		return s.handleUploadPackV2(req, repo, pkts)
	}
	return s.handleUploadPackV0(req, repo, pkts)
}

func (s *EpollServer) handleUploadPackV2(req *Request, repo *Repo, pkts []pktItem) []byte {
	cmd := ""
	inArgs := false
	var args [][]byte
	for _, p := range pkts {
		if p.kind == 2 {
			inArgs = true
			continue
		}
		if p.kind == 1 {
			break
		}
		line := stripLF(p.data)
		if !inArgs {
			if bytes.HasPrefix(line, []byte("command=")) {
				cmd = string(line[8:])
			}
		} else {
			args = append(args, line)
		}
	}

	switch cmd {
	case "ls-refs":
		peel := false
		symrefs := false
		unborn := false
		var prefixes []string
		for _, a := range args {
			switch {
			case string(a) == "peel":
				peel = true
			case string(a) == "symrefs":
				symrefs = true
			case string(a) == "unborn":
				unborn = true
			case bytes.HasPrefix(a, []byte("ref-prefix ")):
				prefixes = append(prefixes, string(a[11:]))
			}
		}
		matches := func(name string) bool {
			if len(prefixes) == 0 {
				return true
			}
			for _, pf := range prefixes {
				if strings.HasPrefix(name, pf) {
					return true
				}
			}
			return false
		}
		var body []byte
		if matches("HEAD") {
			if headOID, ok := repo.Refs[repo.HeadRef]; ok {
				line := headOID.Hex() + " HEAD"
				if symrefs {
					line += " symref-target:" + repo.HeadRef
				}
				body = appendPktLineStr(body, line+"\n")
			} else if unborn {
				line := "unborn HEAD"
				if symrefs {
					line += " symref-target:" + repo.HeadRef
				}
				body = appendPktLineStr(body, line+"\n")
			}
		}
		names := make([]string, 0, len(repo.Refs))
		for name := range repo.Refs {
			if matches(name) {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		for _, name := range names {
			oid := repo.Refs[name]
			line := oid.Hex() + " " + name
			if peel {
				if target, peeled := peelTag(repo, oid); peeled {
					line += " peeled:" + target.Hex()
				}
			}
			body = appendPktLineStr(body, line+"\n")
		}
		body = append(body, pktFlush...)

		if req.KeepAlive {
			resp := buildImmutableGitResp(200, "application/x-git-upload-pack-result", body)
			repo.putFetchCache(req.Body, resp)
			return resp
		}
		return s.buildGitResp(200, "application/x-git-upload-pack-result", body, false)

	case "fetch":
		var wants, haves, clientShallows []OID
		depth := 0
		for _, a := range args {
			switch {
			case bytes.HasPrefix(a, []byte("want ")):
				if oid, ok := ParseOIDBytes(a[5:]); ok {
					wants = append(wants, oid)
				}
			case bytes.HasPrefix(a, []byte("have ")):
				if oid, ok := ParseOIDBytes(a[5:]); ok {
					haves = append(haves, oid)
				}
			case bytes.HasPrefix(a, []byte("shallow ")):
				if oid, ok := ParseOIDBytes(a[8:]); ok {
					clientShallows = append(clientShallows, oid)
				}
			case bytes.HasPrefix(a, []byte("deepen ")):
				if v, err := strconv.Atoi(string(a[7:])); err == nil && v > 0 {
					depth = v
				}
			}
		}
		objs, newShallows, unshallows := collectFetchObjects(repo, wants, haves, clientShallows, depth)
		pack := s.store.BuildPack(objs)

		var body []byte
		if len(newShallows) > 0 || len(unshallows) > 0 {
			body = appendPktLineStr(body, "shallow-info\n")
			for _, o := range newShallows {
				body = appendPktLineStr(body, "shallow "+o.Hex()+"\n")
			}
			for _, o := range unshallows {
				body = appendPktLineStr(body, "unshallow "+o.Hex()+"\n")
			}
			body = append(body, pktDelim...)
		}
		body = appendPktLineStr(body, "packfile\n")
		body = appendSideband(body, 1, pack)
		body = append(body, pktFlush...)

		if req.KeepAlive {
			resp := buildImmutableGitResp(200, "application/x-git-upload-pack-result", body)
			repo.putFetchCache(req.Body, resp)
			return resp
		}
		return s.buildGitResp(200, "application/x-git-upload-pack-result", body, false)

	default:
		return s.buildJSONResp(400, "{\"message\":\"unknown v2 command\"}\n", nil, req.KeepAlive)
	}
}

func (s *EpollServer) handleUploadPackV0(req *Request, repo *Repo, pkts []pktItem) []byte {
	var wants, haves, clientShallows []OID
	depth := 0
	for _, p := range pkts {
		if p.kind != 0 {
			continue
		}
		line := stripLF(p.data)
		switch {
		case bytes.HasPrefix(line, []byte("want ")):
			if len(line) >= 45 {
				if oid, ok := ParseOIDBytes(line[5:45]); ok {
					wants = append(wants, oid)
				}
			}
		case bytes.HasPrefix(line, []byte("have ")):
			if len(line) >= 45 {
				if oid, ok := ParseOIDBytes(line[5:45]); ok {
					haves = append(haves, oid)
				}
			}
		case bytes.HasPrefix(line, []byte("shallow ")):
			if len(line) >= 48 {
				if oid, ok := ParseOIDBytes(line[8:48]); ok {
					clientShallows = append(clientShallows, oid)
				}
			}
		case bytes.HasPrefix(line, []byte("deepen ")):
			if v, err := strconv.Atoi(string(line[7:])); err == nil && v > 0 {
				depth = v
			}
		}
	}

	objs, newShallows, unshallows := collectFetchObjects(repo, wants, haves, clientShallows, depth)
	pack := s.store.BuildPack(objs)

	var body []byte
	if len(newShallows) > 0 || len(unshallows) > 0 {
		for _, o := range newShallows {
			body = appendPktLineStr(body, "shallow "+o.Hex()+"\n")
		}
		for _, o := range unshallows {
			body = appendPktLineStr(body, "unshallow "+o.Hex()+"\n")
		}
		body = append(body, pktFlush...)
	}
	body = appendPktLineStr(body, "NAK\n")
	body = appendSideband(body, 1, pack)
	body = append(body, pktFlush...)
	return s.buildResp(200, "application/x-git-upload-pack-result", body, nil, req.KeepAlive)
}

func collectTreeAndBlobs(repo *Repo, treeOID OID, seen map[OID]struct{}, exclude map[OID]struct{}, out *[]*GitObj) {
	if treeOID == ZeroOID {
		return
	}
	if _, ex := exclude[treeOID]; ex {
		return
	}
	if _, ok := seen[treeOID]; ok {
		return
	}
	obj, ok := repo.Objects[treeOID]
	if !ok {
		return
	}
	seen[treeOID] = struct{}{}
	if out != nil {
		*out = append(*out, obj)
	}
	if obj.Type != ObjTree {
		return
	}
	for _, e := range ParseTree(obj.Data) {
		if _, ex := exclude[e.OID]; ex {
			continue
		}
		if _, dup := seen[e.OID]; dup {
			continue
		}
		if e.Mode == "40000" || e.Mode == "040000" {
			collectTreeAndBlobs(repo, e.OID, seen, exclude, out)
		} else {
			seen[e.OID] = struct{}{}
			if child, ok := repo.Objects[e.OID]; ok && out != nil {
				*out = append(*out, child)
			}
		}
	}
}

func collectReachableWithStop(repo *Repo, roots []OID, stopCommits map[OID]struct{}) map[OID]struct{} {
	seen := make(map[OID]struct{}, 64)
	queue := append([]OID(nil), roots...)
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if _, ok := seen[cur]; ok {
			continue
		}
		obj, ok := repo.Objects[cur]
		if !ok {
			continue
		}
		seen[cur] = struct{}{}
		switch obj.Type {
		case ObjTag:
			parseTagInto(obj)
			queue = append(queue, obj.TagTarget)
		case ObjCommit:
			parseCommitInto(obj)
			collectTreeAndBlobs(repo, obj.TreeOID, seen, nil, nil)
			if _, stop := stopCommits[cur]; !stop {
				queue = obj.AppendParents(queue)
			}
		case ObjTree:
			for _, e := range ParseTree(obj.Data) {
				if e.Mode == "40000" || e.Mode == "040000" {
					collectTreeAndBlobs(repo, e.OID, seen, nil, nil)
				} else {
					seen[e.OID] = struct{}{}
				}
			}
		}
	}
	return seen
}

func collectFetchObjects(repo *Repo, wants, haves, clientShallows []OID, depth int) (objs []*GitObj, newShallows []OID, unshallows []OID) {
	clientShallowSet := make(map[OID]struct{}, len(clientShallows))
	for _, cs := range clientShallows {
		clientShallowSet[cs] = struct{}{}
	}
	exclude := collectReachableWithStop(repo, haves, clientShallowSet)

	seen := make(map[OID]struct{}, 64)
	if depth <= 0 {
		// Full or incremental fetch
		queue := append([]OID(nil), wants...)
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			if _, ex := exclude[cur]; ex {
				continue
			}
			if _, dup := seen[cur]; dup {
				continue
			}
			obj, ok := repo.Objects[cur]
			if !ok {
				continue
			}
			seen[cur] = struct{}{}
			objs = append(objs, obj)
			switch obj.Type {
			case ObjTag:
				parseTagInto(obj)
				queue = append(queue, obj.TagTarget)
			case ObjCommit:
				parseCommitInto(obj)
				collectTreeAndBlobs(repo, obj.TreeOID, seen, exclude, &objs)
				queue = obj.AppendParents(queue)
			case ObjTree:
				for _, e := range ParseTree(obj.Data) {
					if e.Mode == "40000" || e.Mode == "040000" {
						collectTreeAndBlobs(repo, e.OID, seen, exclude, &objs)
					} else if _, ex := exclude[e.OID]; !ex {
						if _, dup := seen[e.OID]; !dup {
							seen[e.OID] = struct{}{}
							if child, ok := repo.Objects[e.OID]; ok {
								objs = append(objs, child)
							}
						}
					}
				}
			}
		}
		return objs, nil, nil
	}

	// Shallow fetch (deepen N): BFS commits tracking min depth from wants (1-based)
	type item struct {
		oid   OID
		level int
	}
	commitDepth := make(map[OID]int, 16)
	var q []item
	for _, w := range wants {
		obj, ok := repo.Objects[w]
		if !ok {
			continue
		}
		if obj.Type == ObjTag {
			if _, ex := exclude[w]; !ex {
				if _, dup := seen[w]; !dup {
					seen[w] = struct{}{}
					objs = append(objs, obj)
				}
			}
			target, _ := peelTag(repo, w)
			q = append(q, item{oid: target, level: 1})
		} else {
			q = append(q, item{oid: w, level: 1})
		}
	}

	newShallowSet := make(map[OID]struct{}, 4)
	for len(q) > 0 {
		cur := q[0]
		q = q[1:]
		if prev, ok := commitDepth[cur.oid]; ok && prev <= cur.level {
			continue
		}
		commitDepth[cur.oid] = cur.level
		obj, ok := repo.Objects[cur.oid]
		if !ok || obj.Type != ObjCommit {
			continue
		}
		parseCommitInto(obj)

		if _, ex := exclude[cur.oid]; !ex {
			if _, dup := seen[cur.oid]; !dup {
				seen[cur.oid] = struct{}{}
				objs = append(objs, obj)
			}
		}
		collectTreeAndBlobs(repo, obj.TreeOID, seen, exclude, &objs)

		if cur.level >= depth {
			if obj.HasParents() {
				newShallowSet[cur.oid] = struct{}{}
			}
			continue
		}
		if obj.HasParent0 {
			q = append(q, item{oid: obj.Parent0, level: cur.level + 1})
		}
		for _, p := range obj.Parents {
			q = append(q, item{oid: p, level: cur.level + 1})
		}
	}

	for oid := range newShallowSet {
		newShallows = append(newShallows, oid)
	}
	for _, cs := range clientShallows {
		if d, ok := commitDepth[cs]; ok && d < depth {
			if _, stillShallow := newShallowSet[cs]; !stillShallow {
				unshallows = append(unshallows, cs)
			}
		}
	}
	return objs, newShallows, unshallows
}

type pushCmd struct {
	oldOID OID
	newOID OID
	ref    string
}

func (s *EpollServer) handleGitReceivePack(req *Request, repo *Repo) []byte {
	var stackPkts [4]pktItem
	pkts, packOff, ok := parsePktLinesInto(req.Body, true, stackPkts[:0])
	if !ok {
		return s.buildJSONResp(400, "{\"message\":\"malformed receive-pack pkt-line\"}\n", nil, req.KeepAlive)
	}

	var stackCmds [4]pushCmd
	cmds := stackCmds[:0]
	useSideband := false
	for i, p := range pkts {
		if p.kind != 0 {
			continue
		}
		line := stripLF(p.data)
		if i == 0 {
			if nul := bytes.IndexByte(line, 0); nul >= 0 {
				caps := line[nul+1:]
				if bytes.Contains(caps, []byte("side-band-64k")) {
					useSideband = true
				}
				line = line[:nul]
			}
		}
		// "<40 hex> <40 hex> <ref>"
		if len(line) < 83 || line[40] != ' ' || line[81] != ' ' {
			return s.buildJSONResp(400, "{\"message\":\"invalid receive-pack command\"}\n", nil, req.KeepAlive)
		}
		oldOID, ok1 := ParseOIDBytes(line[:40])
		newOID, ok2 := ParseOIDBytes(line[41:81])
		if !ok1 || !ok2 {
			return s.buildJSONResp(400, "{\"message\":\"invalid oid in command\"}\n", nil, req.KeepAlive)
		}
		refBytes := line[82:]
		refStr := "refs/heads/main"
		if string(refBytes) != "refs/heads/main" {
			refStr = string(refBytes)
		}
		cmds = append(cmds, pushCmd{
			oldOID: oldOID,
			newOID: newOID,
			ref:    refStr,
		})
	}

	packBytes := req.Body[packOff:]
	unpackErr := s.store.UnpackPackfile(repo, packBytes)

	// Ultra-fast zero-alloc path for single fast-forward update on refs/heads/main (bench.push).
	if unpackErr == nil && len(cmds) == 1 && cmds[0].ref == "refs/heads/main" && cmds[0].newOID != ZeroOID {
		cmd := cmds[0]
		if _, existsObj := repo.Objects[cmd.newOID]; existsObj {
			cur, exists := repo.Refs["refs/heads/main"]
			if exists && cur == cmd.oldOID && IsAncestor(repo, cur, cmd.newOID) {
				repo.Refs["refs/heads/main"] = cmd.newOID
				repo.PatchSingleRefCache("refs/heads/main", cmd.newOID)
				if req.KeepAlive {
					if useSideband {
						return staticPushMainSidebandResp
					}
					return staticPushMainPlainResp
				}
			}
		}
	}

	report := s.store.reportBuf[:0]
	if unpackErr != nil {
		report = appendPktLineStr(report, "unpack "+unpackErr.Error()+"\n")
		for _, cmd := range cmds {
			report = appendPktLineStr(report, "ng "+cmd.ref+" unpacker error\n")
		}
	} else {
		report = appendPktLineStr(report, "unpack ok\n")
		changedCount := 0
		var lastChangedRef string
		var lastChangedOID OID
		for _, cmd := range cmds {
			if cmd.newOID == ZeroOID {
				// Delete ref
				cur, exists := repo.Refs[cmd.ref]
				if !exists {
					report = appendPktLineStr(report, "ng "+cmd.ref+" ref not found\n")
					continue
				}
				if cmd.oldOID != ZeroOID && cur != cmd.oldOID {
					report = appendPktLineStr(report, "ng "+cmd.ref+" non-fast-forward\n")
					continue
				}
				delete(repo.Refs, cmd.ref)
				changedCount = 2 // force full cache invalidation on delete
				report = appendPktLineStr(report, "ok "+cmd.ref+"\n")
				continue
			}

			// Create or update ref
			if _, existsObj := repo.Objects[cmd.newOID]; !existsObj {
				report = appendPktLineStr(report, "ng "+cmd.ref+" missing object\n")
				continue
			}
			cur, exists := repo.Refs[cmd.ref]
			if !exists {
				if cmd.oldOID != ZeroOID {
					report = appendPktLineStr(report, "ng "+cmd.ref+" non-fast-forward\n")
					continue
				}
				repo.Refs[cmd.ref] = cmd.newOID
				changedCount = 2 // new ref added
				report = appendPktLineStr(report, "ok "+cmd.ref+"\n")
				continue
			}
			// Existing ref: must match oldOID and fast-forward
			if cur != cmd.oldOID {
				report = appendPktLineStr(report, "ng "+cmd.ref+" non-fast-forward\n")
				continue
			}
			if !IsAncestor(repo, cur, cmd.newOID) {
				report = appendPktLineStr(report, "ng "+cmd.ref+" non-fast-forward\n")
				continue
			}
			repo.Refs[cmd.ref] = cmd.newOID
			changedCount++
			lastChangedRef = cmd.ref
			lastChangedOID = cmd.newOID
			report = appendPktLineStr(report, "ok "+cmd.ref+"\n")
		}
		if changedCount == 1 {
			repo.PatchSingleRefCache(lastChangedRef, lastChangedOID)
		} else if changedCount > 1 {
			repo.BumpRefGen()
		}
	}
	report = append(report, pktFlush...)
	s.store.reportBuf = report

	var body []byte
	if useSideband {
		body = appendSideband(s.store.sidebandBuf[:0], 1, report)
		body = append(body, pktFlush...)
		s.store.sidebandBuf = body
	} else {
		body = report
	}
	return s.buildGitResp(200, "application/x-git-receive-pack-result", body, req.KeepAlive)
}
