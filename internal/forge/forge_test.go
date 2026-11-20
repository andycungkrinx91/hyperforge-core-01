package forge

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestStoreRepoIssuePullLifecycle(t *testing.T) {
	store := NewStore("adaiversen", "tok", "2026-11-14T00:00:00Z")
	srv := &EpollServer{store: store, scratch: make([]byte, 0, 8192)}

	// Auth failure
	r := &Request{Method: []byte("GET"), Path: []byte("/api/v3/user/repos"), KeepAlive: true}
	resp := srv.dispatch(r)
	if !bytes.HasPrefix(resp, []byte("HTTP/1.1 401")) {
		t.Fatalf("expected 401, got %q", resp[:min(len(resp), 40)])
	}

	// Create repo
	auth := []byte("Bearer tok")
	resp = srv.dispatch(&Request{
		Method:    []byte("POST"),
		Path:      []byte("/api/v3/user/repos"),
		Auth:      auth,
		Body:      []byte(`{"name":"demo","description":"hi","private":true}`),
		KeepAlive: true,
	})
	if !bytes.HasPrefix(resp, []byte("HTTP/1.1 201")) {
		t.Fatalf("expected 201, got %s", resp)
	}

	// Fast-path issue create & cached GET
	resp = srv.dispatch(&Request{
		Method:    []byte("POST"),
		Path:      []byte("/api/v3/repos/adaiversen/demo/issues"),
		Auth:      auth,
		Body:      []byte(`{"title":"Bug 1","body":"Details"}`),
		KeepAlive: true,
	})
	if !bytes.HasPrefix(resp, []byte("HTTP/1.1 201")) {
		t.Fatalf("expected 201, got %s", resp)
	}
	get1 := srv.dispatch(&Request{
		Method:    []byte("GET"),
		Path:      []byte("/api/v3/repos/adaiversen/demo/issues/1"),
		Auth:      auth,
		KeepAlive: true,
	})
	get2 := srv.dispatch(&Request{
		Method:    []byte("GET"),
		Path:      []byte("/api/v3/repos/adaiversen/demo/issues/1"),
		Auth:      auth,
		KeepAlive: true,
	})
	if !bytes.Equal(get1, get2) {
		t.Fatalf("cached GET issue responses diverged")
	}

	// Packfile roundtrip
	repo := store.GetRepo("adaiversen", "demo")
	blob := store.PutObject(repo, ObjBlob, []byte("hello world\n"), nil)
	tree := store.PutObject(repo, ObjTree, BuildTreeBytes([]TreeEntry{{Mode: "100644", Name: "README.md", OID: blob.OID}}), nil)
	commit := store.BuildCommitObj(repo, tree.OID, nil, "init\n")
	pack := store.BuildPack([]*GitObj{blob, tree, commit})

	repo2, _ := store.CreateRepo("demo2", "", true)
	if err := store.UnpackPackfile(repo2, pack); err != nil {
		t.Fatalf("UnpackPackfile failed: %v", err)
	}
	if _, ok := repo2.Objects[commit.OID]; !ok {
		t.Fatalf("commit missing after unpack")
	}

	// MCP discover + tools/list determinism
	disc := srv.dispatch(&Request{
		Method:    []byte("POST"),
		Path:      []byte("/mcp"),
		Auth:      auth,
		Body:      []byte(`{"jsonrpc":"2.0","id":1,"method":"server/discover"}`),
		KeepAlive: true,
	})
	if !bytes.Contains(disc, []byte(`"2026-07-28"`)) {
		t.Fatalf("unexpected discover response: %s", disc)
	}

	// GraphQL invalid field returns HTTP 200 with errors
	gql := srv.dispatch(&Request{
		Method:    []byte("POST"),
		Path:      []byte("/api/graphql"),
		Auth:      auth,
		Body:      []byte(`{"query":"query { viewer { login noSuchField } }"}`),
		KeepAlive: true,
	})
	if !bytes.HasPrefix(gql, []byte("HTTP/1.1 200")) {
		t.Fatalf("expected 200 on GraphQL invalid field, got %s", gql)
	}
	idx := bytes.Index(gql, []byte("\r\n\r\n"))
	var parsed map[string]any
	if err := json.Unmarshal(gql[idx+4:], &parsed); err != nil {
		t.Fatalf("invalid graphql JSON: %v", err)
	}
	if _, hasErr := parsed["errors"]; !hasErr {
		t.Fatalf("expected GraphQL errors array, got %v", parsed)
	}
}
