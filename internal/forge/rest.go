package forge

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

func appendJSONString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' {
			continue
		}
		if start < i {
			dst = append(dst, s[start:i]...)
		}
		switch c {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		default:
			dst = append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0x0f])
		}
		start = i + 1
	}
	if start < len(s) {
		dst = append(dst, s[start:]...)
	}
	return append(dst, '"')
}

func appendIssueJSON(dst []byte, is *Issue, viewerLogin string, viewerID int) []byte {
	dst = append(dst, `{"id":`...)
	dst = strconv.AppendInt(dst, int64(is.ID), 10)
	dst = append(dst, `,"node_id":`...)
	dst = appendJSONString(dst, is.NodeID)
	dst = append(dst, `,"number":`...)
	dst = strconv.AppendInt(dst, int64(is.Number), 10)
	dst = append(dst, `,"title":`...)
	dst = appendJSONString(dst, is.Title)
	dst = append(dst, `,"body":`...)
	dst = appendJSONString(dst, is.Body)
	dst = append(dst, `,"state":`...)
	dst = appendJSONString(dst, is.State)
	dst = append(dst, `,"labels":[`...)
	for i, lbl := range is.Labels {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, `{"id":`...)
		dst = strconv.AppendInt(dst, int64(lbl.ID), 10)
		dst = append(dst, `,"name":`...)
		dst = appendJSONString(dst, lbl.Name)
		dst = append(dst, `,"color":`...)
		dst = appendJSONString(dst, lbl.Color)
		dst = append(dst, '}')
	}
	dst = append(dst, `]}`...)
	return dst
}

func appendRepoJSON(dst []byte, r *Repo, viewerID int) []byte {
	dst = append(dst, `{"id":`...)
	dst = strconv.AppendInt(dst, int64(r.ID), 10)
	dst = append(dst, `,"node_id":`...)
	dst = appendJSONString(dst, r.NodeID)
	dst = append(dst, `,"name":`...)
	dst = appendJSONString(dst, r.Name)
	dst = append(dst, `,"full_name":`...)
	dst = appendJSONString(dst, r.Owner+"/"+r.Name)
	dst = append(dst, `,"private":`...)
	if r.Private {
		dst = append(dst, "true"...)
	} else {
		dst = append(dst, "false"...)
	}
	dst = append(dst, `,"description":`...)
	dst = appendJSONString(dst, r.Description)
	dst = append(dst, `,"default_branch":`...)
	dst = appendJSONString(dst, r.DefaultBranch)
	dst = append(dst, `,"has_issues":true,"owner":{"id":`...)
	dst = strconv.AppendInt(dst, int64(viewerID), 10)
	dst = append(dst, `,"login":`...)
	dst = appendJSONString(dst, r.Owner)
	dst = append(dst, `,"type":"User"},"created_at":`...)
	dst = appendJSONString(dst, r.CreatedAt)
	dst = append(dst, '}')
	return dst
}

func appendPullJSON(dst []byte, pr *PullRequest, viewerID int) []byte {
	dst = append(dst, `{"id":`...)
	dst = strconv.AppendInt(dst, int64(pr.ID), 10)
	dst = append(dst, `,"node_id":`...)
	dst = appendJSONString(dst, pr.NodeID)
	dst = append(dst, `,"number":`...)
	dst = strconv.AppendInt(dst, int64(pr.Number), 10)
	dst = append(dst, `,"state":`...)
	dst = appendJSONString(dst, pr.State)
	dst = append(dst, `,"title":`...)
	dst = appendJSONString(dst, pr.Title)
	dst = append(dst, `,"body":`...)
	dst = appendJSONString(dst, pr.Body)
	dst = append(dst, `,"merged":`...)
	if pr.Merged {
		dst = append(dst, "true"...)
	} else {
		dst = append(dst, "false"...)
	}
	dst = append(dst, `,"merged_at":`...)
	if pr.MergedAt != "" {
		dst = appendJSONString(dst, pr.MergedAt)
	} else {
		dst = append(dst, "null"...)
	}
	dst = append(dst, `,"merge_commit_sha":`...)
	if pr.MergeCommitSHA != "" {
		dst = appendJSONString(dst, pr.MergeCommitSHA)
	} else {
		dst = append(dst, "null"...)
	}
	dst = append(dst, `,"head":{"ref":`...)
	dst = appendJSONString(dst, pr.HeadRef)
	dst = append(dst, `,"sha":"`...)
	dst = pr.HeadSHA.AppendHex(dst)
	dst = append(dst, `","label":`...)
	dst = appendJSONString(dst, pr.Repo.Owner+":"+pr.HeadRef)
	dst = append(dst, `},"base":{"ref":`...)
	dst = appendJSONString(dst, pr.BaseRef)
	dst = append(dst, `,"sha":"`...)
	dst = pr.BaseSHA.AppendHex(dst)
	dst = append(dst, `","label":`...)
	dst = appendJSONString(dst, pr.Repo.Owner+":"+pr.BaseRef)
	dst = append(dst, `},"user":{"id":`...)
	dst = strconv.AppendInt(dst, int64(viewerID), 10)
	dst = append(dst, `,"login":`...)
	dst = appendJSONString(dst, pr.Repo.Owner)
	dst = append(dst, `,"type":"User"},"created_at":`...)
	dst = appendJSONString(dst, pr.CreatedAt)
	dst = append(dst, `,"updated_at":`...)
	dst = appendJSONString(dst, pr.UpdatedAt)
	dst = append(dst, '}')
	return dst
}

