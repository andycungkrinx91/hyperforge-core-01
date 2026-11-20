package forge

import (
	"encoding/json"
	"strconv"
	"strings"
)

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// Static, byte-identical tools/list result for determinism across calls.
const staticMCPToolsJSON = `{"tools":[` +
	`{"name":"create_repo","description":"Create a new repository for the authenticated user","inputSchema":{"type":"object","properties":{"name":{"type":"string","description":"Repository name"},"description":{"type":"string","description":"Repository description"},"private":{"type":"boolean","description":"Whether the repository is private"}},"required":["name"]}},` +
	`{"name":"get_repo","description":"Fetch metadata for a repository by owner and name","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner login"},"repo":{"type":"string","description":"Repository name"}},"required":["owner","repo"]}},` +
	`{"name":"create_issue","description":"Create a new issue in a repository","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner login"},"repo":{"type":"string","description":"Repository name"},"title":{"type":"string","description":"Issue title"},"body":{"type":"string","description":"Issue body"}},"required":["owner","repo","title"]}},` +
	`{"name":"list_issues","description":"List issues in a repository","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner login"},"repo":{"type":"string","description":"Repository name"},"state":{"type":"string","description":"Filter by state: open, closed, or all"}},"required":["owner","repo"]}},` +
	`{"name":"create_pull_request","description":"Open a new pull request between head and base branches","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner login"},"repo":{"type":"string","description":"Repository name"},"title":{"type":"string","description":"Pull request title"},"body":{"type":"string","description":"Pull request description"},"head":{"type":"string","description":"Head branch name"},"base":{"type":"string","description":"Base branch name"}},"required":["owner","repo","title","head","base"]}},` +
	`{"name":"merge_pull_request","description":"Merge an open pull request into its base branch","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner login"},"repo":{"type":"string","description":"Repository name"},"pull_number":{"type":"integer","description":"Pull request number"}},"required":["owner","repo","pull_number"]}}` +
	`]}`

func mcpRPCResult(id json.RawMessage, resultJSON []byte) []byte {
	if len(id) == 0 {
		id = []byte("null")
	}
	var b []byte
	b = append(b, `{"jsonrpc":"2.0","id":`...)
	b = append(b, id...)
	b = append(b, `,"result":`...)
	b = append(b, resultJSON...)
	b = append(b, '}', '\n')
	return b
}

func mcpRPCError(id json.RawMessage, code int, message string) []byte {
	if len(id) == 0 {
		id = []byte("null")
	}
	var b []byte
	b = append(b, `{"jsonrpc":"2.0","id":`...)
	b = append(b, id...)
	b = append(b, `,"error":{"code":`...)
	b = strconv.AppendInt(b, int64(code), 10)
	b = append(b, `,"message":`...)
	b = appendJSONString(b, message)
	b = append(b, `}}`+"\n"...)
	return b
}

func mcpToolSuccess(id json.RawMessage, payloadJSON []byte) []byte {
	var res []byte
	res = append(res, `{"content":[{"type":"text","text":`...)
	res = appendJSONString(res, string(payloadJSON))
	res = append(res, `}],"structuredContent":`...)
	res = append(res, payloadJSON...)
	res = append(res, `,"isError":false}`...)
	return mcpRPCResult(id, res)
}

func mcpToolError(id json.RawMessage, msg string) []byte {
	var res []byte
	res = append(res, `{"content":[{"type":"text","text":`...)
	res = appendJSONString(res, msg)
	res = append(res, `}],"isError":true}`...)
	return mcpRPCResult(id, res)
}

