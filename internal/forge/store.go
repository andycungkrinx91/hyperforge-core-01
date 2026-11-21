package forge

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"hash"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// ObjType matches Git packfile object type numbers.
type ObjType byte

const (
	ObjCommit   ObjType = 1
	ObjTree     ObjType = 2
	ObjBlob     ObjType = 3
	ObjTag      ObjType = 4
	ObjOfsDelta ObjType = 6
	ObjRefDelta ObjType = 7
)

func (t ObjType) Name() string {
	switch t {
	case ObjCommit:
		return "commit"
	case ObjTree:
		return "tree"
	case ObjBlob:
		return "blob"
	case ObjTag:
		return "tag"
	default:
		return "unknown"
	}
}

type OID [20]byte

var ZeroOID OID

const ZeroOIDHex = "0000000000000000000000000000000000000000"

const hexDigits = "0123456789abcdef"

func (o OID) Hex() string {
	var buf [40]byte
	for i, b := range o {
		buf[i*2] = hexDigits[b>>4]
		buf[i*2+1] = hexDigits[b&0x0f]
	}
	return string(buf[:])
}

func (o OID) AppendHex(dst []byte) []byte {
	var buf [40]byte
	for i, b := range o {
		buf[i*2] = hexDigits[b>>4]
		buf[i*2+1] = hexDigits[b&0x0f]
	}
	return append(dst, buf[:]...)
}

var hexValTable = func() [256]byte {
	var t [256]byte
	for i := range t {
		t[i] = 0xff
	}
	for c := byte('0'); c <= '9'; c++ {
		t[c] = c - '0'
	}
	for c := byte('a'); c <= 'f'; c++ {
		t[c] = c - 'a' + 10
	}
	for c := byte('A'); c <= 'F'; c++ {
		t[c] = c - 'A' + 10
	}
	return t
}()

func ParseOID(s string) (OID, bool) {
	var out OID
	if len(s) != 40 {
		return out, false
	}
	for i := 0; i < 20; i++ {
		hi := hexValTable[s[i*2]]
		lo := hexValTable[s[i*2+1]]
		if (hi|lo)&0x80 != 0 {
			return out, false
		}
		out[i] = (hi << 4) | lo
	}
	return out, true
}

func ParseOIDBytes(b []byte) (OID, bool) {
	var out OID
	if len(b) != 40 {
		return out, false
	}
	for i := 0; i < 20; i++ {
		hi := hexValTable[b[i*2]]
		lo := hexValTable[b[i*2+1]]
		if (hi|lo)&0x80 != 0 {
			return out, false
		}
		out[i] = (hi << 4) | lo
	}
	return out, true
}

func fromHex(c byte) (byte, bool) {
	v := hexValTable[c]
	return v, v != 0xff
}

// GitObj stores one immutable Git object and pre-parsed metadata for fast traversal.
type GitObj struct {
	Type    ObjType
	OID     OID
	Data    []byte
	ZlibRaw []byte // cached zlib-compressed payload (populated lazily on fetch/clone)

	// Commit metadata (inline Parent0 avoids slice alloc on linear commits!)
	TreeOID    OID
	Parent0    OID
	HasParent0 bool
	Parents    []OID // extra parents (index 1+), only allocated for merge commits
	Parsed     bool

	// Tag metadata (only populated for ObjTag)
	TagTarget OID
	TagType   string
	TagName   string
}

func (o *GitObj) HasParents() bool {
	return o.HasParent0 || len(o.Parents) > 0
}

func (o *GitObj) AppendParents(dst []OID) []OID {
	if o.HasParent0 {
		dst = append(dst, o.Parent0)
	}
	if len(o.Parents) > 0 {
		dst = append(dst, o.Parents...)
	}
	return dst
}

type Label struct {
	ID    int
	Name  string
	Color string
}

type Comment struct {
	ID        int
	NodeID    string
	Body      string
	Author    string
	CreatedAt string
}

type Issue struct {
	ID        int
	NodeID    string
	Repo      *Repo
	Number    int
	Title     string
	Body      string
	State     string // "open" or "closed"
	Labels    []*Label
	Comments  []*Comment
	CreatedAt string
	UpdatedAt string

	// Complete pre-rendered HTTP/1.1 200 OK response for lock-free GET /api/v3/repos/{o}/{r}/issues/{n}.
	cachedGetResp atomic.Pointer[[]byte]
}

func (is *Issue) InvalidateCache() {
	is.cachedGetResp.Store(nil)
}

