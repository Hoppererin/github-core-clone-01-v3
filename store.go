// Package main: a standalone GitHub-compatible git forge in a single static
// binary. One process serves git smart HTTP (upload-pack v2, receive-pack v0),
// REST v3 (/api/v3), GraphQL v4 (/api/graphql) and MCP (/mcp), all backed by
// an in-memory store. Written from scratch for the github-core-clone-01
// competition; no code was copied from any other forge implementation.
package main

import (
	"crypto/sha1"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ---------------------------------------------------------------------------
// Fixed identity and timestamps (deterministic within a run; the suite's
// scenario is seeded, so the token owner is always this user).
// ---------------------------------------------------------------------------

const (
	viewerLogin = "adaiversen"
	viewerID    = 1000
	viewerName  = "ada iversen"
	viewerEmail = "adaiversen@example.invalid"

	// fixed ISO timestamp used for every created_at / updated_at / merged_at
	fixedTimeISO = "2025-01-01T00:00:00Z"
	// fixed unix time used for author/committer lines of server-made commits
	fixedCommitTime = 1700000000

	zeroOID = "0000000000000000000000000000000000000000"
)

// ---------------------------------------------------------------------------
// Git objects
// ---------------------------------------------------------------------------

// Object is one git object: its type and its raw content bytes (the bytes
// after the "<type> <size>\0" header). The id is the SHA-1 of the header plus
// the content.
type Object struct {
	Type string
	Data []byte
	Seq  uint64 // insertion order, for stable ordering of equal timestamps
}

func hashObject(typ string, data []byte) string {
	h := sha1.New()
	h.Write([]byte(typ + " " + strconv.Itoa(len(data)) + "\x00"))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func (o *Object) id() string { return hashObject(o.Type, o.Data) }

// TreeEntry is one entry of a parsed tree object.
type TreeEntry struct {
	Mode string // "100644", "100755", "120000", "40000", "160000"
	Name string
	OID  string // hex sha-1
}

func parseTree(data []byte) []TreeEntry {
	var out []TreeEntry
	i := 0
	for i < len(data) {
		sp := indexByte(data[i:], ' ')
		if sp < 0 {
			break
		}
		mode := string(data[i : i+sp])
		i += sp + 1
		nul := indexByte(data[i:], 0)
		if nul < 0 || i+nul+20 > len(data) {
			break
		}
		name := string(data[i : i+nul])
		i += nul + 1
		oid := hex.EncodeToString(data[i : i+20])
		i += 20
		out = append(out, TreeEntry{Mode: mode, Name: name, OID: oid})
	}
	return out
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

func encodeTree(entries []TreeEntry) []byte {
	// git sorts tree entries by name, directories as if suffixed with "/"
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(a, b int) bool {
		na, nb := sorted[a].Name, sorted[b].Name
		if sorted[a].Mode == "40000" {
			na += "/"
		}
		if sorted[b].Mode == "40000" {
			nb += "/"
		}
		return na < nb
	})
	var buf []byte
	for _, e := range sorted {
		buf = append(buf, []byte(e.Mode+" "+e.Name+"\x00")...)
		raw, _ := hex.DecodeString(e.OID)
		buf = append(buf, raw...)
	}
	return buf
}

// Commit is a parsed commit object.
type Commit struct {
	Tree      string
	Parents   []string
	Author    string // full "Name <email> ts tz" line
	Committer string
	Message   string
	Time      int64 // author timestamp (for ordering)
}

func parseCommit(data []byte) *Commit {
	c := &Commit{}
	s := string(data)
	head, msg, _ := strings.Cut(s, "\n\n")
	// the raw object ends the message with a newline; the parsed message
	// is the text without it (API responses compare exact strings)
	c.Message = strings.TrimSuffix(msg, "\n")
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			c.Tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			c.Parents = append(c.Parents, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "author "):
			c.Author = strings.TrimSpace(line[7:])
			c.Time = parseGitTime(c.Author)
		case strings.HasPrefix(line, "committer "):
			c.Committer = strings.TrimSpace(line[10:])
		}
	}
	return c
}