// tryFastIssueCreate parses simple {"title":"...","body":"..."} payloads without reflection.
// Returns (title, body, ok). Falls back to encoding/json if escapes or extra keys (like labels) exist.
func tryFastIssueCreate(b []byte) (string, string, bool) {
	b = bytes.TrimSpace(b)
	if len(b) < 2 || b[0] != '{' || b[len(b)-1] != '}' {
		return "", "", false
	}
	pos := 1
	end := len(b) - 1
	var title, body string
	hasTitle := false
	for pos < end {
		for pos < end && (b[pos] == ' ' || b[pos] == '\t' || b[pos] == '\n' || b[pos] == '\r' || b[pos] == ',') {
			pos++
		}
		if pos >= end {
			break
		}
		if b[pos] != '"' {
			return "", "", false
		}
		pos++
		kStart := pos
		for pos < end && b[pos] != '"' {
			if b[pos] == '\\' {
				return "", "", false
			}
			pos++
		}
		if pos >= end {
			return "", "", false
		}
		key := b[kStart:pos]
		pos++
		for pos < end && (b[pos] == ' ' || b[pos] == '\t') {
			pos++
		}
		if pos >= end || b[pos] != ':' {
			return "", "", false
		}
		pos++
		for pos < end && (b[pos] == ' ' || b[pos] == '\t') {
			pos++
		}
		if pos >= end || b[pos] != '"' {
			return "", "", false
		}
		pos++
		vStart := pos
		for pos < end && b[pos] != '"' {
			if b[pos] == '\\' {
				return "", "", false
			}
			pos++
		}
		if pos >= end {
			return "", "", false
		}
		val := b[vStart:pos]
		pos++
		if string(key) == "title" {
			title = string(val)
			hasTitle = true
		} else if string(key) == "body" {
			body = string(val)
		} else {
			return "", "", false
		}
	}
	return title, body, hasTitle
}

func (s *EpollServer) handleREST(req *Request) []byte {
	if !s.store.CheckAuth(req.Auth) {
		return s.buildJSONResp(401, "{\"message\":\"Requires authentication\"}\n", nil, req.KeepAlive)
	}

	// Strip "/api/v3" prefix
	sub := req.Path[7:]
	method := req.Method

	// Ultra-fast dispatch for /repos/{owner}/{repo}/issues[/{n}] (both read & write benchmarks!)
	if len(sub) > 7 && string(sub[:7]) == "/repos/" {
		rest := sub[7:]
		s1 := bytes.IndexByte(rest, '/')
		if s1 > 0 {
			owner := rest[:s1]
			afterOwner := rest[s1+1:]
			s2 := bytes.IndexByte(afterOwner, '/')
			if s2 > 0 {
				repoName := afterOwner[:s2]
				tail := afterOwner[s2:]
				// Hot path A: GET /repos/{o}/{r}/issues/{n}
				if len(method) == 3 && method[0] == 'G' && len(tail) > 8 && string(tail[:8]) == "/issues/" {
					numPart := tail[8:]
					if bytes.IndexByte(numPart, '/') < 0 {
						num, ok := parseUintASCII(numPart)
						if ok && num > 0 {
							repo := s.store.GetRepoBytes(owner, repoName)
							if repo == nil || num > len(repo.Issues) {
								return s.buildJSONResp(404, "{\"message\":\"Not Found\"}\n", nil, req.KeepAlive)
							}
							is := repo.Issues[num-1]
							if req.KeepAlive {
								cached := is.cachedGetResp.Load()
								if cached == nil {
									jb := appendIssueJSON(make([]byte, 0, 512), is, s.store.ViewerLogin, s.store.ViewerID)
									jb = append(jb, '\n')
									resp := buildImmutableRESTResp(200, "application/json", jb)
									is.cachedGetResp.Store(&resp)
									return resp
								}
								return *cached
							}
							jb := appendIssueJSON(req.Tmp[:0], is, s.store.ViewerLogin, s.store.ViewerID)
							jb = append(jb, '\n')
							req.Tmp = jb
							req.Out = appendHTTPResp(req.Out[:0], 200, "application/json", jb, nil, false, true)
							return req.Out
						}
					}
				}
				// Hot path B: POST /repos/{o}/{r}/issues
				if len(method) == 4 && method[0] == 'P' && method[1] == 'O' && len(tail) == 7 && string(tail) == "/issues" {
					repo := s.store.GetRepoBytes(owner, repoName)
					if repo == nil {
						return s.buildJSONResp(404, "{\"message\":\"Not Found\"}\n", nil, req.KeepAlive)
					}
					if title, body, ok := tryFastIssueCreate(req.Body); ok {
						is, _ := s.store.CreateIssue(repo, title, body, nil)
						jb := appendIssueJSON(req.Tmp[:0], is, s.store.ViewerLogin, s.store.ViewerID)
						jb = append(jb, '\n')
						req.Tmp = jb
						req.Out = appendHTTPResp(req.Out[:0], 201, "application/json", jb, nil, req.KeepAlive, true)
						return req.Out
					}
					return s.handleCreateIssueSlow(req, repo)
				}
			}
		}
	}

	m := string(method)
	// Root & meta & user endpoints
	if len(sub) == 0 || (len(sub) == 1 && sub[0] == '/') {
		if m == "GET" {
			return s.buildJSONResp(200, `{"current_user_url":"/api/v3/user","repository_url":"/api/v3/repos/{owner}/{repo}"}`+"\n", [][2]string{{"X-OAuth-Scopes", "repo, read:org"}}, req.KeepAlive)
		}
		return s.buildJSONResp(405, "{\"message\":\"Method Not Allowed\"}\n", nil, req.KeepAlive)
	}
	switch string(sub) {
	case "/meta":
		if m == "GET" {
			return s.buildJSONResp(200, `{"installed_version":"3.14.0","verifiable_password_authentication":false}`+"\n", nil, req.KeepAlive)
		}
		return s.buildJSONResp(405, "{\"message\":\"Method Not Allowed\"}\n", nil, req.KeepAlive)
	case "/user":
		if m == "GET" {
			var b []byte
			b = append(b, `{"id":`...)
			b = strconv.AppendInt(b, int64(s.store.ViewerID), 10)
			b = append(b, `,"login":`...)
			b = appendJSONString(b, s.store.ViewerLogin)
			b = append(b, `,"name":`...)
			b = appendJSONString(b, s.store.ViewerName)
			b = append(b, `,"email":`...)
			b = appendJSONString(b, s.store.ViewerEmail)
			b = append(b, `,"type":"User"}`+"\n"...)
			return s.buildJSONBytesResp(200, b, nil, req.KeepAlive)
		}
		return s.buildJSONResp(405, "{\"message\":\"Method Not Allowed\"}\n", nil, req.KeepAlive)
	case "/user/repos":
		if m == "GET" {
			return s.handleListUserRepos(req)
		}
		if m == "POST" {
			return s.handleCreateRepo(req)
		}
		return s.buildJSONResp(405, "{\"message\":\"Method Not Allowed\"}\n", nil, req.KeepAlive)
	}

	// /repos/{owner}/{repo}...
	if strings.HasPrefix(string(sub), "/repos/") {
		return s.handleRepoRoutes(req, string(sub[7:]))
	}

	return s.buildJSONResp(404, "{\"message\":\"Not Found\"}\n", nil, req.KeepAlive)
}

