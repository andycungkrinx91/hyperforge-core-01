package forge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// -------------------------------------------------------------------------
// GraphQL Lexer & Recursive-Descent Parser
// -------------------------------------------------------------------------

type gqlTokKind int

const (
	tokEOF gqlTokKind = iota
	tokName
	tokInt
	tokString
	tokPunct // { } ( ) [ ] : $ = ! @ ...
)

type gqlTok struct {
	kind gqlTokKind
	val  string
}

func lexGraphQL(src string) []gqlTok {
	var out []gqlTok
	i := 0
	n := len(src)
	for i < n {
		c := src[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			i++
			continue
		}
		if c == '#' {
			for i < n && src[i] != '\n' {
				i++
			}
			continue
		}
		if c == '.' && i+2 < n && src[i+1] == '.' && src[i+2] == '.' {
			out = append(out, gqlTok{kind: tokPunct, val: "..."})
			i += 3
			continue
		}
		if c == '{' || c == '}' || c == '(' || c == ')' || c == '[' || c == ']' || c == ':' || c == '$' || c == '=' || c == '!' || c == '@' {
			out = append(out, gqlTok{kind: tokPunct, val: string(c)})
			i++
			continue
		}
		if c == '"' {
			i++
			var sb strings.Builder
			for i < n && src[i] != '"' {
				if src[i] == '\\' && i+1 < n {
					i++
					switch src[i] {
					case '"':
						sb.WriteByte('"')
					case '\\':
						sb.WriteByte('\\')
					case 'n':
						sb.WriteByte('\n')
					case 'r':
						sb.WriteByte('\r')
					case 't':
						sb.WriteByte('\t')
					default:
						sb.WriteByte(src[i])
					}
				} else {
					sb.WriteByte(src[i])
				}
				i++
			}
			if i < n {
				i++
			}
			out = append(out, gqlTok{kind: tokString, val: sb.String()})
			continue
		}
		if (c >= '0' && c <= '9') || c == '-' {
			start := i
			i++
			for i < n && src[i] >= '0' && src[i] <= '9' {
				i++
			}
			out = append(out, gqlTok{kind: tokInt, val: src[start:i]})
			continue
		}
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' {
			start := i
			i++
			for i < n && ((src[i] >= 'a' && src[i] <= 'z') || (src[i] >= 'A' && src[i] <= 'Z') || (src[i] >= '0' && src[i] <= '9') || src[i] == '_') {
				i++
			}
			out = append(out, gqlTok{kind: tokName, val: src[start:i]})
			continue
		}
		i++
	}
	out = append(out, gqlTok{kind: tokEOF})
	return out
}

type gqlField struct {
	alias     string
	name      string
	args      map[string]any
	selection []*gqlSelection
}

type gqlSelection struct {
	field      *gqlField
	inlineType string
	inlineSel  []*gqlSelection
}

type gqlOp struct {
	opType    string // "query" or "mutation"
	name      string
	selection []*gqlSelection
}

type gqlParser struct {
	toks []gqlTok
	pos  int
	vars map[string]any
}

func (p *gqlParser) peek() gqlTok {
	if p.pos >= len(p.toks) {
		return gqlTok{kind: tokEOF}
	}
	return p.toks[p.pos]
}

func (p *gqlParser) next() gqlTok {
	t := p.peek()
	if p.pos < len(p.toks) {
		p.pos++
	}
	return t
}

func (p *gqlParser) parseDocument() ([]*gqlOp, error) {
	var ops []*gqlOp
	for p.peek().kind != tokEOF {
		t := p.peek()
		if t.kind == tokPunct && t.val == "{" {
			sel, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			ops = append(ops, &gqlOp{opType: "query", selection: sel})
			continue
		}
		if t.kind == tokName && (t.val == "query" || t.val == "mutation") {
			opType := p.next().val
			opName := ""
			if p.peek().kind == tokName {
				opName = p.next().val
			}
			// Skip variable definitions (...)
			if p.peek().kind == tokPunct && p.peek().val == "(" {
				depth := 0
				for p.peek().kind != tokEOF {
					tok := p.next()
					if tok.kind == tokPunct && tok.val == "(" {
						depth++
					} else if tok.kind == tokPunct && tok.val == ")" {
						depth--
						if depth == 0 {
							break
						}
					}
				}
			}
			sel, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			ops = append(ops, &gqlOp{opType: opType, name: opName, selection: sel})
			continue
		}
		return nil, fmt.Errorf("unexpected token %q", t.val)
	}
	return ops, nil
}