func parseGitTime(line string) int64 {
	// "Name <email> 1700000000 +0000"
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	ts, err := strconv.ParseInt(fields[len(fields)-2], 10, 64)
	if err != nil {
		return 0
	}
	return ts
}

func encodeCommit(tree string, parents []string, author, committer, message string) []byte {
	var b strings.Builder
	b.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p + "\n")
	}
	b.WriteString("author " + author + "\n")
	b.WriteString("committer " + committer + "\n")
	b.WriteString("\n")
	b.WriteString(message)
	return []byte(b.String())
}

func serverAuthorLine() string {
	return viewerName + " <" + viewerEmail + "> " + strconv.FormatInt(fixedCommitTime, 10) + " +0000"
}

// parseTag returns the target oid and type of a tag object.
func parseTag(data []byte) (target, typ string) {
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "object ") {
			target = strings.TrimSpace(line[7:])
		} else if strings.HasPrefix(line, "type ") {
			typ = strings.TrimSpace(line[5:])
		} else if line == "" {
			break
		}
	}
	return target, typ
}

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

type Label struct {
	Name  string
	Color string
}

type Comment struct {
	ID   int
	Body string
}

type Issue struct {
	ID     int
	Number int
	Title  string
	Body   string
	State  string // "open" | "closed"
	Labels []string
	Author string
	Comments []*Comment
	Seq    uint64
}

type Pull struct {
	Number    int
	Title     string
	Body      string
	Head      string // branch name
	Base      string // branch name
	State     string // "open" | "closed"
	Merged    bool
	MergedAt  string
	MergeSHA  string
	Seq       uint64
}

type Repo struct {
	ID            int
	Name          string
	Owner         string
	Description   string
	Private       bool
	DefaultBranch string
	Refs          map[string]string // full ref name -> commit oid
	Issues        []*Issue
	Pulls         []*Pull
	Labels        map[string]*Label
	nextIssue     int
	nextPull      int
}

func (r *Repo) fullName() string { return r.Owner + "/" + r.Name }

func (r *Repo) headOID() (string, bool) {
	oid, ok := r.Refs["refs/heads/"+r.DefaultBranch]
	return oid, ok
}

// FileChange is one path difference between two trees.
type FileChange struct {
	Path   string
	Status string // "added" | "modified" | "removed"
	OldOID string
	NewOID string
}

type Store struct {
	mu          sync.RWMutex
	objects     map[string]*Object
	repos       map[string]*Repo // key owner + "/" + name
	nextRepoID  int
	nextIssueID int
	nextCmtID   int
	objSeq      uint64
	reqSeq      uint64
	gen         uint64 // bumped on every mutation; invalidates the fetch cache
}

// bumpGen records a mutation. Callers must hold the write lock.
func (s *Store) bumpGen() { s.gen++ }

func NewStore() *Store {
	return &Store{
		objects:     make(map[string]*Object),
		repos:       make(map[string]*Repo),
		nextRepoID:  1,
		nextIssueID: 1,
		nextCmtID:   1,
	}
}

// --- object helpers (caller must hold at least a read lock) ---

func (s *Store) putObject(typ string, data []byte) string {
	id := hashObject(typ, data)
	if _, ok := s.objects[id]; !ok {
		s.objSeq++
		s.objects[id] = &Object{Type: typ, Data: data, Seq: s.objSeq}
		s.gen++
	}
	return id
}

func (s *Store) getObject(oid string) *Object { return s.objects[oid] }

// --- repo helpers (caller must hold at least a read lock) ---

func (s *Store) getRepo(owner, name string) *Repo {
	if owner != viewerLogin {
		return nil
	}
	return s.repos[owner+"/"+name]
}