func (s *EpollServer) handleMCP(req *Request) []byte {
	if !s.store.CheckAuth(req.Auth) {
		return s.buildJSONResp(401, `{"message":"Requires authentication"}`+"\n", nil, req.KeepAlive)
	}
	if string(req.Method) != "POST" {
		return s.buildJSONResp(405, `{"message":"Method Not Allowed"}`+"\n", nil, req.KeepAlive)
	}

	var rpc mcpRequest
	if err := json.Unmarshal(req.Body, &rpc); err != nil {
		return s.buildJSONBytesResp(400, mcpRPCError(nil, -32700, "Parse error"), nil, req.KeepAlive)
	}

	switch rpc.Method {
	case "server/discover":
		result := []byte(`{"supportedVersions":["2026-07-28","2025-11-25","2025-06-18"],"capabilities":{"tools":{}},"serverInfo":{"name":"hyperforge","version":"1.0.0"},"_meta":{"io.modelcontextprotocol/serverInfo":{"name":"hyperforge","version":"1.0.0"}}}`)
		return s.buildJSONBytesResp(200, mcpRPCResult(rpc.ID, result), nil, req.KeepAlive)

	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(rpc.Params, &params)
		ver := params.ProtocolVersion
		if ver == "" {
			ver = "2025-11-25"
		}
		var result []byte
		result = append(result, `{"protocolVersion":`...)
		result = appendJSONString(result, ver)
		result = append(result, `,"capabilities":{"tools":{}},"serverInfo":{"name":"hyperforge","version":"1.0.0"}}`...)
		return s.buildJSONBytesResp(200, mcpRPCResult(rpc.ID, result), nil, req.KeepAlive)

	case "notifications/initialized":
		return s.buildJSONResp(204, "", nil, req.KeepAlive)

	case "ping":
		return s.buildJSONBytesResp(200, mcpRPCResult(rpc.ID, []byte(`{}`)), nil, req.KeepAlive)

	case "tools/list":
		return s.buildJSONBytesResp(200, mcpRPCResult(rpc.ID, []byte(staticMCPToolsJSON)), nil, req.KeepAlive)

	case "tools/call":
		return s.handleMCPToolCall(req, rpc)

	default:
		return s.buildJSONBytesResp(200, mcpRPCError(rpc.ID, -32601, "Method not found: "+rpc.Method), nil, req.KeepAlive)
	}
}