type PullRequest struct {
	ID             int
	NodeID         string
	Repo           *Repo
	Number         int
	Title          string
	Body           string
	State          string // "open" or "closed"
	HeadRef        string
	HeadSHA        OID
	BaseRef        string
	BaseSHA        OID
	Merged         bool
	MergedAt       string
	MergeCommitSHA string
	CreatedAt      string
	UpdatedAt      string
}

type fetchCacheEntry struct {
	key  []byte
	resp []byte
}

type Repo struct {
	ID            int
	NodeID        string
	Owner         string
	Name          string
	Description   string
	Private       bool
	DefaultBranch string
	CreatedAt     string

	Objects map[OID]*GitObj
	Refs    map[string]OID
	HeadRef string // e.g. "refs/heads/main"

	Labels map[string]*Label
	Issues []*Issue       // 0-indexed: Issues[number-1]
	Pulls  []*PullRequest // 0-indexed: Pulls[number-1]

	// Lock-free atomic snapshots for hot read/clone/push-adv endpoints.
	issuesSnap atomic.Pointer[[]*Issue]
	fetchCache atomic.Pointer[[]fetchCacheEntry]
	recvCache  atomic.Pointer[[]byte]
	upV0Cache  atomic.Pointer[[]byte]

	// Wire cache metadata protected by Store.mu.
	RefGen              uint64
	cachedRecvSingleRef string
	cachedRecvOIDOff    int
	recvPatchRing       [4][]byte
	recvPatchIdx        int

	// Slab allocators (protected by Store.mu) to eliminate per-object GC churn on push/write.
	objSlab         []GitObj
	dataSlab        []byte
	issueSlab       []Issue
	issueNodePrefix string
}

func (r *Repo) allocIssue() *Issue {
	if len(r.issueSlab) == 0 {
		r.issueSlab = make([]Issue, 64)
	}
	is := &r.issueSlab[0]
	r.issueSlab = r.issueSlab[1:]
	return is
}

func (r *Repo) allocObj() *GitObj {
	if len(r.objSlab) == 0 {
		r.objSlab = make([]GitObj, 64)
	}
	o := &r.objSlab[0]
	r.objSlab = r.objSlab[1:]
	return o
}

func (r *Repo) allocData(n int) []byte {
	if n == 0 {
		return nil
	}
	if n > 4096 {
		return make([]byte, n)
	}
	if len(r.dataSlab) < n {
		r.dataSlab = make([]byte, 32768)
	}
	b := r.dataSlab[:n:n]
	r.dataSlab = r.dataSlab[n:]
	return b
}

func (r *Repo) BumpRefGen() {
	r.RefGen++
	r.recvCache.Store(nil)
	r.cachedRecvSingleRef = ""
	r.cachedRecvOIDOff = 0
	r.upV0Cache.Store(nil)
	r.fetchCache.Store(nil)
}

func (r *Repo) setRecvCache(httpBytes []byte, singleRef string, oidOff int) {
	r.cachedRecvSingleRef = singleRef
	r.cachedRecvOIDOff = oidOff
	r.recvCache.Store(&httpBytes)
}

func (r *Repo) putFetchCache(key, resp []byte) {
	var cur []fetchCacheEntry
	if p := r.fetchCache.Load(); p != nil {
		cur = *p
	}
	if len(cur) >= 32 {
		return
	}
	for i := range cur {
		if bytes.Equal(cur[i].key, key) {
			return
		}
	}
	keyCopy := make([]byte, len(key))
	copy(keyCopy, key)
	next := make([]fetchCacheEntry, len(cur)+1)
	copy(next, cur)
	next[len(cur)] = fetchCacheEntry{key: keyCopy, resp: resp}
	r.fetchCache.Store(&next)
}

// PatchSingleRefCache updates recvCache via immutable copy-on-write when a 1-ref repo advances.
func (r *Repo) PatchSingleRefCache(ref string, newOID OID) {
	r.RefGen++
	r.upV0Cache.Store(nil)
	r.fetchCache.Store(nil)
	prevPtr := r.recvCache.Load()
	if len(r.Refs) == 1 && prevPtr != nil && r.cachedRecvSingleRef == ref && r.cachedRecvOIDOff > 0 && r.cachedRecvOIDOff+40 <= len(*prevPtr) {
		prev := *prevPtr
		idx := r.recvPatchIdx & 3
		r.recvPatchIdx++
		next := r.recvPatchRing[idx]
		if cap(next) < len(prev) {
			next = make([]byte, len(prev))
		} else {
			next = next[:len(prev)]
		}
		copy(next, prev)
		r.recvPatchRing[idx] = next
		var hexBuf [40]byte
		for i, b := range newOID {
			hexBuf[i*2] = hexDigits[b>>4]
			hexBuf[i*2+1] = hexDigits[b&0x0f]
		}
		copy(next[r.cachedRecvOIDOff:r.cachedRecvOIDOff+40], hexBuf[:])
		r.recvCache.Store(&next)
		return
	}
	r.recvCache.Store(nil)
	r.cachedRecvSingleRef = ""
	r.cachedRecvOIDOff = 0
}