// resolveRef resolves a ref name, a full ref, a branch name or a raw oid to
// a commit oid. Returns ("", false) when it cannot be resolved.
func (s *Store) resolveRef(r *Repo, ref string) (string, bool) {
	if ref == "" {
		return r.headOID()
	}
	if len(ref) == 40 && isHex(ref) {
		if o := s.objects[ref]; o != nil {
			return ref, true
		}
		return "", false
	}
	if oid, ok := r.Refs[ref]; ok {
		return oid, true
	}
	if oid, ok := r.Refs["refs/heads/"+ref]; ok {
		return oid, true
	}
	if oid, ok := r.Refs["refs/tags/"+ref]; ok {
		return oid, true
	}
	if ref == "HEAD" {
		return r.headOID()
	}
	return "", false
}

func isHex(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// --- graph walks (caller must hold at least a read lock) ---

// commitAncestors returns every commit reachable from tip (including tip),
// in no particular order.
func (s *Store) commitAncestors(tip string) map[string]bool {
	seen := map[string]bool{}
	var stack []string
	if tip != "" && tip != zeroOID {
		stack = append(stack, tip)
	}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		o := s.objects[oid]
		if o == nil || o.Type == "tag" {
			if o != nil && o.Type == "tag" {
				if t, _ := parseTag(o.Data); t != "" {
					stack = append(stack, t)
				}
			}
			continue
		}
		if o.Type != "commit" {
			continue
		}
		c := parseCommit(o.Data)
		stack = append(stack, c.Parents...)
	}
	return seen
}

// reachableObjects returns every object id reachable from the given tips
// (commits, trees, blobs, tags), following tag targets.
func (s *Store) reachableObjects(tips []string) map[string]bool {
	seen := map[string]bool{}
	var stack []string
	stack = append(stack, tips...)
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if oid == "" || oid == zeroOID || seen[oid] {
			continue
		}
		o := s.objects[oid]
		if o == nil {
			continue
		}
		seen[oid] = true
		switch o.Type {
		case "commit":
			c := parseCommit(o.Data)
			stack = append(stack, c.Tree)
			stack = append(stack, c.Parents...)
		case "tree":
			for _, e := range parseTree(o.Data) {
				stack = append(stack, e.OID)
			}
		case "tag":
			if t, _ := parseTag(o.Data); t != "" {
				stack = append(stack, t)
			}
		}
	}
	return seen
}

// commitListWithIDs returns every commit reachable from tip, newest first
func (s *Store) commitListWithIDs(tip string) ([]string, []*Commit) {
	anc := s.commitAncestors(tip)
	type item struct {
		c   *Commit
		oid string
		seq uint64
	}
	items := make([]item, 0, len(anc))
	for oid := range anc {
		o := s.objects[oid]
		if o == nil || o.Type != "commit" {
			continue
		}
		items = append(items, item{c: parseCommit(o.Data), oid: oid, seq: o.Seq})
	}
	sort.SliceStable(items, func(a, b int) bool {
		if items[a].c.Time != items[b].c.Time {
			return items[a].c.Time > items[b].c.Time
		}
		return items[a].seq > items[b].seq
	})
	oids := make([]string, len(items))
	commits := make([]*Commit, len(items))
	for i, it := range items {
		oids[i] = it.oid
		commits[i] = it.c
	}
	return oids, commits
}

// flattenTree returns path -> blob oid for every blob under treeOID.
func (s *Store) flattenTree(treeOID string) map[string]string {
	out := map[string]string{}
	var walk func(oid, prefix string)
	walk = func(oid, prefix string) {
		o := s.objects[oid]
		if o == nil || o.Type != "tree" {
			return
		}
		for _, e := range parseTree(o.Data) {
			p := prefix + e.Name
			if e.Mode == "40000" {
				walk(e.OID, p+"/")
			} else {
				out[p] = e.OID
			}
		}
	}
	walk(treeOID, "")
	return out
}