func (s *EpollServer) handleListUserRepos(req *Request) []byte {
	perPage := 30
	if v := req.QueryParam("per_page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			perPage = n
			if perPage > 100 {
				perPage = 100
			}
		}
	}
	page := 1
	if v := req.QueryParam("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	total := len(s.store.RepoList)
	start := (page - 1) * perPage
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	slice := s.store.RepoList[start:end]

	var b []byte
	b = append(b, '[')
	for i, r := range slice {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendRepoJSON(b, r, s.store.ViewerID)
	}
	b = append(b, ']', '\n')

	var hdrs [][2]string
	if end < total {
		host := string(req.Host)
		if host == "" {
			host = "127.0.0.1:" + strconv.Itoa(s.port)
		}
		nextURL := "http://" + host + "/api/v3/user/repos?per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(page+1)
		hdrs = [][2]string{{"Link", `<` + nextURL + `>; rel="next"`}}
	}
	return s.buildJSONBytesResp(200, b, hdrs, req.KeepAlive)
}

func (s *EpollServer) handleCreateRepo(req *Request) []byte {
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Private     bool   `json:"private"`
	}
	if err := json.Unmarshal(req.Body, &in); err != nil {
		return s.buildJSONResp(400, `{"message":"Problems parsing JSON"}`+"\n", nil, req.KeepAlive)
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return s.buildJSONResp(422, `{"message":"Repository creation failed: name is required"}`+"\n", nil, req.KeepAlive)
	}
	r, ok := s.store.CreateRepo(in.Name, in.Description, in.Private)
	if !ok {
		return s.buildJSONResp(422, `{"message":"Repository creation failed: name already exists"}`+"\n", nil, req.KeepAlive)
	}
	jb := appendRepoJSON(nil, r, s.store.ViewerID)
	jb = append(jb, '\n')
	return s.buildJSONBytesResp(201, jb, nil, req.KeepAlive)
}

func unescapeSegment(seg string) string {
	u, err := url.PathUnescape(seg)
	if err != nil {
		return seg
	}
	return u
}

func (s *EpollServer) handleRepoRoutes(req *Request, rest string) []byte {
	// rest = "{owner}/{repo}" or "{owner}/{repo}/{tail}"
	s1 := strings.IndexByte(rest, '/')
	if s1 <= 0 {
		return s.buildJSONResp(404, "{\"message\":\"Not Found\"}\n", nil, req.KeepAlive)
	}
	owner := unescapeSegment(rest[:s1])
	afterOwner := rest[s1+1:]
	var repoName, tail string
	if s2 := strings.IndexByte(afterOwner, '/'); s2 < 0 {
		repoName = unescapeSegment(afterOwner)
		tail = ""
	} else {
		repoName = unescapeSegment(afterOwner[:s2])
		tail = afterOwner[s2+1:]
	}

	repo := s.store.GetRepo(owner, repoName)
	if repo == nil {
		return s.buildJSONResp(404, "{\"message\":\"Not Found\"}\n", nil, req.KeepAlive)
	}

	m := string(req.Method)
	if tail == "" {
		switch m {
		case "GET":
			jb := appendRepoJSON(nil, repo, s.store.ViewerID)
			jb = append(jb, '\n')
			return s.buildJSONBytesResp(200, jb, nil, req.KeepAlive)
		case "PATCH":
			var in struct {
				Description   *string `json:"description"`
				Private       *bool   `json:"private"`
				DefaultBranch *string `json:"default_branch"`
			}
			if err := json.Unmarshal(req.Body, &in); err != nil {
				return s.buildJSONResp(400, `{"message":"Problems parsing JSON"}`+"\n", nil, req.KeepAlive)
			}
			if in.Description != nil {
				repo.Description = *in.Description
			}
			if in.Private != nil {
				repo.Private = *in.Private
			}
			if in.DefaultBranch != nil && *in.DefaultBranch != "" {
				repo.DefaultBranch = *in.DefaultBranch
				repo.HeadRef = "refs/heads/" + *in.DefaultBranch
				repo.BumpRefGen()
			}
			jb := appendRepoJSON(nil, repo, s.store.ViewerID)
			jb = append(jb, '\n')
			return s.buildJSONBytesResp(200, jb, nil, req.KeepAlive)
		case "DELETE":
			s.store.DeleteRepo(owner, repoName)
			return s.buildJSONResp(204, "", nil, req.KeepAlive)
		default:
			return s.buildJSONResp(405, "{\"message\":\"Method Not Allowed\"}\n", nil, req.KeepAlive)
		}
	}

	// Contents: contents/{path...}
	if tail == "contents" || strings.HasPrefix(tail, "contents/") {
		relPath := ""
		if len(tail) > 9 {
			segs := strings.Split(tail[9:], "/")
			for i := range segs {
				segs[i] = unescapeSegment(segs[i])
			}
			relPath = strings.Join(segs, "/")
		}
		return s.handleContents(req, repo, relPath)
	}

	// Branches: branches or branches/{name}
	if tail == "branches" && m == "GET" {
		return s.handleListBranches(req, repo)
	}
	if strings.HasPrefix(tail, "branches/") && m == "GET" {
		bname := unescapeSegment(tail[9:])
		oid, ok := repo.Refs["refs/heads/"+bname]
		if !ok {
			return s.buildJSONResp(404, "{\"message\":\"Branch not found\"}\n", nil, req.KeepAlive)
		}
		var b []byte
		b = append(b, `{"name":`...)
		b = appendJSONString(b, bname)
		b = append(b, `,"commit":{"sha":"`...)
		b = oid.AppendHex(b)
		b = append(b, `"}}`+"\n"...)
		return s.buildJSONBytesResp(200, b, nil, req.KeepAlive)
	}

	// Git refs: git/refs or git/refs/{ref...}
	if tail == "git/refs" && m == "POST" {
		return s.handleCreateGitRef(req, repo)
	}
	if strings.HasPrefix(tail, "git/refs/") && m == "GET" {
		refSuffix := unescapeSegment(tail[9:])
		fullRef := "refs/" + refSuffix
		oid, ok := repo.Refs[fullRef]
		if !ok {
			return s.buildJSONResp(404, "{\"message\":\"Not Found\"}\n", nil, req.KeepAlive)
		}
		var b []byte
		b = append(b, `{"ref":`...)
		b = appendJSONString(b, fullRef)
		b = append(b, `,"object":{"sha":"`...)
		b = oid.AppendHex(b)
		b = append(b, `","type":"commit"}}`+"\n"...)
		return s.buildJSONBytesResp(200, b, nil, req.KeepAlive)
	}

	// Commits: commits
	if tail == "commits" && m == "GET" {
		return s.handleListCommits(req, repo)
	}

	// Labels: labels
	if tail == "labels" {
		if m == "POST" {
			return s.handleCreateLabel(req, repo)
		}
		if m == "GET" {
			names := make([]string, 0, len(repo.Labels))
			for n := range repo.Labels {
				names = append(names, n)
			}
			sort.Strings(names)
			var b []byte
			b = append(b, '[')
			for i, n := range names {
				lbl := repo.Labels[n]
				if i > 0 {
					b = append(b, ',')
				}
				b = append(b, `{"id":`...)
				b = strconv.AppendInt(b, int64(lbl.ID), 10)
				b = append(b, `,"name":`...)
				b = appendJSONString(b, lbl.Name)
				b = append(b, `,"color":`...)
				b = appendJSONString(b, lbl.Color)
				b = append(b, '}')
			}
			b = append(b, ']', '\n')
			return s.buildJSONBytesResp(200, b, nil, req.KeepAlive)
		}
	}

	// Issues: issues, issues/{n}, issues/{n}/comments
	if tail == "issues" {
		if m == "GET" {
			return s.handleListIssues(req, repo)
		}
		if m == "POST" {
			return s.handleCreateIssueSlow(req, repo)
		}
	}
	if strings.HasPrefix(tail, "issues/") {
		return s.handleIssueItemRoutes(req, repo, tail[7:])
	}

	// Pulls: pulls, pulls/{n}, pulls/{n}/files, pulls/{n}/merge
	if tail == "pulls" {
		if m == "GET" {
			return s.handleListPulls(req, repo)
		}
		if m == "POST" {
			return s.handleCreatePull(req, repo)
		}
	}
	if strings.HasPrefix(tail, "pulls/") {
		return s.handlePullItemRoutes(req, repo, tail[6:])
	}

	return s.buildJSONResp(404, "{\"message\":\"Not Found\"}\n", nil, req.KeepAlive)
}

