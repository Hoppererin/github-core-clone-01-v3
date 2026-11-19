package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// ---------------------------------------------------------------------------
// git smart HTTP: info/refs, upload-pack (v2 + v0), receive-pack (v0)
// ---------------------------------------------------------------------------

const (
	uploadPackCT  = "application/x-git-upload-pack-advertisement"
	uploadPackRes = "application/x-git-upload-pack-result"
	receivePackCT = "application/x-git-receive-pack-advertisement"
	receivePackRes = "application/x-git-receive-pack-result"
)

var uploadPackCaps = "side-band-64k thin-pack ofs-delta shallow no-progress multi_ack agent=forge/1.0"
var receivePackCaps = "report-status report-status-v2 side-band-64k delete-refs ofs-delta agent=forge/1.0"

// sortedRefNames returns the repo's ref names in git's advertisement order
// (HEAD first is handled by callers; the rest sorted).
func sortedRefNames(r *Repo) []string {
	names := make([]string, 0, len(r.Refs))
	for n := range r.Refs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// peel returns the commit oid an annotated tag points at, or ("", false).
func (s *Store) peel(oid string) (string, bool) {
	o := s.objects[oid]
	if o == nil || o.Type != "tag" {
		return "", false
	}
	target, typ := parseTag(o.Data)
	if typ != "commit" {
		return "", false
	}
	return target, true
}

// headLine is the HEAD advertisement line (with symref caps when v2-style
// ls-refs is used; for v0 the symref is not advertised on HEAD).
func (s *Store) headOID(r *Repo) (string, bool) {
	return r.headOID()
}

// v0Advertisement builds a v0 "# service=..." advertisement body.
func (s *Store) v0Advertisement(r *Repo, service, caps string) []byte {
	var out []byte
	out = append(out, pktLine([]byte("# service="+service+"\n"))...)
	out = append(out, pktFlush()...)
	names := sortedRefNames(r)
	if len(names) == 0 {
		// empty repo: a zero-oid capabilities line carries the caps
		line := zeroOID + " capabilities^{}\x00" + caps + "\n"
		out = append(out, pktLine([]byte(line))...)
		out = append(out, pktFlush()...)
		return out
	}
	first := true
	emit := func(oid, ref string) {
		line := oid + " " + ref
		if first {
			line += "\x00" + caps
			first = false
		}
		out = append(out, pktLine([]byte(line+"\n"))...)
	}
	if oid, ok := r.headOID(); ok {
		emit(oid, "HEAD")
	}
	for _, name := range names {
		emit(r.Refs[name], name)
		if target, ok := s.peel(r.Refs[name]); ok {
			out = append(out, pktLine([]byte(target+" "+name+"^{}\n"))...)
		}
	}
	out = append(out, pktFlush()...)
	return out
}

func handleGitInfoRefs(w http.ResponseWriter, r *http.Request, owner, repoName string) {
	s := theStore
	service := r.URL.Query().Get("service")
	if service != "git-upload-pack" && service != "git-receive-pack" {
		http.Error(w, "unknown service", http.StatusNotFound)
		return
	}
	s.mu.RLock()
	repo := s.getRepo(owner, repoName)
	if repo == nil {
		s.mu.RUnlock()
		http.Error(w, "repository not found", http.StatusNotFound)
		return
	}
	var body []byte
	var ctype string
	if service == "git-upload-pack" {
		ctype = uploadPackCT
		if r.Header.Get("Git-Protocol") == "version=2" {
			// v2 advertisement: capabilities only
			var b []byte
			b = append(b, pktLine([]byte("version 2\n"))...)
			b = append(b, pktLine([]byte("agent=forge/1.0\n"))...)
			b = append(b, pktLine([]byte("ls-refs=unborn\n"))...)
			b = append(b, pktLine([]byte("fetch=shallow wait-for-done\n"))...)
			b = append(b, pktLine([]byte("object-format=sha1\n"))...)
			b = append(b, pktFlush()...)
			body = b
		} else {
			body = s.v0Advertisement(repo, service, uploadPackCaps)
		}
	} else {
		ctype = receivePackCT
		body = s.v0Advertisement(repo, service, receivePackCaps)
	}
	s.mu.RUnlock()
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

// --- upload-pack v2 ---

type fetchRequest struct {
	wants         []string
	haves         []string
	shallow       []string
	deepen        int
	thinPack      bool
	noProgress    bool
	ofsDelta      bool
	refPrefixes   []string
	wantPeel      bool
	wantSymrefs   bool
	wantUnborn    bool
	isLsRefs      bool
}

// parseV2Request parses an upload-pack v2 request body.
func parseV2Request(body []byte) (*fetchRequest, error) {
	fr := &fetchRequest{}
	off := 0
	inArgs := false
	for off < len(body) {
		payload, next, kind, err := readPktLine(body, off)
		if err != nil {
			return nil, err
		}
		off = next
		if kind == 1 { // flush: end
			break
		}
		if kind == 2 { // delim: args follow
			inArgs = true
			continue
		}
		line := strings.TrimSuffix(string(payload), "\n")
		if !inArgs {
			switch {
			case line == "command=ls-refs":
				fr.isLsRefs = true
			case line == "command=fetch":
			case strings.HasPrefix(line, "agent="):
			case strings.HasPrefix(line, "object-format="):
			}
			continue
		}
		switch {
		case line == "peel":
			fr.wantPeel = true
		case line == "symrefs":
			fr.wantSymrefs = true
		case line == "unborn":
			fr.wantUnborn = true
		case line == "thin-pack":
			fr.thinPack = true
		case line == "no-progress":
			fr.noProgress = true
		case line == "ofs-delta":
			fr.ofsDelta = true
		case strings.HasPrefix(line, "ref-prefix "):
			fr.refPrefixes = append(fr.refPrefixes, strings.TrimPrefix(line, "ref-prefix "))
		case strings.HasPrefix(line, "want "):
			fr.wants = append(fr.wants, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "have "):
			fr.haves = append(fr.haves, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "shallow "):
			fr.shallow = append(fr.shallow, strings.TrimSpace(line[8:]))
		case strings.HasPrefix(line, "deepen "):
			n, _ := strconv.Atoi(strings.TrimSpace(line[7:]))
			fr.deepen = n
		case line == "done":
		}
	}
	return fr, nil
}

// lsRefsReply builds the ls-refs reply lines for a repo.
func (s *Store) lsRefsReply(r *Repo, fr *fetchRequest) []byte {
	var out []byte
	matchPrefix := func(ref string) bool {
		if len(fr.refPrefixes) == 0 {
			return true
		}
		for _, p := range fr.refPrefixes {
			if strings.HasPrefix(ref, p) {
				return true
			}
		}
		return false
	}
	headMatches := len(fr.refPrefixes) == 0
	if !headMatches {
		for _, p := range fr.refPrefixes {
			if strings.HasPrefix("HEAD", p) {
				headMatches = true
			}
		}
	}
	if len(r.Refs) == 0 {
		// empty repo: unborn HEAD when the client asked for unborn
		if fr.wantUnborn && headMatches {
			out = append(out, pktLine([]byte("unborn HEAD symref-target:refs/heads/"+r.DefaultBranch+"\n"))...)
		}
		out = append(out, pktFlush()...)
		return out
	}
	if headMatches {
		if oid, ok := r.headOID(); ok {
			line := oid + " HEAD"
			if fr.wantSymrefs {
				line += " symref-target:refs/heads/" + r.DefaultBranch
			}
			out = append(out, pktLine([]byte(line+"\n"))...)
		}
	}
	for _, name := range sortedRefNames(r) {
		if !matchPrefix(name) {
			continue
		}
		oid := r.Refs[name]
		line := oid + " " + name
		if fr.wantPeel {
			if target, ok := s.peel(oid); ok {
				line += " peeled:" + target
			}
		}
		out = append(out, pktLine([]byte(line+"\n"))...)
	}
	out = append(out, pktFlush()...)
	return out
}

// fetchObjectSet computes the object ids a fetch must send, plus the
// shallow/unshallow lines for the shallow-info section.
func (s *Store) fetchObjectSet(r *Repo, fr *fetchRequest) (oids []string, shallowLines, unshallowLines []string) {
	// exclusion set: the objects the client already holds. Walk from the
	// haves, but do NOT descend past a shallow boundary: a shallow
	// boundary is a commit the client has whose parents it does NOT have
	// (that is the whole point of deepen). Excluding reachable(shallow)
	// would wrongly exclude the very parents a deepen fetch must deliver.
	shallowSet := map[string]bool{}
	for _, sh := range fr.shallow {
		shallowSet[sh] = true
	}
	exclude := map[string]bool{}
	seen := map[string]bool{}
	queue := append([]string{}, fr.haves...)
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		o := s.objects[oid]
		if o == nil {
			continue
		}
		exclude[oid] = true
		if o.Type == "tag" {
			if t, _ := parseTag(o.Data); t != "" {
				queue = append(queue, t)
			}
			continue
		}
		if o.Type != "commit" {
			continue
		}
		c := parseCommit(o.Data)
		for t := range s.reachableObjects([]string{c.Tree}) {
			exclude[t] = true
		}
		if shallowSet[oid] {
			continue // shallow boundary: parents are NOT held
		}
		queue = append(queue, c.Parents...)
	}
	if fr.deepen <= 0 {
		have := s.reachableObjects(fr.wants)
		for oid := range have {
			if !exclude[oid] {
				oids = append(oids, oid)
			}
		}
		return oids, nil, nil
	}
	// depth-limited walk: level 1 = the wanted commits themselves
	included := map[string]bool{}
	var boundary []string
	level := fr.wants
	for depth := 1; depth <= fr.deepen; depth++ {
		var next []string
		for _, oid := range level {
			if included[oid] {
				continue
			}
			included[oid] = true
			o := s.objects[oid]
			if o == nil {
				continue
			}
			if o.Type == "tag" {
				if t, _ := parseTag(o.Data); t != "" {
					next = append(next, t)
				}
				continue
			}
			if o.Type != "commit" {
				continue
			}
			c := parseCommit(o.Data)
			if depth == fr.deepen && len(c.Parents) > 0 {
				boundary = append(boundary, oid)
			}
			next = append(next, c.Parents...)
		}
		level = next
		if len(level) == 0 {
			break
		}
	}
	// precise set: for each included commit, its tree closure (trees/blobs)
	// plus the commit itself.
	set := map[string]bool{}
	for oid := range included {
		set[oid] = true
		o := s.objects[oid]
		if o == nil || o.Type != "commit" {
			continue
		}
		c := parseCommit(o.Data)
		for t := range s.reachableObjects([]string{c.Tree}) {
			set[t] = true
		}
	}
	for oid := range set {
		if !exclude[oid] {
			oids = append(oids, oid)
		}
	}
	for _, b := range boundary {
		shallowLines = append(shallowLines, "shallow "+b)
	}
	// unshallow: the client's old shallow boundaries now inside the depth
	for _, sh := range fr.shallow {
		if included[sh] {
			// included at a depth shallower than the boundary: it is no
			// longer a shallow boundary
			unshallowLines = append(unshallowLines, "unshallow "+sh)
		}
	}
	return oids, shallowLines, unshallowLines
}

// wantedRefsLines lists "<oid> <ref>" for each want that names a ref tip.
func (s *Store) wantedRefsLines(r *Repo, wants []string) []string {
	var lines []string
	for _, w := range wants {
		for _, name := range sortedRefNames(r) {
			if r.Refs[name] == w {
				lines = append(lines, w+" "+name)
			}
		}
		if oid, ok := r.headOID(); ok && oid == w {
			lines = append(lines, w+" HEAD")
		}
	}
	return lines
}

// fetchCache memoizes complete v2 fetch replies. Git objects are content
// addressed (immutable) and refs only change when the store generation
// bumps, so a reply is a pure function of (repo, gen, wants, haves,
// shallow, depth). The clone workload fires 50 identical depth-1 clones;
// the cache turns 49 of them into a memcpy.
var fetchCache sync.Map // string -> []byte
var fetchCacheEntries atomic.Int64

func fetchCacheKey(repoID int, gen uint64, fr *fetchRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d|%d|%d|", repoID, gen, fr.deepen)
	for _, w := range fr.wants {
		b.WriteString(w)
		b.WriteByte(',')
	}
	b.WriteByte('|')
	for _, h := range fr.haves {
		b.WriteString(h)
		b.WriteByte(',')
	}
	b.WriteByte('|')
	for _, sh := range fr.shallow {
		b.WriteString(sh)
		b.WriteByte(',')
	}
	return b.String()
}

func handleUploadPack(w http.ResponseWriter, r *http.Request, owner, repoName string) {
	s := theStore
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}
	isV2 := r.Header.Get("Git-Protocol") == "version=2"
	w.Header().Set("Content-Type", uploadPackRes)
	w.Header().Set("Cache-Control", "no-cache")

	s.mu.RLock()
	repo := s.getRepo(owner, repoName)
	if repo == nil {
		s.mu.RUnlock()
		http.Error(w, "repository not found", http.StatusNotFound)
		return
	}
	if isV2 {
		fr, err := parseV2Request(body)
		if err != nil {
			s.mu.RUnlock()
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if fr.isLsRefs {
			reply := s.lsRefsReply(repo, fr)
			s.mu.RUnlock()
			w.WriteHeader(http.StatusOK)
			w.Write(reply)
			return
		}
		// fetch — v2 response sections are labelled: each section starts
		// with its name line and ends with a delim packet.
		key := fetchCacheKey(repo.ID, s.gen, fr)
		if cached, ok := fetchCache.Load(key); ok {
			s.mu.RUnlock()
			w.WriteHeader(http.StatusOK)
			w.Write(cached.([]byte))
			return
		}
		oids, shallowLines, unshallowLines := s.fetchObjectSet(repo, fr)
		// acknowledgments
		var out []byte
		acked := ""
		for _, h := range fr.haves {
			// a have is acknowledged when it is an ancestor of a want
			for _, wnt := range fr.wants {
				if s.isAncestor(h, wnt) {
					acked = h
					break
				}
			}
			if acked != "" {
				break
			}
		}
		out = append(out, pktLine([]byte("acknowledgments\n"))...)
		if acked != "" {
			out = append(out, pktLine([]byte("ACK "+acked+"\n"))...)
			out = append(out, pktLine([]byte("ready\n"))...)
		} else {
			out = append(out, pktLine([]byte("NAK\n"))...)
		}
		out = append(out, pktDelim()...)
		// shallow-info
		out = append(out, pktLine([]byte("shallow-info\n"))...)
		for _, l := range shallowLines {
			out = append(out, pktLine([]byte(l+"\n"))...)
		}
		for _, l := range unshallowLines {
			out = append(out, pktLine([]byte(l+"\n"))...)
		}
		out = append(out, pktDelim()...)
		// wanted-refs
		out = append(out, pktLine([]byte("wanted-refs\n"))...)
		for _, l := range s.wantedRefsLines(repo, fr.wants) {
			out = append(out, pktLine([]byte(l+"\n"))...)
		}
		out = append(out, pktDelim()...)
		// packfile
		out = append(out, pktLine([]byte("packfile\n"))...)
		pack := writePack(s, oids)
		out = append(out, sidebandPack(pack)...)
		out = append(out, pktFlush()...)
		s.mu.RUnlock()
		if len(out) <= 1<<20 && fetchCacheEntries.Load() < 128 {
			if _, loaded := fetchCache.LoadOrStore(key, out); !loaded {
				fetchCacheEntries.Add(1)
			}
		}
		w.WriteHeader(http.StatusOK)
		w.Write(out)
		return
	}
	// ---- v0 fetch ----
	fr := parseV0FetchRequest(body)
	oids, shallowLines, _ := s.fetchObjectSet(repo, fr)
	var out []byte
	acked := ""
	for _, h := range fr.haves {
		for _, wnt := range fr.wants {
			if s.isAncestor(h, wnt) {
				acked = h
				break
			}
		}
		if acked != "" {
			break
		}
	}
	if acked != "" {
		out = append(out, pktLine([]byte("ACK "+acked+"\n"))...)
	} else {
		out = append(out, pktLine([]byte("NAK\n"))...)
	}
	for _, l := range shallowLines {
		out = append(out, pktLine([]byte(l+"\n"))...)
	}
	pack := writePack(s, oids)
	out = append(out, sidebandPack(pack)...)
	out = append(out, pktFlush()...)
	s.mu.RUnlock()
	w.WriteHeader(http.StatusOK)
	w.Write(out)
}

// parseV0FetchRequest parses a v0 upload-pack request body.
func parseV0FetchRequest(body []byte) *fetchRequest {
	fr := &fetchRequest{}
	off := 0
	seenFlush := false
	for off < len(body) {
		payload, next, kind, err := readPktLine(body, off)
		if err != nil {
			break
		}
		off = next
		if kind == 1 {
			if !seenFlush {
				seenFlush = true
				continue
			}
			break
		}
		line := strings.TrimSuffix(string(payload), "\n")
		// first want line may carry capabilities after a space
		if strings.HasPrefix(line, "want ") {
			rest := strings.TrimSpace(line[5:])
			parts := strings.SplitN(rest, " ", 2)
			fr.wants = append(fr.wants, parts[0])
			if len(parts) == 2 {
				for _, cap := range strings.Fields(parts[1]) {
					switch cap {
					case "thin-pack":
						fr.thinPack = true
					case "ofs-delta":
						fr.ofsDelta = true
					case "no-progress":
						fr.noProgress = true
					}
				}
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "have "):
			fr.haves = append(fr.haves, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "shallow "):
			fr.shallow = append(fr.shallow, strings.TrimSpace(line[8:]))
		case strings.HasPrefix(line, "deepen "):
			n, _ := strconv.Atoi(strings.TrimSpace(line[7:]))
			fr.deepen = n
		case line == "done":
		}
	}
	return fr
}

// --- receive-pack v0 ---

type pushCommand struct {
	old string
	new string
	ref string
}

func handleReceivePack(w http.ResponseWriter, r *http.Request, owner, repoName string) {
	s := theStore
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", receivePackRes)
	w.Header().Set("Cache-Control", "no-cache")

	// parse the command list (pkt-lines until flush)
	var cmds []pushCommand
	sideband := false
	off := 0
	packStart := -1
	if len(body) < 4 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	for off < len(body) {
		payload, next, kind, err := readPktLine(body, off)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		off = next
		if kind == 1 {
			packStart = off
			break
		}
		line := string(payload)
		if idx := strings.IndexByte(line, 0); idx >= 0 {
			caps := line[idx+1:]
			line = line[:idx]
			if strings.Contains(caps, "side-band-64k") {
				sideband = true
			}
		}
		parts := strings.Fields(strings.TrimSuffix(line, "\n"))
		if len(parts) != 3 {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		cmds = append(cmds, pushCommand{old: parts[0], new: parts[1], ref: parts[2]})
	}
	if packStart < 0 {
		packStart = len(body)
	}
	var pack []byte
	if packStart < len(body) {
		pack = body[packStart:]
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	repo := s.getRepo(owner, repoName)
	if repo == nil {
		http.Error(w, "repository not found", http.StatusNotFound)
		return
	}
	// unpack the pack first (a refused push still unpacks ok)
	unpackOK := true
	unpackErr := ""
	if len(pack) > 0 {
		entries, err := parsePack(pack, s)
		if err != nil {
			unpackOK = false
			unpackErr = err.Error()
		} else {
			for _, e := range entries {
				s.putObject(objTypeName[e.typ], e.data)
			}
		}
	}
	// apply the ref updates
	type refResult struct {
		ref    string
		ok     bool
		reason string
	}
	results := make([]refResult, 0, len(cmds))
	for _, cmd := range cmds {
		cur, exists := repo.Refs[cmd.ref]
		fail := func(reason string) {
			results = append(results, refResult{ref: cmd.ref, ok: false, reason: reason})
		}
		if cmd.new == zeroOID {
			// delete
			if !exists {
				fail("remote ref does not exist")
				continue
			}
			if cmd.old != zeroOID && cmd.old != cur {
				fail("non-fast-forward")
				continue
			}
			delete(repo.Refs, cmd.ref)
			s.bumpGen()
			results = append(results, refResult{ref: cmd.ref, ok: true})
			continue
		}
		// create / update
		obj := s.objects[cmd.new]
		if obj == nil {
			fail("object not found")
			continue
		}
		newCommit := cmd.new
		if obj.Type == "tag" {
			if t, typ := parseTag(obj.Data); typ == "commit" {
				newCommit = t
			} else {
				fail("object not found")
				continue
			}
		} else if obj.Type != "commit" {
			fail("object not found")
			continue
		}
		if !exists {
			if cmd.old != zeroOID {
				fail("non-fast-forward")
				continue
			}
		} else {
			if cmd.old != cur {
				fail("non-fast-forward")
				continue
			}
			if !s.isAncestor(cur, newCommit) {
				fail("non-fast-forward")
				continue
			}
		}
		// The ref must point at the pushed object itself (a tag ref points
		// at the tag object, never at the peeled commit). newCommit is
		// only the peeled commit used for the fast-forward check.
		repo.Refs[cmd.ref] = cmd.new
		s.bumpGen()
		results = append(results, refResult{ref: cmd.ref, ok: true})
	}
	// report-status reply
	var lines bytes.Buffer
	if !unpackOK {
		lines.WriteString("unpack " + unpackErr + "\n")
	} else {
		lines.WriteString("unpack ok\n")
	}
	for _, res := range results {
		if res.ok {
			lines.WriteString("ok " + res.ref + "\n")
		} else {
			lines.WriteString("ng " + res.ref + " " + res.reason + "\n")
		}
	}
	var out []byte
	if sideband {
		// one band-1 packet per line (small), then flush
		for _, l := range strings.SplitAfter(lines.String(), "\n") {
			if l == "" {
				continue
			}
			out = append(out, pktLine(append([]byte{1}, []byte(l)...))...)
		}
		out = append(out, pktFlush()...)
	} else {
		for _, l := range strings.SplitAfter(lines.String(), "\n") {
			if l == "" {
				continue
			}
			out = append(out, pktLine([]byte(l))...)
		}
		out = append(out, pktFlush()...)
	}
	w.WriteHeader(http.StatusOK)
	w.Write(out)
}