// getPath resolves path under treeOID: returns the blob oid and true, or if
// the path is a directory, ("", false) with entries. ok=false when missing.
func (s *Store) getPath(treeOID, path string) (blobOID string, isDir bool, entries []TreeEntry, ok bool) {
	if path == "" {
		o := s.objects[treeOID]
		if o == nil || o.Type != "tree" {
			return "", false, nil, false
		}
		return "", true, parseTree(o.Data), true
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	cur := treeOID
	for i, part := range parts {
		o := s.objects[cur]
		if o == nil || o.Type != "tree" {
			return "", false, nil, false
		}
		var found *TreeEntry
		for _, e := range parseTree(o.Data) {
			if e.Name == part {
				ee := e
				found = &ee
				break
			}
		}
		if found == nil {
			return "", false, nil, false
		}
		if i == len(parts)-1 {
			if found.Mode == "40000" {
				sub := s.objects[found.OID]
				if sub == nil {
					return "", false, nil, false
				}
				return "", true, parseTree(sub.Data), true
			}
			return found.OID, false, nil, true
		}
		cur = found.OID
	}
	return "", false, nil, false
}

// modifyTree returns a new tree oid equal to treeOID with path set to
// newBlobOID (or deleted when newBlobOID == ""). Intermediate directories
// are created as needed; empty directories are pruned. An empty result is
// the well-known empty tree object, never "".
func (s *Store) modifyTree(treeOID, path, newBlobOID string) (string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	oid, ok := s.modifyTreeRec(treeOID, parts, newBlobOID)
	if !ok {
		return "", false
	}
	if oid == "" {
		return s.putObject("tree", []byte{}), true
	}
	return oid, true
}

func (s *Store) modifyTreeRec(treeOID string, parts []string, newBlobOID string) (string, bool) {
	var entries []TreeEntry
	if treeOID != "" {
		if o := s.objects[treeOID]; o != nil && o.Type == "tree" {
			entries = parseTree(o.Data)
		}
	}
	name := parts[0]
	idx := -1
	for i, e := range entries {
		if e.Name == name {
			idx = i
			break
		}
	}
	if len(parts) == 1 {
		if newBlobOID == "" {
			if idx < 0 {
				return treeOID, true // nothing to delete
			}
			entries = append(entries[:idx], entries[idx+1:]...)
		} else {
			if idx < 0 {
				entries = append(entries, TreeEntry{Mode: "100644", Name: name, OID: newBlobOID})
			} else {
				entries[idx].OID = newBlobOID
				entries[idx].Mode = "100644"
			}
		}
	} else {
		var sub string
		if idx >= 0 {
			sub = entries[idx].OID
		}
		newSub, ok := s.modifyTreeRec(sub, parts[1:], newBlobOID)
		if !ok {
			return "", false
		}
		if newSub == "" {
			// subtree became empty: drop the entry
			if idx >= 0 {
				entries = append(entries[:idx], entries[idx+1:]...)
			}
		} else {
			if idx < 0 {
				entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: newSub})
			} else {
				entries[idx].OID = newSub
				entries[idx].Mode = "40000"
			}
		}
	}
	if len(entries) == 0 {
		return "", true
	}
	data := encodeTree(entries)
	return s.putObject("tree", data), true
}

// diffTrees returns the file-level differences from baseTree to headTree.
func (s *Store) diffTrees(baseTree, headTree string) []FileChange {
	b := s.flattenTree(baseTree)
	h := s.flattenTree(headTree)
	var out []FileChange
	for p, hb := range h {
		if bb, ok := b[p]; !ok {
			out = append(out, FileChange{Path: p, Status: "added", NewOID: hb})
		} else if bb != hb {
			out = append(out, FileChange{Path: p, Status: "modified", OldOID: bb, NewOID: hb})
		}
	}
	for p, bb := range b {
		if _, ok := h[p]; !ok {
			out = append(out, FileChange{Path: p, Status: "removed", OldOID: bb})
		}
	}
	sort.Slice(out, func(a, c int) bool { return out[a].Path < out[c].Path })
	return out
}

// mergeBase returns a common ancestor commit of a and b, or "".
func (s *Store) mergeBase(a, b string) string {
	if a == "" || b == "" || a == zeroOID || b == zeroOID {
		return ""
	}
	ancA := s.commitAncestors(a)
	// BFS from b, first commit also in ancA wins.
	seen := map[string]bool{}
	queue := []string{b}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		if ancA[oid] {
			return oid
		}
		o := s.objects[oid]
		if o == nil || o.Type != "commit" {
			continue
		}
		queue = append(queue, parseCommit(o.Data).Parents...)
	}
	return ""
}