func (s *EpollServer) resolveCommitOID(repo *Repo, ref string) (OID, bool) {
	if ref == "" {
		oid, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]
		return oid, ok
	}
	if oid, ok := repo.Refs["refs/heads/"+ref]; ok {
		return oid, true
	}
	if oid, ok := repo.Refs["refs/tags/"+ref]; ok {
		target, _ := peelTag(repo, oid)
		return target, true
	}
	if oid, ok := repo.Refs[ref]; ok {
		return oid, true
	}
	if oid, ok := ParseOID(ref); ok {
		if _, exists := repo.Objects[oid]; exists {
			return oid, true
		}
	}
	return ZeroOID, false
}

func (s *EpollServer) handleContents(req *Request, repo *Repo, relPath string) []byte {
	relPath = strings.Trim(relPath, "/")
	m := string(req.Method)
	switch m {
	case "GET":
		ref := req.QueryParam("ref")
		commitOID, ok := s.resolveCommitOID(repo, ref)
		if !ok {
			return s.buildJSONResp(404, "{\"message\":\"Not Found\"}\n", nil, req.KeepAlive)
		}
		cobj, ok := repo.Objects[commitOID]
		if !ok || cobj.Type != ObjCommit {
			return s.buildJSONResp(404, "{\"message\":\"Not Found\"}\n", nil, req.KeepAlive)
		}
		parseCommitInto(cobj)
		entry, isDir, found := ResolvePathInTree(repo, cobj.TreeOID, relPath)
		if !found {
			return s.buildJSONResp(404, "{\"message\":\"Not Found\"}\n", nil, req.KeepAlive)
		}
		if isDir {
			tobj, ok := repo.Objects[entry.OID]
			if !ok || tobj.Type != ObjTree {
				return s.buildJSONResp(404, "{\"message\":\"Not Found\"}\n", nil, req.KeepAlive)
			}
			var b []byte
			b = append(b, '[')
			for i, e := range ParseTree(tobj.Data) {
				if i > 0 {
					b = append(b, ',')
				}
				etype := "file"
				sz := 0
				if e.Mode == "40000" || e.Mode == "040000" {
					etype = "dir"
				} else if bobj, ok := repo.Objects[e.OID]; ok {
					sz = len(bobj.Data)
				}
				epath := e.Name
				if relPath != "" {
					epath = relPath + "/" + e.Name
				}
				b = append(b, `{"type":`...)
				b = appendJSONString(b, etype)
				b = append(b, `,"name":`...)
				b = appendJSONString(b, e.Name)
				b = append(b, `,"path":`...)
				b = appendJSONString(b, epath)
				b = append(b, `,"sha":"`...)
				b = e.OID.AppendHex(b)
				b = append(b, `","size":`...)
				b = strconv.AppendInt(b, int64(sz), 10)
				b = append(b, '}')
			}
			b = append(b, ']', '\n')
			return s.buildJSONBytesResp(200, b, nil, req.KeepAlive)
		}
		bobj, ok := repo.Objects[entry.OID]
		if !ok || bobj.Type != ObjBlob {
			return s.buildJSONResp(404, "{\"message\":\"Not Found\"}\n", nil, req.KeepAlive)
		}
		encoded := base64.StdEncoding.EncodeToString(bobj.Data)
		var b []byte
		b = append(b, `{"type":"file","encoding":"base64","name":`...)
		b = appendJSONString(b, entry.Name)
		b = append(b, `,"path":`...)
		b = appendJSONString(b, relPath)
		b = append(b, `,"sha":"`...)
		b = entry.OID.AppendHex(b)
		b = append(b, `","size":`...)
		b = strconv.AppendInt(b, int64(len(bobj.Data)), 10)
		b = append(b, `,"content":`...)
		b = appendJSONString(b, encoded)
		b = append(b, '}', '\n')
		return s.buildJSONBytesResp(200, b, nil, req.KeepAlive)

	case "PUT":
		var in struct {
			Message string `json:"message"`
			Content string `json:"content"`
			SHA     string `json:"sha"`
			Branch  string `json:"branch"`
		}
		if err := json.Unmarshal(req.Body, &in); err != nil {
			return s.buildJSONResp(400, `{"message":"Problems parsing JSON"}`+"\n", nil, req.KeepAlive)
		}
		cleaned := strings.NewReplacer("\n", "", "\r", "", " ", "", "\t", "").Replace(in.Content)
		rawBytes, err := base64.StdEncoding.DecodeString(cleaned)
		if err != nil {
			return s.buildJSONResp(422, `{"message":"content is not valid base64"}`+"\n", nil, req.KeepAlive)
		}
		branch := in.Branch
		if branch == "" {
			branch = repo.DefaultBranch
		}
		refName := "refs/heads/" + branch
		parentOID, hasBranch := repo.Refs[refName]
		files := make(map[string]OID)
		var parents []OID
		if hasBranch {
			parents = []OID{parentOID}
			if cobj, ok := repo.Objects[parentOID]; ok && cobj.Type == ObjCommit {
				parseCommitInto(cobj)
				FlattenTree(repo, cobj.TreeOID, "", files)
			}
		} else if branch != repo.DefaultBranch {
			// Creating a new branch implicitly from default branch if default branch exists
			if defOID, hasDef := repo.Refs["refs/heads/"+repo.DefaultBranch]; hasDef {
				parents = []OID{defOID}
				if cobj, ok := repo.Objects[defOID]; ok && cobj.Type == ObjCommit {
					parseCommitInto(cobj)
					FlattenTree(repo, cobj.TreeOID, "", files)
				}
			}
		}

		existingOID, existed := files[relPath]
		if existed {
			if strings.ToLower(in.SHA) != existingOID.Hex() {
				return s.buildJSONResp(409, `{"message":"`+relPath+` does not match expected sha"}`+"\n", nil, req.KeepAlive)
			}
		}

		blobObj := s.store.PutObject(repo, ObjBlob, rawBytes, nil)
		files[relPath] = blobObj.OID
		newTree := s.store.BuildTreeFromFileMap(repo, files)
		msg := in.Message
		if msg == "" {
			msg = "Update " + relPath
		}
		commitObj := s.store.BuildCommitObj(repo, newTree, parents, msg)
		repo.Refs[refName] = commitObj.OID
		repo.BumpRefGen()

		status := 201
		if existed {
			status = 200
		}
		baseName := relPath
		if idx := strings.LastIndexByte(relPath, '/'); idx >= 0 {
			baseName = relPath[idx+1:]
		}
		var b []byte
		b = append(b, `{"content":{"name":`...)
		b = appendJSONString(b, baseName)
		b = append(b, `,"path":`...)
		b = appendJSONString(b, relPath)
		b = append(b, `,"sha":"`...)
		b = blobObj.OID.AppendHex(b)
		b = append(b, `","size":`...)
		b = strconv.AppendInt(b, int64(len(rawBytes)), 10)
		b = append(b, `,"type":"file","encoding":"base64"},"commit":{"sha":"`...)
		b = commitObj.OID.AppendHex(b)
		b = append(b, `","message":`...)
		b = appendJSONString(b, msg)
		b = append(b, `}}`+"\n"...)
		return s.buildJSONBytesResp(status, b, nil, req.KeepAlive)

	case "DELETE":
		var in struct {
			Message string `json:"message"`
			SHA     string `json:"sha"`
			Branch  string `json:"branch"`
		}
		if err := json.Unmarshal(req.Body, &in); err != nil {
			return s.buildJSONResp(400, `{"message":"Problems parsing JSON"}`+"\n", nil, req.KeepAlive)
		}
		branch := in.Branch
		if branch == "" {
			branch = repo.DefaultBranch
		}
		refName := "refs/heads/" + branch
		parentOID, hasBranch := repo.Refs[refName]
		if !hasBranch {
			return s.buildJSONResp(404, "{\"message\":\"Not Found\"}\n", nil, req.KeepAlive)
		}
		files := make(map[string]OID)
		if cobj, ok := repo.Objects[parentOID]; ok && cobj.Type == ObjCommit {
			parseCommitInto(cobj)
			FlattenTree(repo, cobj.TreeOID, "", files)
		}
		existingOID, existed := files[relPath]
		if !existed {
			return s.buildJSONResp(404, "{\"message\":\"Not Found\"}\n", nil, req.KeepAlive)
		}
		if in.SHA != "" && strings.ToLower(in.SHA) != existingOID.Hex() {
			return s.buildJSONResp(409, `{"message":"sha mismatch"}`+"\n", nil, req.KeepAlive)
		}
		delete(files, relPath)
		newTree := s.store.BuildTreeFromFileMap(repo, files)
		msg := in.Message
		if msg == "" {
			msg = "Delete " + relPath
		}
		commitObj := s.store.BuildCommitObj(repo, newTree, []OID{parentOID}, msg)
		repo.Refs[refName] = commitObj.OID
		repo.BumpRefGen()

		var b []byte
		b = append(b, `{"content":null,"commit":{"sha":"`...)
		b = commitObj.OID.AppendHex(b)
		b = append(b, `","message":`...)
		b = appendJSONString(b, msg)
		b = append(b, `}}`+"\n"...)
		return s.buildJSONBytesResp(200, b, nil, req.KeepAlive)
	}
	return s.buildJSONResp(405, "{\"message\":\"Method Not Allowed\"}\n", nil, req.KeepAlive)
}