// Store holds all forge state. Mutations & cold reads are serialized by mu; hot reads are lock-free.
type Store struct {
	mu sync.Mutex

	ViewerLogin string
	ViewerName  string
	ViewerEmail string
	ViewerID    int
	Token       string
	TokenBearer []byte // "Bearer <token>"
	TrialNow    string
	CommitEpoch int64

	Repos     map[string]*Repo // keyed by repo name (owned by ViewerLogin)
	reposSnap atomic.Pointer[map[string]*Repo]
	repoEpoch atomic.Uint64
	RepoList  []*Repo // deterministic creation order

	NextRepoID    int
	NextLabelID   int
	NextIssueID   int
	NextCommentID int
	NextPRID      int
	CommitSeq     int64

	RepoByNode  map[string]*Repo
	IssueByNode map[string]*Issue
	PRByNode    map[string]*PullRequest

	// Reusable crypto/compression helpers (protected by Store.mu).
	sha1Hasher  hash.Hash
	zReader     io.ReadCloser
	zWriter     *zlib.Writer
	zBuf        bytes.Buffer
	reportBuf   []byte
	sidebandBuf []byte
	packReader  sliceByteReader
	eofProbe    [1]byte
}

func NewStore(login, token, trialNow string) *Store {
	s := &Store{
		ViewerLogin:   login,
		ViewerName:    "ada iversen",
		ViewerEmail:   login + "@example.invalid",
		ViewerID:      1000,
		Token:         token,
		TokenBearer:   []byte("Bearer " + token),
		TrialNow:      trialNow,
		CommitEpoch:   1735689600,
		Repos:         make(map[string]*Repo, 64),
		RepoList:      make([]*Repo, 0, 64),
		NextRepoID:    1,
		NextLabelID:   1,
		NextIssueID:   1,
		NextCommentID: 1,
		NextPRID:      1,
		RepoByNode:    make(map[string]*Repo, 64),
		IssueByNode:   make(map[string]*Issue, 256),
		PRByNode:      make(map[string]*PullRequest, 64),
		sha1Hasher:    sha1.New(),
	}
	if login != "adaiversen" {
		s.ViewerName = login
	}
	zw, _ := zlib.NewWriterLevel(&s.zBuf, zlib.BestCompression)
	s.zWriter = zw
	emptySnap := make(map[string]*Repo)
	s.reposSnap.Store(&emptySnap)
	return s
}

func (s *Store) publishReposSnapLocked() {
	cp := make(map[string]*Repo, len(s.Repos))
	for k, v := range s.Repos {
		cp[k] = v
	}
	s.reposSnap.Store(&cp)
}

func (s *Store) CheckAuth(authVal []byte) bool {
	if s.Token == "" {
		return true
	}
	return bytes.Equal(authVal, s.TokenBearer)
}

func (s *Store) CreateRepo(name, desc string, private bool) (*Repo, bool) {
	if _, exists := s.Repos[name]; exists {
		return nil, false
	}
	id := s.NextRepoID
	s.NextRepoID++
	nodeID := "R_" + strconv.Itoa(id)
	r := &Repo{
		ID:            id,
		NodeID:        nodeID,
		Owner:         s.ViewerLogin,
		Name:          name,
		Description:   desc,
		Private:       private,
		DefaultBranch: "main",
		CreatedAt:     s.TrialNow,
		Objects:       make(map[OID]*GitObj, 256),
		Refs:          make(map[string]OID, 8),
		HeadRef:       "refs/heads/main",
		Labels:        make(map[string]*Label, 8),
		Issues:          make([]*Issue, 0, 16),
		Pulls:           make([]*PullRequest, 0, 8),
		RefGen:          1,
		issueNodePrefix: "I_" + strconv.Itoa(id) + "_",
	}
	emptyIssues := r.Issues
	r.issuesSnap.Store(&emptyIssues)
	s.Repos[name] = r
	s.RepoList = append(s.RepoList, r)
	s.RepoByNode[nodeID] = r
	s.publishReposSnapLocked()
	return r, true
}

func (s *Store) GetRepo(owner, name string) *Repo {
	if owner != s.ViewerLogin {
		return nil
	}
	return s.Repos[name]
}