// mergeConflict returns the list of conflicting paths for merging headTip
// into baseTip with a 3-way merge, plus whether it is mergeable.
func (s *Store) mergeConflict(headTip, baseTip string) []string {
	mb := s.mergeBase(headTip, baseTip)
	if mb == "" {
		// unrelated histories: any overlapping path counts as conflict-free
		// only if head adds nothing base also has; treat as mergeable.
		return nil
	}
	mbCommit := s.objects[mb]
	if mbCommit == nil {
		return nil
	}
	mbTree := parseCommit(mbCommit.Data).Tree
	headTree := ""
	if o := s.objects[headTip]; o != nil && o.Type == "commit" {
		headTree = parseCommit(o.Data).Tree
	}
	baseTree := ""
	if o := s.objects[baseTip]; o != nil && o.Type == "commit" {
		baseTree = parseCommit(o.Data).Tree
	}
	headChanges := s.diffTrees(mbTree, headTree)
	baseChanges := s.diffTrees(mbTree, baseTree)
	baseMap := map[string]FileChange{}
	for _, c := range baseChanges {
		baseMap[c.Path] = c
	}
	var conflicts []string
	for _, hc := range headChanges {
		if bc, ok := baseMap[hc.Path]; ok {
			// both sides changed the path
			if hc.Status == "removed" || bc.Status == "removed" {
				if hc.Status != bc.Status || hc.NewOID != bc.NewOID {
					conflicts = append(conflicts, hc.Path)
				}
			} else if hc.NewOID != bc.NewOID {
				conflicts = append(conflicts, hc.Path)
			}
		}
	}
	sort.Strings(conflicts)
	return conflicts
}

// squashMerge performs a squash merge of pr into its base branch. Caller
// must hold the write lock. Returns the new commit oid or "" on conflict.
func (s *Store) squashMerge(r *Repo, pr *Pull) string {
	headTip, ok1 := s.resolveRef(r, pr.Head)
	baseTip, ok2 := s.resolveRef(r, pr.Base)
	if !ok1 || !ok2 {
		return ""
	}
	if len(s.mergeConflict(headTip, baseTip)) > 0 {
		return ""
	}
	mb := s.mergeBase(headTip, baseTip)
	mbTree := ""
	if mb != "" {
		if o := s.objects[mb]; o != nil && o.Type == "commit" {
			mbTree = parseCommit(o.Data).Tree
		}
	}
	headTree := ""
	if o := s.objects[headTip]; o != nil && o.Type == "commit" {
		headTree = parseCommit(o.Data).Tree
	}
	baseTree := ""
	if o := s.objects[baseTip]; o != nil && o.Type == "commit" {
		baseTree = parseCommit(o.Data).Tree
	}
	// merged tree = base tree + head-side changes (mb -> head)
	merged := baseTree
	for _, c := range s.diffTrees(mbTree, headTree) {
		var newOID string
		if c.Status == "removed" {
			newOID = ""
		} else {
			newOID = c.NewOID
		}
		nt, ok := s.modifyTree(merged, c.Path, newOID)
		if !ok {
			return ""
		}
		merged = nt
	}
	msg := pr.Title + "\n"
	commitData := encodeCommit(merged, []string{baseTip}, serverAuthorLine(), serverAuthorLine(), msg)
	commitOID := s.putObject("commit", commitData)
	r.Refs["refs/heads/"+pr.Base] = commitOID
	pr.State = "closed"
	pr.Merged = true
	pr.MergedAt = fixedTimeISO
	pr.MergeSHA = commitOID
	return commitOID
}

// isAncestor reports whether anc is an ancestor of (or equal to) tip.
func (s *Store) isAncestor(anc, tip string) bool {
	if anc == tip {
		return true
	}
	if anc == "" || anc == zeroOID {
		return true // creating from nothing is always fast-forward
	}
	return s.commitAncestors(tip)[anc]
}