func (s *EpollServer) handleListBranches(req *Request, repo *Repo) []byte {
	var names []string
	for ref := range repo.Refs {
		if strings.HasPrefix(ref, "refs/heads/") {
			names = append(names, ref[11:])
		}
	}
	sort.Strings(names)
	var b []byte
	b = append(b, '[')
	for i, name := range names {
		if i > 0 {
			b = append(b, ',')
		}
		oid := repo.Refs["refs/heads/"+name]
		b = append(b, `{"name":`...)
		b = appendJSONString(b, name)
		b = append(b, `,"commit":{"sha":"`...)
		b = oid.AppendHex(b)
		b = append(b, `"}}`...)
	}
	b = append(b, ']', '\n')
	return s.buildJSONBytesResp(200, b, nil, req.KeepAlive)
}

func (s *EpollServer) handleCreateGitRef(req *Request, repo *Repo) []byte {
	var in struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(req.Body, &in); err != nil {
		return s.buildJSONResp(400, `{"message":"Problems parsing JSON"}`+"\n", nil, req.KeepAlive)
	}
	oid, ok := ParseOID(in.SHA)
	if !ok {
		return s.buildJSONResp(422, `{"message":"Invalid sha"}`+"\n", nil, req.KeepAlive)
	}
	if _, exists := repo.Objects[oid]; !exists {
		return s.buildJSONResp(422, `{"message":"Object does not exist"}`+"\n", nil, req.KeepAlive)
	}
	ref := in.Ref
	if !strings.HasPrefix(ref, "refs/") {
		ref = "refs/" + strings.TrimPrefix(ref, "/")
	}
	repo.Refs[ref] = oid
	repo.BumpRefGen()
	var b []byte
	b = append(b, `{"ref":`...)
	b = appendJSONString(b, ref)
	b = append(b, `,"object":{"sha":"`...)
	b = oid.AppendHex(b)
	b = append(b, `","type":"commit"}}`+"\n"...)
	return s.buildJSONBytesResp(201, b, nil, req.KeepAlive)
}