func (s *Store) GetRepoBytes(owner, name []byte) *Repo {
	if len(owner) != len(s.ViewerLogin) || string(owner) != s.ViewerLogin {
		return nil
	}
	return s.Repos[string(name)]
}

func (s *Store) GetRepoLockFree(owner, name []byte) *Repo {
	if len(owner) != len(s.ViewerLogin) || string(owner) != s.ViewerLogin {
		return nil
	}
	m := s.reposSnap.Load()
	if m == nil {
		return nil
	}
	return (*m)[string(name)]
}

func (s *Store) DeleteRepo(owner, name string) bool {
	if owner != s.ViewerLogin {
		return false
	}
	r, ok := s.Repos[name]
	if !ok {
		return false
	}
	delete(s.Repos, name)
	delete(s.RepoByNode, r.NodeID)
	for _, is := range r.Issues {
		delete(s.IssueByNode, is.NodeID)
	}
	for _, pr := range r.Pulls {
		delete(s.PRByNode, pr.NodeID)
	}
	for i, candidate := range s.RepoList {
		if candidate == r {
			s.RepoList = append(s.RepoList[:i], s.RepoList[i+1:]...)
			break
		}
	}
	s.repoEpoch.Add(1)
	s.publishReposSnapLocked()
	return true
}

func (s *Store) CreateLabel(r *Repo, name, color string) *Label {
	if existing, ok := r.Labels[name]; ok {
		if color != "" {
			existing.Color = color
		}
		return existing
	}
	if color == "" {
		color = "ededed"
	}
	id := s.NextLabelID
	s.NextLabelID++
	lbl := &Label{ID: id, Name: name, Color: color}
	r.Labels[name] = lbl
	return lbl
}

func (s *Store) CreateIssue(r *Repo, title, body string, labelNames []string) (*Issue, string) {
	var labels []*Label
	if len(labelNames) > 0 {
		labels = make([]*Label, 0, len(labelNames))
		for _, ln := range labelNames {
			lbl, ok := r.Labels[ln]
			if !ok {
				return nil, "unknown label: " + ln
			}
			labels = append(labels, lbl)
		}
	}
	id := s.NextIssueID
	s.NextIssueID++
	num := len(r.Issues) + 1
	var nodeBuf [32]byte
	nb := append(nodeBuf[:0], r.issueNodePrefix...)
	nb = strconv.AppendInt(nb, int64(num), 10)
	nodeID := string(nb)
	is := r.allocIssue()
	*is = Issue{
		ID:        id,
		NodeID:    nodeID,
		Repo:      r,
		Number:    num,
		Title:     title,
		Body:      body,
		State:     "open",
		Labels:    labels,
		CreatedAt: s.TrialNow,
		UpdatedAt: s.TrialNow,
	}
	r.Issues = append(r.Issues, is)
	cp := r.Issues
	r.issuesSnap.Store(&cp)
	s.IssueByNode[nodeID] = is
	return is, ""
}

func (s *Store) AddComment(is *Issue, body string) *Comment {
	id := s.NextCommentID
	s.NextCommentID++
	c := &Comment{
		ID:        id,
		NodeID:    "IC_" + strconv.Itoa(id),
		Body:      body,
		Author:    s.ViewerLogin,
		CreatedAt: s.TrialNow,
	}
	is.Comments = append(is.Comments, c)
	return c
}

func (s *Store) CreatePull(r *Repo, title, body, headRef, baseRef string) (*PullRequest, string) {
	headRef = strings.TrimPrefix(headRef, "refs/heads/")
	baseRef = strings.TrimPrefix(baseRef, "refs/heads/")
	if baseRef == "" {
		baseRef = r.DefaultBranch
	}
	headOID, okH := r.Refs["refs/heads/"+headRef]
	if !okH {
		return nil, "head branch not found: " + headRef
	}
	baseOID, okB := r.Refs["refs/heads/"+baseRef]
	if !okB {
		return nil, "base branch not found: " + baseRef
	}
	id := s.NextPRID
	s.NextPRID++
	num := len(r.Pulls) + 1
	nodeID := "PR_" + strconv.Itoa(r.ID) + "_" + strconv.Itoa(num)
	pr := &PullRequest{
		ID:        id,
		NodeID:    nodeID,
		Repo:      r,
		Number:    num,
		Title:     title,
		Body:      body,
		State:     "open",
		HeadRef:   headRef,
		HeadSHA:   headOID,
		BaseRef:   baseRef,
		BaseSHA:   baseOID,
		CreatedAt: s.TrialNow,
		UpdatedAt: s.TrialNow,
	}
	r.Pulls = append(r.Pulls, pr)
	s.PRByNode[nodeID] = pr
	return pr, ""
}