func (p *gqlParser) parseSelectionSet() ([]*gqlSelection, error) {
	if t := p.next(); t.kind != tokPunct || t.val != "{" {
		return nil, fmt.Errorf("expected '{', got %q", t.val)
	}
	var sels []*gqlSelection
	for p.peek().kind != tokEOF && !(p.peek().kind == tokPunct && p.peek().val == "}") {
		if p.peek().kind == tokPunct && p.peek().val == "..." {
			p.next()
			typeCond := ""
			if p.peek().kind == tokName && p.peek().val == "on" {
				p.next()
				typeCond = p.next().val
			}
			sub, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			sels = append(sels, &gqlSelection{inlineType: typeCond, inlineSel: sub})
			continue
		}
		nameTok := p.next()
		if nameTok.kind != tokName {
			return nil, fmt.Errorf("expected field name, got %q", nameTok.val)
		}
		alias := ""
		name := nameTok.val
		if p.peek().kind == tokPunct && p.peek().val == ":" {
			p.next()
			alias = name
			name = p.next().val
		}
		var args map[string]any
		if p.peek().kind == tokPunct && p.peek().val == "(" {
			var err error
			args, err = p.parseArgs()
			if err != nil {
				return nil, err
			}
		}
		var sub []*gqlSelection
		if p.peek().kind == tokPunct && p.peek().val == "{" {
			var err error
			sub, err = p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
		}
		sels = append(sels, &gqlSelection{
			field: &gqlField{alias: alias, name: name, args: args, selection: sub},
		})
	}
	if t := p.next(); t.kind != tokPunct || t.val != "}" {
		return nil, fmt.Errorf("expected '}', got %q", t.val)
	}
	return sels, nil
}