func (s *EpollServer) handleListCommits(req *Request, repo *Repo) []byte {
	shaParam := req.QueryParam("sha")
	tip, ok := s.resolveCommitOID(repo, shaParam)
	if !ok {
		return s.buildJSONResp(200, "[]\n", nil, req.KeepAlive)
	}
	var commits []*GitObj
	visited := make(map[OID]struct{}, 16)
	q := []OID{tip}
	for len(q) > 0 && len(commits) < 100 {
		cur := q[0]
		q = q[1:]
		if _, seen := visited[cur]; seen {
			continue
		}
		visited[cur] = struct{}{}
		obj, exists := repo.Objects[cur]
		if !exists || obj.Type != ObjCommit {
			continue
		}
		parseCommitInto(obj)
		commits = append(commits, obj)
		q = obj.AppendParents(q)
	}
	var b []byte
	b = append(b, '[')
	for i, c := range commits {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, `{"sha":"`...)
		b = c.OID.AppendHex(b)
		b = append(b, `","commit":{"message":`...)
		b = appendJSONString(b, commitMessageOf(c))
		b = append(b, `,"author":{"name":`...)
		b = appendJSONString(b, s.store.ViewerName)
		b = append(b, `,"email":`...)
		b = appendJSONString(b, s.store.ViewerEmail)
		b = append(b, `,"date":`...)
		b = appendJSONString(b, s.store.TrialNow)
		b = append(b, `},"committer":{"name":`...)
		b = appendJSONString(b, s.store.ViewerName)
		b = append(b, `,"email":`...)
		b = appendJSONString(b, s.store.ViewerEmail)
		b = append(b, `,"date":`...)
		b = appendJSONString(b, s.store.TrialNow)
		b = append(b, `}},"parents":[`...)
		ps := c.AppendParents(nil)
		for j, p := range ps {
			if j > 0 {
				b = append(b, ',')
			}
			b = append(b, `{"sha":"`...)
			b = p.AppendHex(b)
			b = append(b, `"}`...)
		}
		b = append(b, `]}`...)
	}
	b = append(b, ']', '\n')
	return s.buildJSONBytesResp(200, b, nil, req.KeepAlive)
}

func (s *EpollServer) handleCreateLabel(req *Request, repo *Repo) []byte {
	var in struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	}
	if err := json.Unmarshal(req.Body, &in); err != nil {
		return s.buildJSONResp(400, `{"message":"Problems parsing JSON"}`+"\n", nil, req.KeepAlive)
	}
	if in.Name == "" {
		return s.buildJSONResp(422, `{"message":"Validation Failed: name required"}`+"\n", nil, req.KeepAlive)
	}
	lbl := s.store.CreateLabel(repo, in.Name, in.Color)
	var b []byte
	b = append(b, `{"id":`...)
	b = strconv.AppendInt(b, int64(lbl.ID), 10)
	b = append(b, `,"name":`...)
	b = appendJSONString(b, lbl.Name)
	b = append(b, `,"color":`...)
	b = appendJSONString(b, lbl.Color)
	b = append(b, '}', '\n')
	return s.buildJSONBytesResp(201, b, nil, req.KeepAlive)
}

func (s *EpollServer) handleCreateIssueSlow(req *Request, repo *Repo) []byte {
	var in struct {
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		Labels []string `json:"labels"`
	}
	if err := json.Unmarshal(req.Body, &in); err != nil {
		return s.buildJSONResp(400, `{"message":"Problems parsing JSON"}`+"\n", nil, req.KeepAlive)
	}
	if in.Title == "" {
		return s.buildJSONResp(422, `{"message":"Validation Failed: title is required"}`+"\n", nil, req.KeepAlive)
	}
	is, errMsg := s.store.CreateIssue(repo, in.Title, in.Body, in.Labels)
	if errMsg != "" {
		return s.buildJSONResp(422, `{"message":"Validation Failed: `+errMsg+`"}`+"\n", nil, req.KeepAlive)
	}
	jb := appendIssueJSON(nil, is, s.store.ViewerLogin, s.store.ViewerID)
	jb = append(jb, '\n')
	return s.buildJSONBytesResp(201, jb, nil, req.KeepAlive)
}