func (s *EpollServer) handleMCPToolCall(req *Request, rpc mcpRequest) []byte {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(rpc.Params, &params); err != nil {
		return s.buildJSONBytesResp(200, mcpRPCError(rpc.ID, -32602, "Invalid params"), nil, req.KeepAlive)
	}
	args := params.Arguments
	if args == nil {
		args = map[string]any{}
	}

	strArg := func(k string) string {
		if v, ok := args[k].(string); ok {
			return v
		}
		return ""
	}
	intArg := func(k string) (int, bool) {
		switch v := args[k].(type) {
		case float64:
			return int(v), true
		case int:
			return v, true
		case string:
			if n, err := strconv.Atoi(v); err == nil {
				return n, true
			}
		}
		return 0, false
	}

	switch params.Name {
	case "create_repo":
		name := strings.TrimSpace(strArg("name"))
		if name == "" {
			return s.buildJSONBytesResp(200, mcpRPCError(rpc.ID, -32602, "Missing required argument: name"), nil, req.KeepAlive)
		}
		desc := strArg("description")
		priv, _ := args["private"].(bool)
		r, ok := s.store.CreateRepo(name, desc, priv)
		if !ok {
			return s.buildJSONBytesResp(200, mcpToolError(rpc.ID, "repository already exists"), nil, req.KeepAlive)
		}
		payload := appendRepoJSON(nil, r, s.store.ViewerID)
		return s.buildJSONBytesResp(200, mcpToolSuccess(rpc.ID, payload), nil, req.KeepAlive)

	case "get_repo":
		owner := strArg("owner")
		repoName := strArg("repo")
		if owner == "" || repoName == "" {
			return s.buildJSONBytesResp(200, mcpRPCError(rpc.ID, -32602, "Missing required arguments: owner, repo"), nil, req.KeepAlive)
		}
		r := s.store.GetRepo(owner, repoName)
		if r == nil {
			return s.buildJSONBytesResp(200, mcpToolError(rpc.ID, "repository not found"), nil, req.KeepAlive)
		}
		payload := appendRepoJSON(nil, r, s.store.ViewerID)
		return s.buildJSONBytesResp(200, mcpToolSuccess(rpc.ID, payload), nil, req.KeepAlive)

	case "create_issue":
		owner := strArg("owner")
		repoName := strArg("repo")
		title := strArg("title")
		if owner == "" || repoName == "" || title == "" {
			return s.buildJSONBytesResp(200, mcpRPCError(rpc.ID, -32602, "Missing required arguments: owner, repo, title"), nil, req.KeepAlive)
		}
		r := s.store.GetRepo(owner, repoName)
		if r == nil {
			return s.buildJSONBytesResp(200, mcpToolError(rpc.ID, "repository not found"), nil, req.KeepAlive)
		}
		is, errMsg := s.store.CreateIssue(r, title, strArg("body"), nil)
		if errMsg != "" {
			return s.buildJSONBytesResp(200, mcpToolError(rpc.ID, errMsg), nil, req.KeepAlive)
		}
		payload := appendIssueJSON(nil, is, s.store.ViewerLogin, s.store.ViewerID)
		return s.buildJSONBytesResp(200, mcpToolSuccess(rpc.ID, payload), nil, req.KeepAlive)

	case "list_issues":
		owner := strArg("owner")
		repoName := strArg("repo")
		if owner == "" || repoName == "" {
			return s.buildJSONBytesResp(200, mcpRPCError(rpc.ID, -32602, "Missing required arguments: owner, repo"), nil, req.KeepAlive)
		}
		r := s.store.GetRepo(owner, repoName)
		if r == nil {
			return s.buildJSONBytesResp(200, mcpToolError(rpc.ID, "repository not found"), nil, req.KeepAlive)
		}
		state := strArg("state")
		if state == "" {
			state = "open"
		}
		var payload []byte
		payload = append(payload, '[')
		first := true
		for i := len(r.Issues) - 1; i >= 0; i-- {
			is := r.Issues[i]
			if state != "all" && is.State != state {
				continue
			}
			if !first {
				payload = append(payload, ',')
			}
			first = false
			payload = appendIssueJSON(payload, is, s.store.ViewerLogin, s.store.ViewerID)
		}
		payload = append(payload, ']')
		return s.buildJSONBytesResp(200, mcpToolSuccess(rpc.ID, payload), nil, req.KeepAlive)

	case "create_pull_request":
		owner := strArg("owner")
		repoName := strArg("repo")
		title := strArg("title")
		head := strArg("head")
		base := strArg("base")
		if owner == "" || repoName == "" || title == "" || head == "" || base == "" {
			return s.buildJSONBytesResp(200, mcpRPCError(rpc.ID, -32602, "Missing required arguments: owner, repo, title, head, base"), nil, req.KeepAlive)
		}
		r := s.store.GetRepo(owner, repoName)
		if r == nil {
			return s.buildJSONBytesResp(200, mcpToolError(rpc.ID, "repository not found"), nil, req.KeepAlive)
		}
		pr, errMsg := s.store.CreatePull(r, title, strArg("body"), head, base)
		if errMsg != "" {
			return s.buildJSONBytesResp(200, mcpToolError(rpc.ID, errMsg), nil, req.KeepAlive)
		}
		payload := appendPullJSON(nil, pr, s.store.ViewerID)
		return s.buildJSONBytesResp(200, mcpToolSuccess(rpc.ID, payload), nil, req.KeepAlive)

	case "merge_pull_request":
		owner := strArg("owner")
		repoName := strArg("repo")
		num, okNum := intArg("pull_number")
		if owner == "" || repoName == "" || !okNum || num <= 0 {
			return s.buildJSONBytesResp(200, mcpRPCError(rpc.ID, -32602, "Missing required arguments: owner, repo, pull_number"), nil, req.KeepAlive)
		}
		r := s.store.GetRepo(owner, repoName)
		if r == nil || num > len(r.Pulls) {
			return s.buildJSONBytesResp(200, mcpToolError(rpc.ID, "pull request not found"), nil, req.KeepAlive)
		}
		pr := r.Pulls[num-1]
		sha, code, msg := s.store.MergePull(pr, "")
		if code != 200 {
			return s.buildJSONBytesResp(200, mcpToolError(rpc.ID, msg), nil, req.KeepAlive)
		}
		var payload []byte
		payload = append(payload, `{"sha":`...)
		payload = appendJSONString(payload, sha)
		payload = append(payload, `,"merged":true,"message":`...)
		payload = appendJSONString(payload, msg)
		payload = append(payload, '}')
		return s.buildJSONBytesResp(200, mcpToolSuccess(rpc.ID, payload), nil, req.KeepAlive)

	default:
		return s.buildJSONBytesResp(200, mcpRPCError(rpc.ID, -32601, "Unknown tool: "+params.Name), nil, req.KeepAlive)
	}
}
