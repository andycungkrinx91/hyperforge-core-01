package forge

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

func (s *Store) HashGitObject(typ ObjType, data []byte) OID {
	h := s.sha1Hasher
	h.Reset()
	var hdr [32]byte
	hb := hdr[:0]
	hb = append(hb, typ.Name()...)
	hb = append(hb, ' ')
	hb = strconv.AppendInt(hb, int64(len(data)), 10)
	hb = append(hb, 0)
	_, _ = h.Write(hb)
	_, _ = h.Write(data)
	var out OID
	h.Sum(out[:0])
	return out
}

func (s *Store) PutObject(r *Repo, typ ObjType, data []byte, preCompressed []byte) *GitObj {
	oid := s.HashGitObject(typ, data)
	if existing, ok := r.Objects[oid]; ok {
		if len(existing.ZlibRaw) == 0 && len(preCompressed) > 0 {
			existing.ZlibRaw = preCompressed
		}
		return existing
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	obj := &GitObj{
		Type:    typ,
		OID:     oid,
		Data:    cp,
		ZlibRaw: preCompressed,
	}
	if typ == ObjCommit {
		parseCommitInto(obj)
	} else if typ == ObjTag {
		parseTagInto(obj)
	}
	r.Objects[oid] = obj
	return obj
}

func parseCommitInto(obj *GitObj) {
	if obj.Parsed {
		return
	}
	obj.Parsed = true
	data := obj.Data
	sep := bytes.Index(data, []byte("\n\n"))
	hdr := data
	if sep >= 0 {
		hdr = data[:sep]
	}
	for len(hdr) > 0 {
		nl := bytes.IndexByte(hdr, '\n')
		var line []byte
		if nl < 0 {
			line = hdr
			hdr = nil
		} else {
			line = hdr[:nl]
			hdr = hdr[nl+1:]
		}
		if len(line) == 45 && line[0] == 't' && line[4] == ' ' && string(line[:5]) == "tree " {
			if oid, ok := ParseOIDBytes(line[5:]); ok {
				obj.TreeOID = oid
			}
		} else if len(line) == 47 && line[0] == 'p' && line[6] == ' ' && string(line[:7]) == "parent " {
			if oid, ok := ParseOIDBytes(line[7:]); ok {
				obj.Parents = append(obj.Parents, oid)
			}
		}
	}
}

func commitMessageOf(obj *GitObj) string {
	sep := bytes.Index(obj.Data, []byte("\n\n"))
	if sep < 0 {
		return ""
	}
	return strings.TrimRight(string(obj.Data[sep+2:]), "\n")
}

func parseTagInto(obj *GitObj) {
	if obj.Parsed {
		return
	}
	obj.Parsed = true
	data := obj.Data
	sep := bytes.Index(data, []byte("\n\n"))
	hdr := data
	if sep >= 0 {
		hdr = data[:sep]
	}
	for len(hdr) > 0 {
		nl := bytes.IndexByte(hdr, '\n')
		var line []byte
		if nl < 0 {
			line = hdr
			hdr = nil
		} else {
			line = hdr[:nl]
			hdr = hdr[nl+1:]
		}
		switch {
		case bytes.HasPrefix(line, []byte("object ")):
			if oid, ok := ParseOIDBytes(line[7:]); ok {
				obj.TagTarget = oid
			}
		case bytes.HasPrefix(line, []byte("type ")):
			obj.TagType = string(line[5:])
		case bytes.HasPrefix(line, []byte("tag ")):
			obj.TagName = string(line[4:])
		}
	}
}

type TreeEntry struct {
	Mode string
	Name string
	OID  OID
}

func ParseTree(data []byte) []TreeEntry {
	var out []TreeEntry
	pos := 0
	for pos < len(data) {
		sp := bytes.IndexByte(data[pos:], ' ')
		if sp < 0 {
			break
		}
		mode := string(data[pos : pos+sp])
		pos += sp + 1
		nul := bytes.IndexByte(data[pos:], 0)
		if nul < 0 || pos+nul+1+20 > len(data) {
			break
		}
		name := string(data[pos : pos+nul])
		pos += nul + 1
		var oid OID
		copy(oid[:], data[pos:pos+20])
		pos += 20
		out = append(out, TreeEntry{Mode: mode, Name: name, OID: oid})
	}
	return out
}

func gitTreeSortKey(e TreeEntry) string {
	if e.Mode == "40000" || e.Mode == "040000" {
		return e.Name + "/"
	}
	return e.Name
}

func BuildTreeBytes(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		return gitTreeSortKey(sorted[i]) < gitTreeSortKey(sorted[j])
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		mode := e.Mode
		if mode == "040000" {
			mode = "40000"
		}
		buf.WriteString(mode)
		buf.WriteByte(' ')
		buf.WriteString(e.Name)
		buf.WriteByte(0)
		buf.Write(e.OID[:])
	}
	return buf.Bytes()
}