func (s *EpollServer) handleListIssues(req *Request, repo *Repo) []byte {
	state := req.QueryParam("state")
	if state == "" {
		state = "open"
	}
	dir := req.QueryParam("direction")
	if dir == "" {
		dir = "desc"
	}
	perPage := 30
	if v := req.QueryParam("per_page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			perPage = n
			if perPage > 100 {
				perPage = 100
			}
		}
	}
	page := 1
	if v := req.QueryParam("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}

	filtered := make([]*Issue, 0, len(repo.Issues))
	if dir == "asc" {
		for _, is := range repo.Issues {
			if state == "all" || is.State == state {
				filtered = append(filtered, is)
			}
		}
	} else {
		for i := len(repo.Issues) - 1; i >= 0; i-- {
			is := repo.Issues[i]
			if state == "all" || is.State == state {
				filtered = append(filtered, is)
			}
		}
	}

	start := (page - 1) * perPage
	if start > len(filtered) {
		start = len(filtered)
	}
	end := start + perPage
	if end > len(filtered) {
		end = len(filtered)
	}
	var b []byte
	b = append(b, '[')
	for i, is := range filtered[start:end] {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendIssueJSON(b, is, s.store.ViewerLogin, s.store.ViewerID)
	}
	b = append(b, ']', '\n')
	return s.buildJSONBytesResp(200, b, nil, req.KeepAlive)
}

func (s *EpollServer) handleIssueItemRoutes(req *Request, repo *Repo, tail string) []byte {
	// tail = "{n}" or "{n}/comments"
	var numStr, sub string
	if idx := strings.IndexByte(tail, '/'); idx < 0 {
		numStr = tail
	} else {
		numStr = tail[:idx]
		sub = tail[idx+1:]
	}
	num, err := strconv.Atoi(numStr)
	if err != nil || num <= 0 || num > len(repo.Issues) {
		return s.buildJSONResp(404, "{\"message\":\"Not Found\"}\n", nil, req.KeepAlive)
	}
	is := repo.Issues[num-1]
	m := string(req.Method)

	if sub == "" {
		switch m {
		case "GET":
			jb := appendIssueJSON(nil, is, s.store.ViewerLogin, s.store.ViewerID)
			jb = append(jb, '\n')
			return s.buildJSONBytesResp(200, jb, nil, req.KeepAlive)
		case "PATCH":
			var in struct {
				Title  *string  `json:"title"`
				Body   *string  `json:"body"`
				State  *string  `json:"state"`
				Labels []string `json:"labels"`
			}
			if err := json.Unmarshal(req.Body, &in); err != nil {
				return s.buildJSONResp(400, `{"message":"Problems parsing JSON"}`+"\n", nil, req.KeepAlive)
			}
			if in.Title != nil {
				is.Title = *in.Title
			}
			if in.Body != nil {
				is.Body = *in.Body
			}
			if in.State != nil {
				st := strings.ToLower(*in.State)
				if st == "open" || st == "closed" {
					is.State = st
				}
			}
			if in.Labels != nil {
				lbls := make([]*Label, 0, len(in.Labels))
				for _, ln := range in.Labels {
					lbl, ok := repo.Labels[ln]
					if !ok {
						return s.buildJSONResp(422, `{"message":"Validation Failed: unknown label"}`+"\n", nil, req.KeepAlive)
					}
					lbls = append(lbls, lbl)
				}
				is.Labels = lbls
			}
			is.InvalidateCache()
			jb := appendIssueJSON(nil, is, s.store.ViewerLogin, s.store.ViewerID)
			jb = append(jb, '\n')
			return s.buildJSONBytesResp(200, jb, nil, req.KeepAlive)
		}
		return s.buildJSONResp(405, "{\"message\":\"Method Not Allowed\"}\n", nil, req.KeepAlive)
	}

	if sub == "comments" {
		if m == "GET" {
			var b []byte
			b = append(b, '[')
			for i, c := range is.Comments {
				if i > 0 {
					b = append(b, ',')
				}
				b = append(b, `{"id":`...)
				b = strconv.AppendInt(b, int64(c.ID), 10)
				b = append(b, `,"node_id":`...)
				b = appendJSONString(b, c.NodeID)
				b = append(b, `,"body":`...)
				b = appendJSONString(b, c.Body)
				b = append(b, `,"user":{"id":`...)
				b = strconv.AppendInt(b, int64(s.store.ViewerID), 10)
				b = append(b, `,"login":`...)
				b = appendJSONString(b, c.Author)
				b = append(b, `,"type":"User"},"created_at":`...)
				b = appendJSONString(b, c.CreatedAt)
				b = append(b, '}')
			}
			b = append(b, ']', '\n')
			return s.buildJSONBytesResp(200, b, nil, req.KeepAlive)
		}
		if m == "POST" {
			var in struct {
				Body string `json:"body"`
			}
			if err := json.Unmarshal(req.Body, &in); err != nil {
				return s.buildJSONResp(400, `{"message":"Problems parsing JSON"}`+"\n", nil, req.KeepAlive)
			}
			c := s.store.AddComment(is, in.Body)
			is.InvalidateCache()
			var b []byte
			b = append(b, `{"id":`...)
			b = strconv.AppendInt(b, int64(c.ID), 10)
			b = append(b, `,"node_id":`...)
			b = appendJSONString(b, c.NodeID)
			b = append(b, `,"body":`...)
			b = appendJSONString(b, c.Body)
			b = append(b, `,"user":{"id":`...)
			b = strconv.AppendInt(b, int64(s.store.ViewerID), 10)
			b = append(b, `,"login":`...)
			b = appendJSONString(b, c.Author)
			b = append(b, `,"type":"User"},"created_at":`...)
			b = appendJSONString(b, c.CreatedAt)
			b = append(b, '}', '\n')
			return s.buildJSONBytesResp(201, b, nil, req.KeepAlive)
		}
	}
	return s.buildJSONResp(404, "{\"message\":\"Not Found\"}\n", nil, req.KeepAlive)
}

func (s *EpollServer) handleCreatePull(req *Request, repo *Repo) []byte {
	var in struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Head  string `json:"head"`
		Base  string `json:"base"`
	}
	if err := json.Unmarshal(req.Body, &in); err != nil {
		return s.buildJSONResp(400, `{"message":"Problems parsing JSON"}`+"\n", nil, req.KeepAlive)
	}
	if in.Title == "" || in.Head == "" {
		return s.buildJSONResp(422, `{"message":"Validation Failed: title and head are required"}`+"\n", nil, req.KeepAlive)
	}
	pr, errMsg := s.store.CreatePull(repo, in.Title, in.Body, in.Head, in.Base)
	if errMsg != "" {
		return s.buildJSONResp(422, `{"message":"Validation Failed: `+errMsg+`"}`+"\n", nil, req.KeepAlive)
	}
	jb := appendPullJSON(nil, pr, s.store.ViewerID)
	jb = append(jb, '\n')
	return s.buildJSONBytesResp(201, jb, nil, req.KeepAlive)
}