func (p *gqlParser) parseArgs() (map[string]any, error) {
	p.next() // '('
	args := make(map[string]any)
	for p.peek().kind != tokEOF && !(p.peek().kind == tokPunct && p.peek().val == ")") {
		k := p.next().val
		if t := p.next(); t.kind != tokPunct || t.val != ":" {
			return nil, fmt.Errorf("expected ':' after arg %s", k)
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args[k] = v
	}
	if t := p.next(); t.kind != tokPunct || t.val != ")" {
		return nil, fmt.Errorf("expected ')'")
	}
	return args, nil
}

func (p *gqlParser) parseValue() (any, error) {
	t := p.next()
	switch t.kind {
	case tokString:
		return t.val, nil
	case tokInt:
		n, _ := strconv.Atoi(t.val)
		return n, nil
	case tokName:
		switch t.val {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "null":
			return nil, nil
		default:
			return t.val, nil // enum literal
		}
	case tokPunct:
		switch t.val {
		case "$":
			varName := p.next().val
			if p.vars == nil {
				return nil, nil
			}
			return p.vars[varName], nil
		case "[":
			var list []any
			for p.peek().kind != tokEOF && !(p.peek().kind == tokPunct && p.peek().val == "]") {
				v, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				list = append(list, v)
			}
			p.next() // ']'
			return list, nil
		case "{":
			obj := make(map[string]any)
			for p.peek().kind != tokEOF && !(p.peek().kind == tokPunct && p.peek().val == "}") {
				k := p.next().val
				if col := p.next(); col.kind != tokPunct || col.val != ":" {
					return nil, fmt.Errorf("expected ':' in input object")
				}
				v, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				obj[k] = v
			}
			p.next() // '}'
			return obj, nil
		}
	}
	return nil, fmt.Errorf("unexpected value token %q", t.val)
}

// -------------------------------------------------------------------------
// GraphQL Schema Introspection & Execution
// -------------------------------------------------------------------------

var gqlSchemaFields = map[string][]string{
	"User":        {"id", "login", "name", "email"},
	"Repository":  {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest"},
	"Issue":       {"id", "number", "title", "state", "body", "comments"},
	"PullRequest": {"id", "number", "title", "body", "headRefName", "baseRefName", "mergeable", "state", "merged", "files"},
	"Ref":         {"name"},
}

var gqlSchemaEnums = map[string][]string{
	"SearchType":           {"ISSUE", "ISSUE_ADVANCED", "REPOSITORY", "USER", "DISCUSSION"},
	"IssueState":           {"OPEN", "CLOSED"},
	"PullRequestState":     {"OPEN", "CLOSED", "MERGED"},
	"MergeableState":       {"MERGEABLE", "CONFLICTING", "UNKNOWN"},
	"PullRequestMergeMethod": {"MERGE", "SQUASH", "REBASE"},
	"RepositoryPermission": {"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"},
	"IssueOrderField":      {"CREATED_AT", "UPDATED_AT", "COMMENTS"},
	"OrderDirection":       {"ASC", "DESC"},
}

type gqlReqBody struct {
	Query         string         `json:"query"`
	Variables     map[string]any `json:"variables"`
	OperationName string         `json:"operationName"`
}

type gqlErr struct {
	Message string `json:"message"`
}

type gqlResp struct {
	Data   any      `json:"data,omitempty"`
	Errors []gqlErr `json:"errors,omitempty"`
}

func (s *EpollServer) handleGraphQL(req *Request) []byte {
	if !s.store.CheckAuth(req.Auth) {
		return s.buildJSONResp(401, `{"message":"Bad credentials"}`+"\n", nil, req.KeepAlive)
	}
	if string(req.Method) != "POST" {
		return s.buildJSONResp(405, `{"message":"Method Not Allowed"}`+"\n", nil, req.KeepAlive)
	}
	trimmed := bytes.TrimSpace(req.Body)
	if len(trimmed) == 0 {
		return s.buildJSONResp(400, `{"message":"Empty body"}`+"\n", nil, req.KeepAlive)
	}
	if trimmed[0] == '[' {
		var batch []gqlReqBody
		if err := json.Unmarshal(trimmed, &batch); err != nil {
			return s.buildJSONResp(400, `{"message":"Problems parsing JSON"}`+"\n", nil, req.KeepAlive)
		}
		results := make([]gqlResp, len(batch))
		for i, item := range batch {
			results[i] = s.execGraphQL(item)
		}
		raw, _ := json.Marshal(results)
		raw = append(raw, '\n')
		return s.buildJSONBytesResp(200, raw, nil, req.KeepAlive)
	}
	var single gqlReqBody
	if err := json.Unmarshal(trimmed, &single); err != nil {
		return s.buildJSONResp(400, `{"message":"Problems parsing JSON"}`+"\n", nil, req.KeepAlive)
	}
	res := s.execGraphQL(single)
	raw, _ := json.Marshal(res)
	raw = append(raw, '\n')
	return s.buildJSONBytesResp(200, raw, nil, req.KeepAlive)
}

func (s *EpollServer) execGraphQL(req gqlReqBody) gqlResp {
	p := &gqlParser{toks: lexGraphQL(req.Query), vars: req.Variables}
	ops, err := p.parseDocument()
	if err != nil || len(ops) == 0 {
		msg := "syntax error"
		if err != nil {
			msg = err.Error()
		}
		return gqlResp{Errors: []gqlErr{{Message: msg}}}
	}
	op := ops[0]
	if req.OperationName != "" {
		for _, candidate := range ops {
			if candidate.name == req.OperationName {
				op = candidate
				break
			}
		}
	}

	var errs []gqlErr
	data := make(map[string]any)
	if op.opType == "mutation" {
		s.evalMutation(op.selection, data, &errs)
	} else {
		s.evalQuery(op.selection, data, &errs)
	}
	if len(errs) > 0 {
		return gqlResp{Errors: errs}
	}
	return gqlResp{Data: data}
}

func fieldOutKey(f *gqlField) string {
	if f.alias != "" {
		return f.alias
	}
	return f.name
}

func argString(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	if s, ok := args[key].(string); ok {
		return s
	}
	return ""
}

func argInt(args map[string]any, key string, def int) int {
	if args == nil {
		return def
	}
	switch v := args[key].(type) {
	case int:
		return v
	case float64:
		return int(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n)
		}
	}
	return def
}

func argMap(args map[string]any, key string) map[string]any {
	if args == nil {
		return nil
	}
	if m, ok := args[key].(map[string]any); ok {
		return m
	}
	return nil
}

func expandSelections(sels []*gqlSelection, typeName string) []*gqlField {
	var out []*gqlField
	for _, sel := range sels {
		if sel.field != nil {
			out = append(out, sel.field)
		} else if sel.inlineType == "" || sel.inlineType == typeName {
			out = append(out, expandSelections(sel.inlineSel, typeName)...)
		}
	}
	return out
}

func (s *EpollServer) evalQuery(sels []*gqlSelection, out map[string]any, errs *[]gqlErr) {
	for _, f := range expandSelections(sels, "Query") {
		key := fieldOutKey(f)
		switch f.name {
		case "viewer":
			out[key] = s.evalUser(f.selection, errs)
		case "repository":
			owner := argString(f.args, "owner")
			name := argString(f.args, "name")
			repo := s.store.GetRepo(owner, name)
			if repo == nil {
				out[key] = nil
			} else {
				out[key] = s.evalRepo(repo, f.selection, errs)
			}
		case "node":
			id := argString(f.args, "id")
			if repo, ok := s.store.RepoByNode[id]; ok {
				out[key] = s.evalRepo(repo, f.selection, errs)
			} else if is, ok := s.store.IssueByNode[id]; ok {
				out[key] = s.evalIssue(is, f.selection, errs)
			} else if pr, ok := s.store.PRByNode[id]; ok {
				out[key] = s.evalPull(pr, f.selection, errs)
			} else {
				out[key] = nil
			}
		case "__type":
			tname := argString(f.args, "name")
			out[key] = evalIntrospectionType(tname, f.selection)
		default:
			*errs = append(*errs, gqlErr{Message: fmt.Sprintf("Field '%s' doesn't exist on type 'Query'", f.name)})
		}
	}
}

func evalIntrospectionType(typeName string, sels []*gqlSelection) any {
	fields, hasFields := gqlSchemaFields[typeName]
	enums, hasEnums := gqlSchemaEnums[typeName]
	if !hasFields && !hasEnums {
		return nil
	}
	res := make(map[string]any)
	for _, f := range expandSelections(sels, "__Type") {
		key := fieldOutKey(f)
		switch f.name {
		case "name":
			res[key] = typeName
		case "fields":
			if !hasFields {
				res[key] = nil
			} else {
				list := make([]map[string]any, 0, len(fields))
				for _, fn := range fields {
					item := make(map[string]any)
					for _, sf := range expandSelections(f.selection, "__Field") {
						if sf.name == "name" {
							item[fieldOutKey(sf)] = fn
						}
					}
					list = append(list, item)
				}
				res[key] = list
			}
		case "enumValues":
			if !hasEnums {
				res[key] = nil
			} else {
				list := make([]map[string]any, 0, len(enums))
				for _, en := range enums {
					item := make(map[string]any)
					for _, sf := range expandSelections(f.selection, "__EnumValue") {
						if sf.name == "name" {
							item[fieldOutKey(sf)] = en
						}
					}
					list = append(list, item)
				}
				res[key] = list
			}
		}
	}
	return res
}

func (s *EpollServer) evalUser(sels []*gqlSelection, errs *[]gqlErr) map[string]any {
	res := make(map[string]any)
	for _, f := range expandSelections(sels, "User") {
		key := fieldOutKey(f)
		switch f.name {
		case "id":
			res[key] = "U_" + strconv.Itoa(s.store.ViewerID)
		case "login":
			res[key] = s.store.ViewerLogin
		case "name":
			res[key] = s.store.ViewerName
		case "email":
			res[key] = s.store.ViewerEmail
		case "__typename":
			res[key] = "User"
		default:
			*errs = append(*errs, gqlErr{Message: fmt.Sprintf("Field '%s' doesn't exist on type 'User'", f.name)})
		}
	}
	return res
}

func (s *EpollServer) evalRepo(r *Repo, sels []*gqlSelection, errs *[]gqlErr) map[string]any {
	res := make(map[string]any)
	for _, f := range expandSelections(sels, "Repository") {
		key := fieldOutKey(f)
		switch f.name {
		case "id":
			res[key] = r.NodeID
		case "databaseId":
			res[key] = r.ID
		case "name":
			res[key] = r.Name
		case "nameWithOwner":
			res[key] = r.Owner + "/" + r.Name
		case "owner":
			res[key] = s.evalUser(f.selection, errs)
		case "defaultBranchRef":
			if _, ok := r.Refs["refs/heads/"+r.DefaultBranch]; !ok {
				res[key] = nil
			} else {
				refObj := make(map[string]any)
				for _, sf := range expandSelections(f.selection, "Ref") {
					if sf.name == "name" {
						refObj[fieldOutKey(sf)] = r.DefaultBranch
					}
				}
				res[key] = refObj
			}
		case "viewerPermission":
			res[key] = "ADMIN"
		case "hasIssuesEnabled":
			res[key] = true
		case "__typename":
			res[key] = "Repository"
		case "issue":
			num := argInt(f.args, "number", 0)
			if num <= 0 || num > len(r.Issues) {
				res[key] = nil
			} else {
				res[key] = s.evalIssue(r.Issues[num-1], f.selection, errs)
			}
		case "issues":
			res[key] = s.evalIssueConnection(r, f.args, f.selection, errs)
		case "pullRequest":
			num := argInt(f.args, "number", 0)
			if num <= 0 || num > len(r.Pulls) {
				res[key] = nil
			} else {
				res[key] = s.evalPull(r.Pulls[num-1], f.selection, errs)
			}
		case "pullRequests":
			res[key] = s.evalPullConnection(r, f.args, f.selection, errs)
		default:
			*errs = append(*errs, gqlErr{Message: fmt.Sprintf("Field '%s' doesn't exist on type 'Repository'", f.name)})
		}
	}
	return res
}

func (s *EpollServer) evalIssue(is *Issue, sels []*gqlSelection, errs *[]gqlErr) map[string]any {
	res := make(map[string]any)
	for _, f := range expandSelections(sels, "Issue") {
		key := fieldOutKey(f)
		switch f.name {
		case "id":
			res[key] = is.NodeID
		case "number":
			res[key] = is.Number
		case "title":
			res[key] = is.Title
		case "body":
			res[key] = is.Body
		case "state":
			res[key] = strings.ToUpper(is.State)
		case "__typename":
			res[key] = "Issue"
		default:
			*errs = append(*errs, gqlErr{Message: fmt.Sprintf("Field '%s' doesn't exist on type 'Issue'", f.name)})
		}
	}
	return res
}

func (s *EpollServer) evalIssueConnection(r *Repo, args map[string]any, sels []*gqlSelection, errs *[]gqlErr) map[string]any {
	wantOpen, wantClosed := true, true
	if rawStates, ok := args["states"]; ok && rawStates != nil {
		if list, ok := rawStates.([]any); ok && len(list) > 0 {
			wantOpen, wantClosed = false, false
			for _, st := range list {
				if str, ok := st.(string); ok {
					if strings.EqualFold(str, "OPEN") {
						wantOpen = true
					} else if strings.EqualFold(str, "CLOSED") {
						wantClosed = true
					}
				}
			}
		}
	}

	desc := false
	if ob := argMap(args, "orderBy"); ob != nil {
		if dir, ok := ob["direction"].(string); ok && strings.EqualFold(dir, "DESC") {
			desc = true
		}
	}

	filtered := make([]*Issue, 0, len(r.Issues))
	if desc {
		for i := len(r.Issues) - 1; i >= 0; i-- {
			is := r.Issues[i]
			if (is.State == "open" && wantOpen) || (is.State == "closed" && wantClosed) {
				filtered = append(filtered, is)
			}
		}
	} else {
		for _, is := range r.Issues {
			if (is.State == "open" && wantOpen) || (is.State == "closed" && wantClosed) {
				filtered = append(filtered, is)
			}
		}
	}

	start := 0
	if after := argString(args, "after"); after != "" {
		if strings.HasPrefix(after, "cursor:") {
			if idx, err := strconv.Atoi(after[7:]); err == nil && idx >= 0 {
				start = idx + 1
			}
		}
	}
	if start > len(filtered) {
		start = len(filtered)
	}
	first := argInt(args, "first", len(filtered)-start)
	if first < 0 {
		first = 0
	}
	end := start + first
	if end > len(filtered) {
		end = len(filtered)
	}
	hasNext := end < len(filtered)
	var endCursor any
	if end > start {
		endCursor = "cursor:" + strconv.Itoa(end-1)
	}

	res := make(map[string]any)
	for _, f := range expandSelections(sels, "IssueConnection") {
		key := fieldOutKey(f)
		switch f.name {
		case "nodes":
			nodes := make([]map[string]any, 0, end-start)
			for _, is := range filtered[start:end] {
				nodes = append(nodes, s.evalIssue(is, f.selection, errs))
			}
			res[key] = nodes
		case "pageInfo":
			pi := make(map[string]any)
			for _, sf := range expandSelections(f.selection, "PageInfo") {
				switch sf.name {
				case "hasNextPage":
					pi[fieldOutKey(sf)] = hasNext
				case "endCursor":
					pi[fieldOutKey(sf)] = endCursor
				}
			}
			res[key] = pi
		case "totalCount":
			res[key] = len(filtered)
		}
	}
	return res
}

func (s *EpollServer) evalPull(pr *PullRequest, sels []*gqlSelection, errs *[]gqlErr) map[string]any {
	res := make(map[string]any)
	for _, f := range expandSelections(sels, "PullRequest") {
		key := fieldOutKey(f)
		switch f.name {
		case "id":
			res[key] = pr.NodeID
		case "number":
			res[key] = pr.Number
		case "title":
			res[key] = pr.Title
		case "body":
			res[key] = pr.Body
		case "headRefName":
			res[key] = pr.HeadRef
		case "baseRefName":
			res[key] = pr.BaseRef
		case "merged":
			res[key] = pr.Merged
		case "state":
			if pr.Merged {
				res[key] = "MERGED"
			} else {
				res[key] = strings.ToUpper(pr.State)
			}
		case "mergeable":
			if pr.Merged {
				res[key] = "UNKNOWN"
			} else {
				baseOID := pr.BaseSHA
				if cur, ok := pr.Repo.Refs["refs/heads/"+pr.BaseRef]; ok {
					baseOID = cur
				}
				_, conflict := ThreeWayMerge(pr.Repo, baseOID, pr.HeadSHA)
				if conflict {
					res[key] = "CONFLICTING"
				} else {
					res[key] = "MERGEABLE"
				}
			}
		case "files":
			diffFiles := DiffCommits(pr.Repo, pr.BaseSHA, pr.HeadSHA)
			first := argInt(f.args, "first", len(diffFiles))
			if first < len(diffFiles) {
				diffFiles = diffFiles[:first]
			}
			conn := make(map[string]any)
			for _, sf := range expandSelections(f.selection, "PullRequestChangedFileConnection") {
				if sf.name == "nodes" {
					nodes := make([]map[string]any, 0, len(diffFiles))
					for _, df := range diffFiles {
						node := make(map[string]any)
						for _, nf := range expandSelections(sf.selection, "PullRequestChangedFile") {
							switch nf.name {
							case "path":
								node[fieldOutKey(nf)] = df.Filename
							case "additions":
								node[fieldOutKey(nf)] = df.Additions
							case "deletions":
								node[fieldOutKey(nf)] = df.Deletions
							}
						}
						nodes = append(nodes, node)
					}
					conn[fieldOutKey(sf)] = nodes
				}
			}
			res[key] = conn
		case "__typename":
			res[key] = "PullRequest"
		default:
			*errs = append(*errs, gqlErr{Message: fmt.Sprintf("Field '%s' doesn't exist on type 'PullRequest'", f.name)})
		}
	}
	return res
}

func (s *EpollServer) evalPullConnection(r *Repo, args map[string]any, sels []*gqlSelection, errs *[]gqlErr) map[string]any {
	first := argInt(args, "first", len(r.Pulls))
	list := r.Pulls
	if first < len(list) {
		list = list[:first]
	}
	res := make(map[string]any)
	for _, f := range expandSelections(sels, "PullRequestConnection") {
		if f.name == "nodes" {
			nodes := make([]map[string]any, 0, len(list))
			for _, pr := range list {
				nodes = append(nodes, s.evalPull(pr, f.selection, errs))
			}
			res[fieldOutKey(f)] = nodes
		}
	}
	return res
}

func (s *EpollServer) evalMutation(sels []*gqlSelection, out map[string]any, errs *[]gqlErr) {
	for _, f := range expandSelections(sels, "Mutation") {
		key := fieldOutKey(f)
		input := argMap(f.args, "input")
		switch f.name {
		case "createIssue":
			repoID, _ := input["repositoryId"].(string)
			title, _ := input["title"].(string)
			body, _ := input["body"].(string)
			repo, ok := s.store.RepoByNode[repoID]
			if !ok {
				*errs = append(*errs, gqlErr{Message: "repository not found"})
				return
			}
			is, errMsg := s.store.CreateIssue(repo, title, body, nil)
			if errMsg != "" {
				*errs = append(*errs, gqlErr{Message: errMsg})
				return
			}
			payload := make(map[string]any)
			for _, sf := range expandSelections(f.selection, "CreateIssuePayload") {
				if sf.name == "issue" {
					payload[fieldOutKey(sf)] = s.evalIssue(is, sf.selection, errs)
				}
			}
			out[key] = payload

		case "closeIssue":
			issueID, _ := input["issueId"].(string)
			is, ok := s.store.IssueByNode[issueID]
			if !ok {
				*errs = append(*errs, gqlErr{Message: "issue not found"})
				return
			}
			is.State = "closed"
			is.InvalidateCache()
			payload := make(map[string]any)
			for _, sf := range expandSelections(f.selection, "CloseIssuePayload") {
				if sf.name == "issue" {
					payload[fieldOutKey(sf)] = s.evalIssue(is, sf.selection, errs)
				}
			}
			out[key] = payload

		case "createPullRequest":
			repoID, _ := input["repositoryId"].(string)
			baseRef, _ := input["baseRefName"].(string)
			headRef, _ := input["headRefName"].(string)
			title, _ := input["title"].(string)
			body, _ := input["body"].(string)
			repo, ok := s.store.RepoByNode[repoID]
			if !ok {
				*errs = append(*errs, gqlErr{Message: "repository not found"})
				return
			}
			pr, errMsg := s.store.CreatePull(repo, title, body, headRef, baseRef)
			if errMsg != "" {
				*errs = append(*errs, gqlErr{Message: errMsg})
				return
			}
			payload := make(map[string]any)
			for _, sf := range expandSelections(f.selection, "CreatePullRequestPayload") {
				if sf.name == "pullRequest" {
					payload[fieldOutKey(sf)] = s.evalPull(pr, sf.selection, errs)
				}
			}
			out[key] = payload

		case "mergePullRequest":
			prID, _ := input["pullRequestId"].(string)
			pr, ok := s.store.PRByNode[prID]
			if !ok {
				*errs = append(*errs, gqlErr{Message: "pull request not found"})
				return
			}
			_, code, msg := s.store.MergePull(pr, "")
			if code != 200 {
				*errs = append(*errs, gqlErr{Message: msg})
				return
			}
			payload := make(map[string]any)
			for _, sf := range expandSelections(f.selection, "MergePullRequestPayload") {
				if sf.name == "pullRequest" {
					payload[fieldOutKey(sf)] = s.evalPull(pr, sf.selection, errs)
				}
			}
			out[key] = payload

		case "addComment":
			subjectID, _ := input["subjectId"].(string)
			body, _ := input["body"].(string)
			is, ok := s.store.IssueByNode[subjectID]
			if !ok {
				*errs = append(*errs, gqlErr{Message: "subject not found"})
				return
			}
			c := s.store.AddComment(is, body)
			is.InvalidateCache()
			payload := make(map[string]any)
			for _, sf := range expandSelections(f.selection, "AddCommentPayload") {
				if sf.name == "commentEdge" {
					edge := make(map[string]any)
					for _, ef := range expandSelections(sf.selection, "IssueCommentEdge") {
						if ef.name == "node" {
							node := make(map[string]any)
							for _, nf := range expandSelections(ef.selection, "IssueComment") {
								switch nf.name {
								case "id":
									node[fieldOutKey(nf)] = c.NodeID
								case "body":
									node[fieldOutKey(nf)] = c.Body
								}
							}
							edge[fieldOutKey(ef)] = node
						}
					}
					payload[fieldOutKey(sf)] = edge
				}
			}
			out[key] = payload

		default:
			*errs = append(*errs, gqlErr{Message: fmt.Sprintf("Field '%s' doesn't exist on type 'Mutation'", f.name)})
		}
	}
}