func (s *Store) BuildCommitObj(r *Repo, treeOID OID, parents []OID, message string) *GitObj {
	s.CommitSeq++
	ts := s.CommitEpoch + s.CommitSeq
	ident := fmt.Sprintf("%s <%s> %d +0000", s.ViewerName, s.ViewerEmail, ts)
	var buf bytes.Buffer
	buf.WriteString("tree ")
	buf.WriteString(treeOID.Hex())
	buf.WriteByte('\n')
	for _, p := range parents {
		buf.WriteString("parent ")
		buf.WriteString(p.Hex())
		buf.WriteByte('\n')
	}
	buf.WriteString("author ")
	buf.WriteString(ident)
	buf.WriteString("\ncommitter ")
	buf.WriteString(ident)
	buf.WriteString("\n\n")
	buf.WriteString(message)
	return s.PutObject(r, ObjCommit, buf.Bytes(), nil)
}

// FlattenTree returns map[path]blobOID for every file in a tree recursively.
func FlattenTree(r *Repo, treeOID OID, prefix string, dst map[string]OID) {
	if treeOID == ZeroOID {
		return
	}
	obj, ok := r.Objects[treeOID]
	if !ok || obj.Type != ObjTree {
		return
	}
	for _, e := range ParseTree(obj.Data) {
		full := e.Name
		if prefix != "" {
			full = prefix + "/" + e.Name
		}
		if e.Mode == "40000" || e.Mode == "040000" {
			FlattenTree(r, e.OID, full, dst)
		} else {
			dst[full] = e.OID
		}
	}
}

type treeNode struct {
	files map[string]OID
	dirs  map[string]*treeNode
}

func newTreeNode() *treeNode {
	return &treeNode{
		files: make(map[string]OID),
		dirs:  make(map[string]*treeNode),
	}
}

func (s *Store) BuildTreeFromFileMap(r *Repo, files map[string]OID) OID {
	root := newTreeNode()
	for p, oid := range files {
		parts := strings.Split(p, "/")
		cur := root
		for i := 0; i < len(parts)-1; i++ {
			seg := parts[i]
			if seg == "" {
				continue
			}
			next, ok := cur.dirs[seg]
			if !ok {
				next = newTreeNode()
				cur.dirs[seg] = next
			}
			cur = next
		}
		cur.files[parts[len(parts)-1]] = oid
	}
	var build func(n *treeNode) OID
	build = func(n *treeNode) OID {
		entries := make([]TreeEntry, 0, len(n.files)+len(n.dirs))
		for name, oid := range n.files {
			entries = append(entries, TreeEntry{Mode: "100644", Name: name, OID: oid})
		}
		for name, child := range n.dirs {
			if len(child.files) == 0 && len(child.dirs) == 0 {
				continue
			}
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: build(child)})
		}
		obj := s.PutObject(r, ObjTree, BuildTreeBytes(entries), nil)
		return obj.OID
	}
	return build(root)
}