func (s *EpollServer) handleListPulls(req *Request, repo *Repo) []byte {
	state := req.QueryParam("state")
	if state == "" {
		state = "open"
	}
	var b []byte
	b = append(b, '[')
	first := true
	for i := len(repo.Pulls) - 1; i >= 0; i-- {
		pr := repo.Pulls[i]
		if state != "all" && pr.State != state {
			continue
		}
		if !first {
			b = append(b, ',')
		}
		first = false
		b = appendPullJSON(b, pr, s.store.ViewerID)
	}
	b = append(b, ']', '\n')
	return s.buildJSONBytesResp(200, b, nil, req.KeepAlive)
}

func (s *Store) MergePull(pr *PullRequest, message string) (string, int, string) {
	if pr.Merged {
		return "", 405, "Pull Request is already merged"
	}
	repo := pr.Repo
	baseRefName := "refs/heads/" + pr.BaseRef
	headRefName := "refs/heads/" + pr.HeadRef
	baseOID, okB := repo.Refs[baseRefName]
	if !okB {
		baseOID = pr.BaseSHA
	}
	headOID, okH := repo.Refs[headRefName]
	if !okH {
		headOID = pr.HeadSHA
	}
	mergedFiles, conflict := ThreeWayMerge(repo, baseOID, headOID)
	if conflict {
		return "", 405, "Pull Request is not mergeable"
	}
	newTree := s.BuildTreeFromFileMap(repo, mergedFiles)
	if message == "" {
		message = pr.Title + " (#" + strconv.Itoa(pr.Number) + ")"
	}
	var parents []OID
	if baseOID != ZeroOID {
		parents = []OID{baseOID}
	}
	commitObj := s.BuildCommitObj(repo, newTree, parents, message)
	repo.Refs[baseRefName] = commitObj.OID
	repo.BumpRefGen()

	sha := commitObj.OID.Hex()
	pr.State = "closed"
	pr.Merged = true
	pr.MergedAt = s.TrialNow
	pr.MergeCommitSHA = sha
	pr.BaseSHA = commitObj.OID
	return sha, 200, "Pull Request successfully merged"
}

func (s *EpollServer) handlePullItemRoutes(req *Request, repo *Repo, tail string) []byte {
	var numStr, sub string
	if idx := strings.IndexByte(tail, '/'); idx < 0 {
		numStr = tail
	} else {
		numStr = tail[:idx]
		sub = tail[idx+1:]
	}
	num, err := strconv.Atoi(numStr)
	if err != nil || num <= 0 || num > len(repo.Pulls) {
		return s.buildJSONResp(404, "{\"message\":\"Not Found\"}\n", nil, req.KeepAlive)
	}
	pr := repo.Pulls[num-1]
	m := string(req.Method)

	switch sub {
	case "":
		if m == "GET" {
			jb := appendPullJSON(nil, pr, s.store.ViewerID)
			jb = append(jb, '\n')
			return s.buildJSONBytesResp(200, jb, nil, req.KeepAlive)
		}
		if m == "PATCH" {
			var in struct {
				Title *string `json:"title"`
				Body  *string `json:"body"`
				State *string `json:"state"`
			}
			if err := json.Unmarshal(req.Body, &in); err != nil {
				return s.buildJSONResp(400, `{"message":"Problems parsing JSON"}`+"\n", nil, req.KeepAlive)
			}
			if in.Title != nil {
				pr.Title = *in.Title
			}
			if in.Body != nil {
				pr.Body = *in.Body
			}
			if in.State != nil {
				st := strings.ToLower(*in.State)
				if st == "open" || st == "closed" {
					pr.State = st
				}
			}
			jb := appendPullJSON(nil, pr, s.store.ViewerID)
			jb = append(jb, '\n')
			return s.buildJSONBytesResp(200, jb, nil, req.KeepAlive)
		}
	case "files":
		if m == "GET" {
			files := DiffCommits(repo, pr.BaseSHA, pr.HeadSHA)
			var b []byte
			b = append(b, '[')
			for i, f := range files {
				if i > 0 {
					b = append(b, ',')
				}
				b = append(b, `{"filename":`...)
				b = appendJSONString(b, f.Filename)
				b = append(b, `,"status":`...)
				b = appendJSONString(b, f.Status)
				b = append(b, `,"additions":`...)
				b = strconv.AppendInt(b, int64(f.Additions), 10)
				b = append(b, `,"deletions":`...)
				b = strconv.AppendInt(b, int64(f.Deletions), 10)
				b = append(b, `,"changes":`...)
				b = strconv.AppendInt(b, int64(f.Changes), 10)
				b = append(b, `,"sha":`...)
				b = appendJSONString(b, f.SHA)
				b = append(b, '}')
			}
			b = append(b, ']', '\n')
			return s.buildJSONBytesResp(200, b, nil, req.KeepAlive)
		}
	case "merge":
		if m == "PUT" {
			var in struct {
				CommitTitle string `json:"commit_title"`
				MergeMethod string `json:"merge_method"`
			}
			if len(bytes.TrimSpace(req.Body)) > 0 {
				if err := json.Unmarshal(req.Body, &in); err != nil {
					return s.buildJSONResp(400, `{"message":"Problems parsing JSON"}`+"\n", nil, req.KeepAlive)
				}
			}
			sha, code, msg := s.store.MergePull(pr, in.CommitTitle)
			if code != 200 {
				return s.buildJSONResp(code, `{"message":`+string(appendJSONString(nil, msg))+`}`+"\n", nil, req.KeepAlive)
			}
			var b []byte
			b = append(b, `{"sha":`...)
			b = appendJSONString(b, sha)
			b = append(b, `,"merged":true,"message":`...)
			b = appendJSONString(b, msg)
			b = append(b, '}', '\n')
			return s.buildJSONBytesResp(200, b, nil, req.KeepAlive)
		}
	}
	return s.buildJSONResp(404, "{\"message\":\"Not Found\"}\n", nil, req.KeepAlive)
}
