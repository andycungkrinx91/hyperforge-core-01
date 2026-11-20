package forge

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"hash"
	"io"
	"strconv"
	"strings"
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

func ParseOID(s string) (OID, bool) {
	var out OID
	if len(s) != 40 {
		return out, false
	}
	for i := 0; i < 20; i++ {
		hi, ok1 := fromHex(s[i*2])
		lo, ok2 := fromHex(s[i*2+1])
		if !ok1 || !ok2 {
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
		hi, ok1 := fromHex(b[i*2])
		lo, ok2 := fromHex(b[i*2+1])
		if !ok1 || !ok2 {
			return out, false
		}
		out[i] = (hi << 4) | lo
	}
	return out, true
}

func fromHex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	default:
		return 0, false
	}
}

// GitObj stores one immutable Git object and pre-parsed metadata for fast traversal.
type GitObj struct {
	Type    ObjType
	OID     OID
	Data    []byte
	ZlibRaw []byte // cached zlib-compressed payload (populated lazily or on ingest)

	// Commit metadata (only populated for ObjCommit)
	TreeOID   OID
	Parents   []OID
	Message   string
	Author    string
	Committer string
	Parsed    bool

	// Tag metadata (only populated for ObjTag)
	TagTarget OID
	TagType   string
	TagName   string
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

	// Complete pre-rendered HTTP/1.1 200 OK response for GET /api/v3/repos/{o}/{r}/issues/{n}.
	cachedGetResp []byte
}

func (is *Issue) InvalidateCache() {
	is.cachedGetResp = nil
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

	// Wire caches invalidated whenever RefGen increments.
	RefGen              uint64
	cachedRecvGen       uint64
	cachedRecvHTTP      []byte
	cachedRecvSingleRef string
	cachedRecvOIDOff    int
	cachedUpV0Gen       uint64
	cachedUpV0HTTP      []byte
	cachedLsRefsGen     uint64
	cachedLsRefs        map[string][]byte
	cachedFetchGen      uint64
	cachedFetch         map[string][]byte
}

func (r *Repo) BumpRefGen() {
	r.RefGen++
	r.cachedRecvHTTP = nil
	r.cachedRecvSingleRef = ""
	r.cachedRecvOIDOff = 0
	r.cachedUpV0HTTP = nil
	if len(r.cachedLsRefs) > 0 {
		clear(r.cachedLsRefs)
	}
	if len(r.cachedFetch) > 0 {
		clear(r.cachedFetch)
	}
}

// PatchSingleRefCache updates cachedRecvHTTP in-place when a 1-ref repo advances its single ref.
func (r *Repo) PatchSingleRefCache(ref string, newOID OID) {
	r.RefGen++
	r.cachedUpV0HTTP = nil
	if len(r.cachedLsRefs) > 0 {
		clear(r.cachedLsRefs)
	}
	if len(r.cachedFetch) > 0 {
		clear(r.cachedFetch)
	}
	if len(r.Refs) == 1 && r.cachedRecvHTTP != nil && r.cachedRecvSingleRef == ref && r.cachedRecvOIDOff > 0 && r.cachedRecvOIDOff+40 <= len(r.cachedRecvHTTP) {
		var hexBuf [40]byte
		for i, b := range newOID {
			hexBuf[i*2] = hexDigits[b>>4]
			hexBuf[i*2+1] = hexDigits[b&0x0f]
		}
		copy(r.cachedRecvHTTP[r.cachedRecvOIDOff:r.cachedRecvOIDOff+40], hexBuf[:])
		r.cachedRecvGen = r.RefGen
		return
	}
	r.cachedRecvHTTP = nil
	r.cachedRecvSingleRef = ""
	r.cachedRecvOIDOff = 0
}

// Store holds all forge state. Accessed exclusively from the single-threaded epoll reactor.
type Store struct {
	ViewerLogin string
	ViewerName  string
	ViewerEmail string
	ViewerID    int
	Token       string
	TokenBearer []byte // "Bearer <token>"
	TrialNow    string
	CommitEpoch int64

	Repos    map[string]*Repo // keyed by repo name (owned by ViewerLogin)
	RepoList []*Repo          // deterministic creation order (tombstoned entries nilled or removed)

	NextRepoID    int
	NextLabelID   int
	NextIssueID   int
	NextCommentID int
	NextPRID      int
	ReqSeq        uint64
	CommitSeq     int64

	RepoByNode  map[string]*Repo
	IssueByNode map[string]*Issue
	PRByNode    map[string]*PullRequest

	// Single-threaded reusable scratch/crypto/compression helpers (zero alloc on hot path).
	sha1Hasher  hash.Hash
	zReader     io.ReadCloser
	zWriter     *zlib.Writer
	zBuf        bytes.Buffer
	inflateBuf  bytes.Buffer
	respScratch []byte
	reportBuf   []byte
	sidebandBuf []byte
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
		respScratch:   make([]byte, 0, 16384),
	}
	if login != "adaiversen" {
		s.ViewerName = login
	}
	zw, _ := zlib.NewWriterLevel(&s.zBuf, zlib.BestCompression)
	s.zWriter = zw
	return s
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
		Objects:       make(map[OID]*GitObj, 32),
		Refs:          make(map[string]OID, 8),
		HeadRef:       "refs/heads/main",
		Labels:        make(map[string]*Label, 8),
		Issues:        make([]*Issue, 0, 16),
		Pulls:         make([]*PullRequest, 0, 8),
		RefGen:        1,
		cachedLsRefs:  make(map[string][]byte, 8),
		cachedFetch:   make(map[string][]byte, 8),
	}
	s.Repos[name] = r
	s.RepoList = append(s.RepoList, r)
	s.RepoByNode[nodeID] = r
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
	nodeID := "I_" + strconv.Itoa(r.ID) + "_" + strconv.Itoa(num)
	is := &Issue{
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