// ResolvePathInTree resolves a repo-relative path to (entry, isDir, ok).
func ResolvePathInTree(r *Repo, rootTree OID, relPath string) (TreeEntry, bool, bool) {
	relPath = strings.Trim(relPath, "/")
	if relPath == "" {
		return TreeEntry{Mode: "40000", Name: "", OID: rootTree}, true, rootTree != ZeroOID
	}
	parts := strings.Split(relPath, "/")
	curTree := rootTree
	for i, seg := range parts {
		obj, ok := r.Objects[curTree]
		if !ok || obj.Type != ObjTree {
			return TreeEntry{}, false, false
		}
		found := false
		var matched TreeEntry
		for _, e := range ParseTree(obj.Data) {
			if e.Name == seg {
				matched = e
				found = true
				break
			}
		}
		if !found {
			return TreeEntry{}, false, false
		}
		isDir := matched.Mode == "40000" || matched.Mode == "040000"
		if i == len(parts)-1 {
			return matched, isDir, true
		}
		if !isDir {
			return TreeEntry{}, false, false
		}
		curTree = matched.OID
	}
	return TreeEntry{}, false, false
}

// IsAncestor checks whether oldOID is an ancestor of newOID.
// O(1) fast path for single-parent linear push (100% of bench.push).
func IsAncestor(r *Repo, oldOID, newOID OID) bool {
	if oldOID == newOID {
		return true
	}
	obj, ok := r.Objects[newOID]
	if !ok || obj.Type != ObjCommit {
		return false
	}
	parseCommitInto(obj)
	if len(obj.Parents) == 1 && obj.Parents[0] == oldOID {
		return true
	}
	visited := make(map[OID]struct{}, 16)
	queue := append([]OID(nil), obj.Parents...)
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == oldOID {
			return true
		}
		if _, seen := visited[cur]; seen {
			continue
		}
		visited[cur] = struct{}{}
		cobj, ok := r.Objects[cur]
		if !ok || cobj.Type != ObjCommit {
			continue
		}
		parseCommitInto(cobj)
		queue = append(queue, cobj.Parents...)
	}
	return false
}

// FindMergeBase finds the nearest common ancestor commit of a and b.
func FindMergeBase(r *Repo, a, b OID) OID {
	ancA := make(map[OID]struct{}, 16)
	q := []OID{a}
	for len(q) > 0 {
		cur := q[0]
		q = q[1:]
		if _, seen := ancA[cur]; seen {
			continue
		}
		ancA[cur] = struct{}{}
		obj, ok := r.Objects[cur]
		if !ok || obj.Type != ObjCommit {
			continue
		}
		parseCommitInto(obj)
		q = append(q, obj.Parents...)
	}
	visitedB := make(map[OID]struct{}, 16)
	qb := []OID{b}
	for len(qb) > 0 {
		cur := qb[0]
		qb = qb[1:]
		if _, inA := ancA[cur]; inA {
			return cur
		}
		if _, seen := visitedB[cur]; seen {
			continue
		}
		visitedB[cur] = struct{}{}
		obj, ok := r.Objects[cur]
		if !ok || obj.Type != ObjCommit {
			continue
		}
		parseCommitInto(obj)
		qb = append(qb, obj.Parents...)
	}
	return ZeroOID
}

// ThreeWayMerge merges headCommit into baseCommit using their merge base.
// Returns (mergedFiles, conflict).
func ThreeWayMerge(r *Repo, baseCommit, headCommit OID) (map[string]OID, bool) {
	ancCommit := FindMergeBase(r, baseCommit, headCommit)
	ancFiles := make(map[string]OID)
	baseFiles := make(map[string]OID)
	headFiles := make(map[string]OID)

	if obj, ok := r.Objects[ancCommit]; ok && obj.Type == ObjCommit {
		parseCommitInto(obj)
		FlattenTree(r, obj.TreeOID, "", ancFiles)
	}
	if obj, ok := r.Objects[baseCommit]; ok && obj.Type == ObjCommit {
		parseCommitInto(obj)
		FlattenTree(r, obj.TreeOID, "", baseFiles)
	}
	if obj, ok := r.Objects[headCommit]; ok && obj.Type == ObjCommit {
		parseCommitInto(obj)
		FlattenTree(r, obj.TreeOID, "", headFiles)
	}

	allPaths := make(map[string]struct{}, len(baseFiles)+len(headFiles))
	for p := range ancFiles {
		allPaths[p] = struct{}{}
	}
	for p := range baseFiles {
		allPaths[p] = struct{}{}
	}
	for p := range headFiles {
		allPaths[p] = struct{}{}
	}

	merged := make(map[string]OID, len(allPaths))
	for p := range allPaths {
		aOID, inA := ancFiles[p]
		bOID, inB := baseFiles[p]
		hOID, inH := headFiles[p]

		if inB == inH && bOID == hOID {
			if inB {
				merged[p] = bOID
			}
			continue
		}
		if inB == inA && bOID == aOID {
			// Base unchanged, take head
			if inH {
				merged[p] = hOID
			}
			continue
		}
		if inH == inA && hOID == aOID {
			// Head unchanged, keep base
			if inB {
				merged[p] = bOID
			}
			continue
		}
		// Both sides changed differently -> conflict
		return nil, true
	}
	return merged, false
}

type DiffFile struct {
	Filename  string
	Status    string
	Additions int
	Deletions int
	Changes   int
	SHA       string
}

func countLines(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	n := bytes.Count(b, []byte{'\n'})
	if b[len(b)-1] != '\n' {
		n++
	}
	return n
}

// DiffCommits compares baseCommit's tree against headCommit's tree.
func DiffCommits(r *Repo, baseCommit, headCommit OID) []DiffFile {
	anc := FindMergeBase(r, baseCommit, headCommit)
	refCommit := baseCommit
	if anc != ZeroOID {
		refCommit = anc
	}
	baseFiles := make(map[string]OID)
	headFiles := make(map[string]OID)
	if obj, ok := r.Objects[refCommit]; ok && obj.Type == ObjCommit {
		parseCommitInto(obj)
		FlattenTree(r, obj.TreeOID, "", baseFiles)
	}
	if obj, ok := r.Objects[headCommit]; ok && obj.Type == ObjCommit {
		parseCommitInto(obj)
		FlattenTree(r, obj.TreeOID, "", headFiles)
	}
	all := make(map[string]struct{}, len(baseFiles)+len(headFiles))
	for p := range baseFiles {
		all[p] = struct{}{}
	}
	for p := range headFiles {
		all[p] = struct{}{}
	}
	paths := make([]string, 0, len(all))
	for p := range all {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var out []DiffFile
	for _, p := range paths {
		bOID, inB := baseFiles[p]
		hOID, inH := headFiles[p]
		if inB && inH && bOID == hOID {
			continue
		}
		switch {
		case !inB && inH:
			adds := 1
			if obj, ok := r.Objects[hOID]; ok {
				if l := countLines(obj.Data); l > 0 {
					adds = l
				}
			}
			out = append(out, DiffFile{
				Filename:  p,
				Status:    "added",
				Additions: adds,
				Deletions: 0,
				Changes:   adds,
				SHA:       hOID.Hex(),
			})
		case inB && !inH:
			dels := 1
			if obj, ok := r.Objects[bOID]; ok {
				if l := countLines(obj.Data); l > 0 {
					dels = l
				}
			}
			out = append(out, DiffFile{
				Filename:  p,
				Status:    "removed",
				Additions: 0,
				Deletions: dels,
				Changes:   dels,
				SHA:       bOID.Hex(),
			})
		default:
			adds, dels := 1, 1
			if bObj, okB := r.Objects[bOID]; okB {
				if hObj, okH := r.Objects[hOID]; okH {
					bLines := bytes.Split(bObj.Data, []byte{'\n'})
					hLines := bytes.Split(hObj.Data, []byte{'\n'})
					bSet := make(map[string]int, len(bLines))
					for _, l := range bLines {
						bSet[string(l)]++
					}
					a := 0
					for _, l := range hLines {
						if bSet[string(l)] > 0 {
							bSet[string(l)]--
						} else {
							a++
						}
					}
					d := 0
					for _, rem := range bSet {
						d += rem
					}
					if a == 0 && d == 0 {
						a = 1
					}
					adds, dels = a, d
				}
			}
			out = append(out, DiffFile{
				Filename:  p,
				Status:    "modified",
				Additions: adds,
				Deletions: dels,
				Changes:   adds + dels,
				SHA:       hOID.Hex(),
			})
		}
	}
	return out
}

// -------------------------------------------------------------------------
// Packfile Reader (zero-alloc pooled zlib + sha1 on hot path)
// -------------------------------------------------------------------------

type sliceByteReader struct {
	data []byte
	pos  int
}

func (r *sliceByteReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func (r *sliceByteReader) ReadByte() (byte, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	b := r.data[r.pos]
	r.pos++
	return b, nil
}

type rawPackEntry struct {
	offset     int
	typ        ObjType
	size       int
	baseOffset int
	baseOID    OID
	data       []byte
	zlibRaw    []byte
	resolved   *GitObj
}

// UnpackPackfile validates the packfile checksum, inflates all objects, resolves deltas,
// and stores all resulting objects in r.Objects.
func (s *Store) UnpackPackfile(r *Repo, pack []byte) error {
	if len(pack) == 0 {
		return nil
	}
	if len(pack) < 32 {
		return errors.New("pack too short")
	}
	if pack[0] != 'P' || pack[1] != 'A' || pack[2] != 'C' || pack[3] != 'K' {
		return errors.New("bad pack magic")
	}
	ver := binary.BigEndian.Uint32(pack[4:8])
	if ver != 2 && ver != 3 {
		return errors.New("unsupported pack version")
	}
	count := int(binary.BigEndian.Uint32(pack[8:12]))
	trailerStart := len(pack) - 20

	// Verify pack SHA-1 trailer
	h := s.sha1Hasher
	h.Reset()
	_, _ = h.Write(pack[:trailerStart])
	var expected [20]byte
	h.Sum(expected[:0])
	if !bytes.Equal(expected[:], pack[trailerStart:]) {
		return errors.New("pack checksum mismatch")
	}
	if count == 0 {
		if trailerStart != 12 {
			return errors.New("trailing bytes in empty pack")
		}
		return nil
	}

	var stackEntries [8]rawPackEntry
	var entries []rawPackEntry
	if count <= len(stackEntries) {
		entries = stackEntries[:count]
	} else {
		entries = make([]rawPackEntry, count)
	}
	var byOffset map[int]int
	hasDeltas := false

	pos := 12
	var br sliceByteReader
	for i := 0; i < count; i++ {
		if pos >= trailerStart {
			return errors.New("truncated pack")
		}
		entryOff := pos
		c := pack[pos]
		pos++
		typ := ObjType((c >> 4) & 7)
		size := int(c & 0x0f)
		shift := uint(4)
		for c&0x80 != 0 {
			if pos >= trailerStart {
				return errors.New("truncated object header")
			}
			c = pack[pos]
			pos++
			size |= int(c&0x7f) << shift
			shift += 7
		}

		var baseOff int
		var baseOID OID
		if typ == ObjOfsDelta {
			hasDeltas = true
			if pos >= trailerStart {
				return errors.New("truncated ofs-delta")
			}
			b := pack[pos]
			pos++
			neg := int(b & 0x7f)
			for b&0x80 != 0 {
				if pos >= trailerStart {
					return errors.New("truncated ofs-delta")
				}
				b = pack[pos]
				pos++
				neg = ((neg + 1) << 7) | int(b&0x7f)
			}
			baseOff = entryOff - neg
		} else if typ == ObjRefDelta {
			hasDeltas = true
			if pos+20 > trailerStart {
				return errors.New("truncated ref-delta")
			}
			copy(baseOID[:], pack[pos:pos+20])
			pos += 20
		} else if typ < ObjCommit || typ > ObjTag {
			return errors.New("invalid pack object type")
		}

		br.data = pack[:trailerStart]
		br.pos = pos
		var err error
		if s.zReader == nil {
			s.zReader, err = zlib.NewReader(&br)
		} else {
			err = s.zReader.(zlib.Resetter).Reset(&br, nil)
		}
		if err != nil {
			return fmt.Errorf("zlib init: %w", err)
		}
		s.inflateBuf.Reset()
		if size > 0 {
			s.inflateBuf.Grow(size)
		}
		if _, err := io.Copy(&s.inflateBuf, s.zReader); err != nil {
			return fmt.Errorf("zlib inflate: %w", err)
		}
		pos = br.pos
		inflated := s.inflateBuf.Bytes()
		if len(inflated) != size {
			return fmt.Errorf("inflated size mismatch: got %d want %d", len(inflated), size)
		}

		// Fast path: non-delta object — store immediately without copying compressed bytes
		// (ZlibRaw is populated lazily on fetch/clone so push pays zero extra copies).
		if !hasDeltas && typ >= ObjCommit && typ <= ObjTag {
			obj := s.PutObject(r, typ, inflated, nil)
			entries[i] = rawPackEntry{
				offset:   entryOff,
				typ:      typ,
				size:     size,
				resolved: obj,
			}
			continue
		}

		dataCopy := make([]byte, len(inflated))
		copy(dataCopy, inflated)
		entries[i] = rawPackEntry{
			offset:     entryOff,
			typ:        typ,
			size:       size,
			baseOffset: baseOff,
			baseOID:    baseOID,
			data:       dataCopy,
		}
	}

	if !hasDeltas {
		return nil
	}

	byOffset = make(map[int]int, count)
	for i := range entries {
		byOffset[entries[i].offset] = i
		if entries[i].resolved == nil && entries[i].typ >= ObjCommit && entries[i].typ <= ObjTag {
			entries[i].resolved = s.PutObject(r, entries[i].typ, entries[i].data, entries[i].zlibRaw)
		}
	}

	var resolve func(idx int, depth int) (*GitObj, error)
	resolve = func(idx int, depth int) (*GitObj, error) {
		if depth > 64 {
			return nil, errors.New("delta chain too deep")
		}
		e := &entries[idx]
		if e.resolved != nil {
			return e.resolved, nil
		}
		var base *GitObj
		if e.typ == ObjOfsDelta {
			bidx, ok := byOffset[e.baseOffset]
			if !ok {
				return nil, errors.New("missing ofs-delta base")
			}
			var err error
			base, err = resolve(bidx, depth+1)
			if err != nil {
				return nil, err
			}
		} else if e.typ == ObjRefDelta {
			if obj, ok := r.Objects[e.baseOID]; ok {
				base = obj
			} else {
				return nil, errors.New("missing ref-delta base")
			}
		}
		out, err := applyGitDelta(base.Data, e.data)
		if err != nil {
			return nil, err
		}
		e.resolved = s.PutObject(r, base.Type, out, nil)
		return e.resolved, nil
	}

	for i := range entries {
		if entries[i].resolved == nil {
			if _, err := resolve(i, 0); err != nil {
				return err
			}
		}
	}
	return nil
}

func readDeltaVarint(b []byte, pos int) (int, int, error) {
	val := 0
	shift := uint(0)
	for {
		if pos >= len(b) {
			return 0, pos, errors.New("truncated delta varint")
		}
		c := b[pos]
		pos++
		val |= int(c&0x7f) << shift
		if c&0x80 == 0 {
			return val, pos, nil
		}
		shift += 7
	}
}

func applyGitDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	srcSize, pos, err := readDeltaVarint(delta, pos)
	if err != nil {
		return nil, err
	}
	if srcSize != len(base) {
		return nil, fmt.Errorf("delta src size %d != base %d", srcSize, len(base))
	}
	dstSize, pos, err := readDeltaVarint(delta, pos)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, dstSize)
	for pos < len(delta) {
		cmd := delta[pos]
		pos++
		if cmd&0x80 != 0 {
			cpOff := 0
			cpSize := 0
			if cmd&0x01 != 0 {
				cpOff |= int(delta[pos])
				pos++
			}
			if cmd&0x02 != 0 {
				cpOff |= int(delta[pos]) << 8
				pos++
			}
			if cmd&0x04 != 0 {
				cpOff |= int(delta[pos]) << 16
				pos++
			}
			if cmd&0x08 != 0 {
				cpOff |= int(delta[pos]) << 24
				pos++
			}
			if cmd&0x10 != 0 {
				cpSize |= int(delta[pos])
				pos++
			}
			if cmd&0x20 != 0 {
				cpSize |= int(delta[pos]) << 8
				pos++
			}
			if cmd&0x40 != 0 {
				cpSize |= int(delta[pos]) << 16
				pos++
			}
			if cpSize == 0 {
				cpSize = 0x10000
			}
			if cpOff+cpSize > len(base) {
				return nil, errors.New("delta copy out of bounds")
			}
			out = append(out, base[cpOff:cpOff+cpSize]...)
		} else if cmd > 0 {
			n := int(cmd)
			if pos+n > len(delta) {
				return nil, errors.New("delta insert out of bounds")
			}
			out = append(out, delta[pos:pos+n]...)
			pos += n
		} else {
			return nil, errors.New("invalid delta opcode 0")
		}
	}
	if len(out) != dstSize {
		return nil, fmt.Errorf("delta dst size %d != expected %d", len(out), dstSize)
	}
	return out, nil
}

// -------------------------------------------------------------------------
// Packfile Writer (reuses cached obj.ZlibRaw for zero-recompression clones!)
// -------------------------------------------------------------------------

func appendPackObjHeader(dst []byte, typ ObjType, size int) []byte {
	first := byte(typ<<4) | byte(size&0x0f)
	rem := size >> 4
	for rem > 0 {
		dst = append(dst, first|0x80)
		first = byte(rem & 0x7f)
		rem >>= 7
	}
	return append(dst, first)
}

func (s *Store) compressObj(obj *GitObj) []byte {
	if len(obj.ZlibRaw) > 0 {
		return obj.ZlibRaw
	}
	s.zBuf.Reset()
	s.zWriter.Reset(&s.zBuf)
	_, _ = s.zWriter.Write(obj.Data)
	_ = s.zWriter.Close()
	out := make([]byte, s.zBuf.Len())
	copy(out, s.zBuf.Bytes())
	obj.ZlibRaw = out
	return out
}

func (s *Store) BuildPack(objs []*GitObj) []byte {
	est := 32 + len(objs)*64
	for _, o := range objs {
		if len(o.ZlibRaw) > 0 {
			est += len(o.ZlibRaw)
		} else {
			est += len(o.Data)
		}
	}
	buf := make([]byte, 12, est)
	buf[0], buf[1], buf[2], buf[3] = 'P', 'A', 'C', 'K'
	binary.BigEndian.PutUint32(buf[4:8], 2)
	binary.BigEndian.PutUint32(buf[8:12], uint32(len(objs)))

	for _, o := range objs {
		buf = appendPackObjHeader(buf, o.Type, len(o.Data))
		buf = append(buf, s.compressObj(o)...)
	}
	sum := sha1.Sum(buf)
	buf = append(buf, sum[:]...)
	return buf
}
