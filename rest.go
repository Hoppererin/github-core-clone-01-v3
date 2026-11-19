package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
)

// request id counter (atomic: it is used on error paths while the store
// lock is already held, so it must never take the store lock).
var reqCounter atomic.Uint64

// ---------------------------------------------------------------------------
// REST v3 (/api/v3)
// ---------------------------------------------------------------------------

// restError writes a JSON error body.
func restError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"message": msg})
}

// writeJSON writes v as JSON with the standard REST headers. The
// X-GitHub-Request-Id header is present on every reply.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-GitHub-Request-Id", nextRequestID())
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func nextRequestID() string {
	n := reqCounter.Add(1)
	return fmt.Sprintf("req-%012x", n)
}

// repoJSON is the wire shape of a repository.
type repoJSON struct {
	ID            int    `json:"id"`
	NodeID        string `json:"node_id"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Owner         userJSON `json:"owner"`
	Private       bool   `json:"private"`
	Description   string `json:"description"`
	DefaultBranch string `json:"default_branch"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
	PushedAt      string `json:"pushed_at"`
	HTMLURL       string `json:"html_url"`
	URL           string `json:"url"`
	CloneURL      string `json:"clone_url"`
	GitURL        string `json:"git_url"`
	SSHURL        string `json:"ssh_url"`
	HasIssues     bool   `json:"has_issues"`
	HasWiki       bool   `json:"has_wiki"`
	Fork          bool   `json:"fork"`
	Archived      bool   `json:"archived"`
	Disabled      bool   `json:"disabled"`
	Visibility    string `json:"visibility"`
}

type userJSON struct {
	Login     string `json:"login"`
	ID        int    `json:"id"`
	NodeID    string `json:"node_id"`
	Type      string `json:"type"`
	SiteAdmin bool   `json:"site_admin"`
	HTMLURL   string `json:"html_url"`
	URL       string `json:"url"`
}

func viewerUserJSON() userJSON {
	return userJSON{
		Login: viewerLogin, ID: viewerID, NodeID: globalID("User", viewerID),
		Type: "User", HTMLURL: "https://github.com/" + viewerLogin,
		URL: "https://api.github.com/users/" + viewerLogin,
	}
}

func (r *Repo) toJSON() repoJSON {
	vis := "public"
	if r.Private {
		vis = "private"
	}
	return repoJSON{
		ID: r.ID, NodeID: globalID("Repository", r.ID), Name: r.Name,
		FullName: r.fullName(), Owner: viewerUserJSON(), Private: r.Private,
		Description: r.Description, DefaultBranch: r.DefaultBranch,
		CreatedAt: fixedTimeISO, UpdatedAt: fixedTimeISO, PushedAt: fixedTimeISO,
		HTMLURL: "https://github.com/" + r.fullName(),
		URL:     "https://api.github.com/repos/" + r.fullName(),
		CloneURL: "https://github.com/" + r.fullName() + ".git",
		GitURL:   "git://github.com/" + r.fullName() + ".git",
		SSHURL:   "git@github.com:" + r.fullName() + ".git",
		HasIssues: true, HasWiki: true, Visibility: vis,
	}
}

// issueJSON is the wire shape of an issue.
type issueJSON struct {
	ID        int         `json:"id"`
	NodeID    string      `json:"node_id"`
	Number    int         `json:"number"`
	Title     string      `json:"title"`
	Body      string      `json:"body"`
	State     string      `json:"state"`
	Locked    bool        `json:"locked"`
	User      userJSON    `json:"user"`
	Labels    []labelJSON `json:"labels"`
	Comments  int         `json:"comments"`
	CreatedAt string      `json:"created_at"`
	UpdatedAt string      `json:"updated_at"`
	HTMLURL   string      `json:"html_url"`
	URL       string      `json:"url"`
}

type labelJSON struct {
	ID          int    `json:"id"`
	NodeID      string `json:"node_id"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
	Default     bool   `json:"default"`
	URL         string `json:"url"`
}

func (s *Store) issueToJSON(r *Repo, iss *Issue) issueJSON {
	labels := make([]labelJSON, 0, len(iss.Labels))
	for _, name := range iss.Labels {
		if l, ok := r.Labels[name]; ok {
			labels = append(labels, labelJSON{
				ID: s.labelID(r, name), NodeID: globalID("Label", s.labelID(r, name)),
				Name: l.Name, Color: l.Color, Description: "", Default: false,
				URL: "https://api.github.com/repos/" + r.fullName() + "/labels/" + name,
			})
		}
	}
	return issueJSON{
		ID: iss.ID, NodeID: gqlIssueID(r.ID, iss.Number), Number: iss.Number,
		Title: iss.Title, Body: iss.Body, State: iss.State,
		User: viewerUserJSON(), Labels: labels, Comments: len(iss.Comments),
		CreatedAt: fixedTimeISO, UpdatedAt: fixedTimeISO,
		HTMLURL: "https://github.com/" + r.fullName() + "/issues/" + strconv.Itoa(iss.Number),
		URL:     "https://api.github.com/repos/" + r.fullName() + "/issues/" + strconv.Itoa(iss.Number),
	}
}

// labelID derives a stable small integer id for a label (per repo).
func (s *Store) labelID(r *Repo, name string) int {
	names := make([]string, 0, len(r.Labels))
	for n := range r.Labels {
		names = append(names, n)
	}
	sort.Strings(names)
	for i, n := range names {
		if n == name {
			return 1000 + i
		}
	}
	return 1000
}

// pullJSON is the wire shape of a pull request.
type pullJSON struct {
	ID            int      `json:"id"`
	NodeID        string   `json:"node_id"`
	Number        int      `json:"number"`
	Title         string   `json:"title"`
	Body          string   `json:"body"`
	State         string   `json:"state"`
	Locked        bool     `json:"locked"`
	User          userJSON `json:"user"`
	Head          refJSON  `json:"head"`
	Base          refJSON  `json:"base"`
	Merged        bool     `json:"merged"`
	Mergeable     *bool    `json:"mergeable"`
	MergedAt      *string  `json:"merged_at"`
	MergeCommitSHA *string `json:"merge_commit_sha"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
	HTMLURL       string   `json:"html_url"`
	URL           string   `json:"url"`
}

type refJSON struct {
	Label string `json:"label"`
	Ref   string `json:"ref"`
	SHA   string `json:"sha"`
	User  userJSON `json:"user"`
	Repo  *repoJSON `json:"repo"`
}

func (s *Store) pullToJSON(r *Repo, pr *Pull) pullJSON {
	headSHA, _ := s.resolveRef(r, pr.Head)
	baseSHA, _ := s.resolveRef(r, pr.Base)
	mergeable := len(s.mergeConflict(headSHA, baseSHA)) == 0
	repoJ := r.toJSON()
	out := pullJSON{
		ID: s.pullID(r, pr.Number), NodeID: gqlPullID(r.ID, pr.Number),
		Number: pr.Number, Title: pr.Title, Body: pr.Body, State: pr.State,
		User: viewerUserJSON(),
		Head: refJSON{Label: viewerLogin + ":" + pr.Head, Ref: pr.Head, SHA: headSHA, User: viewerUserJSON(), Repo: &repoJ},
		Base: refJSON{Label: viewerLogin + ":" + pr.Base, Ref: pr.Base, SHA: baseSHA, User: viewerUserJSON(), Repo: &repoJ},
		Merged: pr.Merged, Mergeable: &mergeable,
		CreatedAt: fixedTimeISO, UpdatedAt: fixedTimeISO,
		HTMLURL: "https://github.com/" + r.fullName() + "/pull/" + strconv.Itoa(pr.Number),
		URL:     "https://api.github.com/repos/" + r.fullName() + "/pulls/" + strconv.Itoa(pr.Number),
	}
	if pr.Merged {
		out.MergedAt = &pr.MergedAt
		out.MergeCommitSHA = &pr.MergeSHA
	}
	return out
}

func (s *Store) pullID(r *Repo, number int) int { return r.ID*100000 + number }

// globalID builds an opaque Relay id: base64("Type:key"), never the plain number.
func globalID(typ string, key any) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%v", typ, key)))
}

// --- pagination helpers ---

type pageParams struct {
	page    int
	perPage int
}

func parsePage(q map[string][]string) pageParams {
	p := pageParams{page: 1, perPage: 30}
	if v := firstQuery(q, "page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			p.page = n
		}
	}
	if v := firstQuery(q, "per_page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			if n > 100 {
				n = 100
			}
			p.perPage = n
		}
	}
	return p
}

func firstQuery(q map[string][]string, key string) string {
	if vs, ok := q[key]; ok && len(vs) > 0 {
		return vs[0]
	}
	return ""
}

// linkHeader builds a GitHub-style Link header for paginated lists.
func linkHeader(base string, p pageParams, total int) string {
	last := (total + p.perPage - 1) / p.perPage
	if last < 1 {
		last = 1
	}
	var parts []string
	if p.page < last {
		parts = append(parts, fmt.Sprintf("<%s?page=%d&per_page=%d>; rel=\"next\"", base, p.page+1, p.perPage))
	}
	if p.page > 1 {
		parts = append(parts, fmt.Sprintf("<%s?page=%d&per_page=%d>; rel=\"prev\"", base, p.page-1, p.perPage))
	}
	parts = append(parts, fmt.Sprintf("<%s?page=1&per_page=%d>; rel=\"first\"", base, p.perPage))
	parts = append(parts, fmt.Sprintf("<%s?page=%d&per_page=%d>; rel=\"last\"", base, last, p.perPage))
	return strings.Join(parts, ", ")
}

func paginate[T any](items []T, p pageParams) []T {
	start := (p.page - 1) * p.perPage
	if start >= len(items) {
		return []T{}
	}
	end := start + p.perPage
	if end > len(items) {
		end = len(items)
	}
	return items[start:end]
}

// --- handlers ---

func handleRestRoot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-OAuth-Scopes", "repo, user")
	writeJSON(w, http.StatusOK, map[string]any{
		"current_user_url":                 "https://api.github.com/user",
		"current_user_authorizations_html_url": "https://github.com/settings/applications",
		"authorizations_url":               "https://api.github.com/authorizations",
		"code_search_url":                  "https://api.github.com/search/code?q={query}",
		"emails_url":                       "https://api.github.com/user/emails",
		"emojis_url":                       "https://api.github.com/emojis",
		"events_url":                       "https://api.github.com/events",
		"feeds_url":                        "https://api.github.com/feeds",
		"following_url":                    "https://api.github.com/user/following{/other_user}",
		"gists_url":                        "https://api.github.com/gists{/gist_id}",
		"hub_url":                          "https://api.github.com/hub",
		"issue_search_url":                 "https://api.github.com/search/issues?q={query}",
		"issues_url":                       "https://api.github.com/issues",
		"keys_url":                         "https://api.github.com/user/keys",
		"notifications_url":                "https://api.github.com/notifications",
		"organization_repositories_url":    "https://api.github.com/orgs/{org}/repos",
		"organization_url":                 "https://api.github.com/orgs/{org}",
		"public_gists_url":                 "https://api.github.com/gists/public",
		"rate_limit_url":                   "https://api.github.com/rate_limit",
		"repository_url":                   "https://api.github.com/repos/{owner}/{repo}",
		"repository_search_url":            "https://api.github.com/search/repositories?q={query}",
		"current_user_repositories_url":    "https://api.github.com/user/repos",
		"user_url":                         "https://api.github.com/users/{user}",
	})
}

func handleRestMeta(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"installed_version": "1.0.0"})
}

func handleRestUser(w http.ResponseWriter, r *http.Request) {
	u := viewerUserJSON()
	writeJSON(w, http.StatusOK, map[string]any{
		"login": u.Login, "id": u.ID, "node_id": u.NodeID, "type": "User",
		"name": viewerName, "email": viewerEmail,
		"html_url": u.HTMLURL, "url": u.URL,
		"site_admin": false, "hireable": nil, "bio": nil,
		"public_repos": 0, "followers": 0, "following": 0,
		"created_at": fixedTimeISO, "updated_at": fixedTimeISO,
	})
}

func decodeBody(r *http.Request, v any) bool {
	if r.Body == nil {
		return false
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		return false
	}
	return true
}

// --- /user/repos ---

func handleUserRepos(w http.ResponseWriter, r *http.Request, baseURL string) {
	s := theStore
	if r.Method == http.MethodGet {
		p := parsePage(r.URL.Query())
		s.mu.RLock()
		var repos []*Repo
		for _, repo := range s.repos {
			repos = append(repos, repo)
		}
		sort.Slice(repos, func(a, b int) bool { return repos[a].ID < repos[b].ID })
		total := len(repos)
		page := paginate(repos, p)
		out := make([]repoJSON, 0, len(page))
		for _, repo := range page {
			out = append(out, repo.toJSON())
		}
		s.mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-GitHub-Request-Id", nextRequestID())
		w.Header().Set("Link", linkHeader(baseURL+"/api/v3/user/repos", p, total))
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(out)
		return
	}
	// POST: create
	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Private     bool   `json:"private"`
	}
	if !decodeBody(r, &body) || body.Name == "" {
		restError(w, http.StatusBadRequest, "Bad Request")
		return
	}
	s.mu.Lock()
	if _, exists := s.repos[viewerLogin+"/"+body.Name]; exists {
		s.mu.Unlock()
		restError(w, http.StatusUnprocessableEntity, "name already exists on this account")
		return
	}
	repo := &Repo{
		ID: s.nextRepoID, Name: body.Name, Owner: viewerLogin,
		Description: body.Description, Private: body.Private,
		DefaultBranch: "main", Refs: map[string]string{},
		Labels: map[string]*Label{},
		nextIssue: 1, nextPull: 1,
	}
	s.nextRepoID++
	s.repos[viewerLogin+"/"+body.Name] = repo
	out := repo.toJSON()
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, out)
}

// --- /repos/{owner}/{repo}... ---

func repoFromPath(w http.ResponseWriter, owner, name string) *Repo {
	s := theStore
	s.mu.RLock()
	defer s.mu.RUnlock()
	repo := s.getRepo(owner, name)
	if repo == nil {
		restError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return repo
}

func handleRepo(w http.ResponseWriter, r *http.Request, owner, name string) {
	s := theStore
	switch r.Method {
	case http.MethodGet:
		repo := repoFromPath(w, owner, name)
		if repo == nil {
			return
		}
		writeJSON(w, http.StatusOK, repo.toJSON())
	case http.MethodPatch:
		var body struct {
			Description *string `json:"description"`
			Private     *bool   `json:"private"`
			DefaultBranch *string `json:"default_branch"`
		}
		if !decodeBody(r, &body) {
			restError(w, http.StatusBadRequest, "Bad Request")
			return
		}
		s.mu.Lock()
		repo := s.getRepo(owner, name)
		if repo == nil {
			s.mu.Unlock()
			restError(w, http.StatusNotFound, "Not Found")
			return
		}
		if body.Description != nil {
			repo.Description = *body.Description
		}
		if body.Private != nil {
			repo.Private = *body.Private
		}
		if body.DefaultBranch != nil && *body.DefaultBranch != "" {
			repo.DefaultBranch = *body.DefaultBranch
		}
		out := repo.toJSON()
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, out)
	case http.MethodDelete:
		s.mu.Lock()
		if s.getRepo(owner, name) == nil {
			s.mu.Unlock()
			restError(w, http.StatusNotFound, "Not Found")
			return
		}
		delete(s.repos, owner+"/"+name)
		s.mu.Unlock()
		w.Header().Set("X-GitHub-Request-Id", nextRequestID())
		w.WriteHeader(http.StatusNoContent)
	default:
		restError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
	}
}

// --- branches ---

func handleBranches(w http.ResponseWriter, r *http.Request, owner, name string) {
	repo := repoFromPath(w, owner, name)
	if repo == nil {
		return
	}
	s := theStore
	s.mu.RLock()
	defer s.mu.RUnlock()
	type branchJSON struct {
		Name      string `json:"name"`
		Commit    struct {
			SHA string `json:"sha"`
			URL string `json:"url"`
		} `json:"commit"`
		Protected bool `json:"protected"`
	}
	var out []branchJSON
	for _, ref := range sortedRefNames(repo) {
		if !strings.HasPrefix(ref, "refs/heads/") {
			continue
		}
		b := branchJSON{Name: strings.TrimPrefix(ref, "refs/heads/")}
		b.Commit.SHA = repo.Refs[ref]
		b.Commit.URL = "https://api.github.com/repos/" + repo.fullName() + "/commits/" + b.Commit.SHA
		out = append(out, b)
	}
	if out == nil {
		out = []branchJSON{}
	}
	writeJSON(w, http.StatusOK, out)
}

func handleBranch(w http.ResponseWriter, r *http.Request, owner, name, branch string) {
	repo := repoFromPath(w, owner, name)
	if repo == nil {
		return
	}
	s := theStore
	s.mu.RLock()
	defer s.mu.RUnlock()
	oid, ok := repo.Refs["refs/heads/"+branch]
	if !ok {
		restError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name": branch,
		"commit": map[string]string{
			"sha": oid,
			"url": "https://api.github.com/repos/" + repo.fullName() + "/commits/" + oid,
		},
		"protected": false,
	})
}

// --- git refs ---

func handleGitRefs(w http.ResponseWriter, r *http.Request, owner, name, refPath string) {
	s := theStore
	if r.Method == http.MethodPost && refPath == "" {
		// create a ref
		var body struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		}
		if !decodeBody(r, &body) || body.Ref == "" || body.SHA == "" {
			restError(w, http.StatusBadRequest, "Bad Request")
			return
		}
		s.mu.Lock()
		repo := s.getRepo(owner, name)
		if repo == nil {
			s.mu.Unlock()
			restError(w, http.StatusNotFound, "Not Found")
			return
		}
		if _, exists := repo.Refs[body.Ref]; exists {
			s.mu.Unlock()
			restError(w, http.StatusUnprocessableEntity, "Reference already exists")
			return
		}
		obj := s.objects[body.SHA]
		if obj == nil {
			s.mu.Unlock()
			restError(w, http.StatusUnprocessableEntity, "Object does not exist")
			return
		}
		sha := body.SHA
		if obj.Type == "tag" {
			if t, typ := parseTag(obj.Data); typ == "commit" {
				sha = t
			}
		}
		repo.Refs[body.Ref] = sha
	s.bumpGen()
		s.mu.Unlock()
		writeJSON(w, http.StatusCreated, map[string]any{
			"ref": body.Ref,
			"object": map[string]string{"sha": sha, "type": "commit"},
		})
		return
	}
	// GET a single ref (or list all when refPath == "")
	repo := repoFromPath(w, owner, name)
	if repo == nil {
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if refPath == "" {
		type refJSON struct {
			Ref    string `json:"ref"`
			Object struct {
				SHA  string `json:"sha"`
				Type string `json:"type"`
			} `json:"object"`
		}
		var out []refJSON
		for _, n := range sortedRefNames(repo) {
			rj := refJSON{Ref: n}
			rj.Object.SHA = repo.Refs[n]
			rj.Object.Type = "commit"
			out = append(out, rj)
		}
		if out == nil {
			out = []refJSON{}
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	full := refPath
	if !strings.HasPrefix(full, "refs/") {
		full = "refs/" + full
	}
	oid, ok := repo.Refs[full]
	if !ok {
		restError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ref": full,
		"object": map[string]string{"sha": oid, "type": "commit"},
	})
}

// --- commits ---

func handleCommits(w http.ResponseWriter, r *http.Request, owner, name string) {
	repo := repoFromPath(w, owner, name)
	if repo == nil {
		return
	}
	s := theStore
	ref := firstQuery(r.URL.Query(), "sha")
	s.mu.RLock()
	tip, ok := s.resolveRef(repo, ref)
	if !ok {
		s.mu.RUnlock()
		restError(w, http.StatusNotFound, "Not Found")
		return
	}
	oids, commits := s.commitListWithIDs(tip)
	type commitJSON struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message   string `json:"message"`
			Author    map[string]any `json:"author"`
			Committer map[string]any `json:"committer"`
		} `json:"commit"`
		Author    *userJSON `json:"author"`
		Committer *userJSON `json:"committer"`
	}
	out := make([]commitJSON, 0, len(oids))
	for i, oid := range oids {
		c := commits[i]
		cj := commitJSON{SHA: oid}
		cj.Commit.Message = c.Message
		cj.Commit.Author = map[string]any{"name": viewerName, "email": viewerEmail, "date": fixedTimeISO}
		cj.Commit.Committer = map[string]any{"name": viewerName, "email": viewerEmail, "date": fixedTimeISO}
		u := viewerUserJSON()
		cj.Author = &u
		cj.Committer = &u
		out = append(out, cj)
	}
	s.mu.RUnlock()
	writeJSON(w, http.StatusOK, out)
}

// --- contents ---

func handleContents(w http.ResponseWriter, r *http.Request, owner, name, path string) {
	s := theStore
	switch r.Method {
	case http.MethodGet:
		repo := repoFromPath(w, owner, name)
		if repo == nil {
			return
		}
		ref := firstQuery(r.URL.Query(), "ref")
		s.mu.RLock()
		tip, ok := s.resolveRef(repo, ref)
		if !ok {
			s.mu.RUnlock()
			restError(w, http.StatusNotFound, "Not Found")
			return
		}
		commit := s.objects[tip]
		if commit == nil || commit.Type != "commit" {
			s.mu.RUnlock()
			restError(w, http.StatusNotFound, "Not Found")
			return
		}
		treeOID := parseCommit(commit.Data).Tree
		blobOID, isDir, entries, ok := s.getPath(treeOID, path)
		if !ok {
			s.mu.RUnlock()
			restError(w, http.StatusNotFound, "Not Found")
			return
		}
		if isDir {
			type dirJSON struct {
				Type string `json:"type"`
				Name string `json:"name"`
				Path string `json:"path"`
				SHA  string `json:"sha"`
				Size int    `json:"size"`
			}
			var out []dirJSON
			for _, e := range entries {
				typ := "file"
				if e.Mode == "40000" {
					typ = "dir"
				}
				p := e.Name
				if path != "" {
					p = path + "/" + e.Name
				}
				out = append(out, dirJSON{Type: typ, Name: e.Name, Path: p, SHA: e.OID})
			}
			if out == nil {
				out = []dirJSON{}
			}
			s.mu.RUnlock()
			writeJSON(w, http.StatusOK, out)
			return
		}
		obj := s.objects[blobOID]
		if obj == nil {
			s.mu.RUnlock()
			restError(w, http.StatusNotFound, "Not Found")
			return
		}
		data := obj.Data
		s.mu.RUnlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"type": "file", "encoding": "base64",
			"content": base64.StdEncoding.EncodeToString(data),
			"sha": blobOID, "size": len(data),
			"name": lastSegment(path), "path": path,
		})
	case http.MethodPut:
		s.contentsPut(w, r, owner, name, path, false)
	case http.MethodDelete:
		s.contentsPut(w, r, owner, name, path, true)
	default:
		restError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
	}
}

func lastSegment(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// contentsPut implements PUT (create/update) and DELETE for contents.
func (s *Store) contentsPut(w http.ResponseWriter, r *http.Request, owner, name, path string, isDelete bool) {
	var body struct {
		Message string `json:"message"`
		Content string `json:"content"`
		SHA     string `json:"sha"`
		Branch  string `json:"branch"`
	}
	if !decodeBody(r, &body) {
		restError(w, http.StatusBadRequest, "Bad Request")
		return
	}
	s.mu.Lock()
	repo := s.getRepo(owner, name)
	if repo == nil {
		s.mu.Unlock()
		restError(w, http.StatusNotFound, "Not Found")
		return
	}
	branch := body.Branch
	if branch == "" {
		branch = repo.DefaultBranch
	}
	tip, exists := repo.Refs["refs/heads/"+branch]
	baseTree := ""
	var parents []string
	if !exists {
		// create the branch from the default branch tip, or from nothing
		// when the repository has no commits yet (first contents write)
		if baseTip, ok := repo.headOID(); ok {
			repo.Refs["refs/heads/"+branch] = baseTip
			s.bumpGen()
			tip = baseTip
			exists = true
		}
	}
	if exists {
		commit := s.objects[tip]
		if commit == nil || commit.Type != "commit" {
			s.mu.Unlock()
			restError(w, http.StatusNotFound, "Not Found")
			return
		}
		baseTree = parseCommit(commit.Data).Tree
		parents = []string{tip}
	} else if isDelete {
		s.mu.Unlock()
		restError(w, http.StatusNotFound, "Not Found")
		return
	}
	curBlob, _, _, curOK := s.getPath(baseTree, path)
	var newTree string
	var blobOID string
	if isDelete {
		if !curOK {
			s.mu.Unlock()
			restError(w, http.StatusNotFound, "Not Found")
			return
		}
		if body.SHA != "" && body.SHA != curBlob {
			s.mu.Unlock()
			restError(w, http.StatusConflict, "SHA does not match")
			return
		}
		nt, ok := s.modifyTree(baseTree, path, "")
		if !ok {
			s.mu.Unlock()
			restError(w, http.StatusBadRequest, "Bad Request")
			return
		}
		newTree = nt
	} else {
		raw, err := base64.StdEncoding.DecodeString(body.Content)
		if err != nil {
			s.mu.Unlock()
			restError(w, http.StatusBadRequest, "Invalid base64 content")
			return
		}
		blobOID = s.putObject("blob", raw)
		if curOK {
			if body.SHA != "" && body.SHA != curBlob {
				s.mu.Unlock()
				restError(w, http.StatusConflict, "SHA does not match")
				return
			}
		}
		nt, ok := s.modifyTree(baseTree, path, blobOID)
		if !ok {
			s.mu.Unlock()
			restError(w, http.StatusBadRequest, "Bad Request")
			return
		}
		newTree = nt
	}
	msg := body.Message
	if msg == "" {
		msg = "Update " + path
	}
	commitData := encodeCommit(newTree, parents, serverAuthorLine(), serverAuthorLine(), msg+"\n")
	newCommit := s.putObject("commit", commitData)
	repo.Refs["refs/heads/"+branch] = newCommit
	status := http.StatusOK
	if !isDelete && !curOK {
		status = http.StatusCreated
	}
	if isDelete {
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"commit": map[string]string{"sha": newCommit},
		})
		return
	}
	resp := map[string]any{
		"content": map[string]any{"sha": blobOID, "encoding": "base64"},
		"commit":  map[string]string{"sha": newCommit},
	}
	s.mu.Unlock()
	writeJSON(w, status, resp)
}

// --- labels ---

func handleLabels(w http.ResponseWriter, r *http.Request, owner, name string) {
	repo := repoFromPath(w, owner, name)
	if repo == nil {
		return
	}
	s := theStore
	if r.Method == http.MethodGet {
		s.mu.RLock()
		names := make([]string, 0, len(repo.Labels))
		for n := range repo.Labels {
			names = append(names, n)
		}
		sort.Strings(names)
		out := make([]labelJSON, 0, len(names))
		for _, n := range names {
			l := repo.Labels[n]
			out = append(out, labelJSON{
				ID: s.labelID(repo, n), NodeID: globalID("Label", s.labelID(repo, n)),
				Name: l.Name, Color: l.Color,
				URL: "https://api.github.com/repos/" + repo.fullName() + "/labels/" + n,
			})
		}
		s.mu.RUnlock()
		writeJSON(w, http.StatusOK, out)
		return
	}
	var body struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	}
	if !decodeBody(r, &body) || body.Name == "" {
		restError(w, http.StatusBadRequest, "Bad Request")
		return
	}
	s.mu.Lock()
	if _, exists := repo.Labels[body.Name]; exists {
		s.mu.Unlock()
		restError(w, http.StatusUnprocessableEntity, "Label already exists")
		return
	}
	color := strings.TrimPrefix(body.Color, "#")
	repo.Labels[body.Name] = &Label{Name: body.Name, Color: color}
	id := s.labelID(repo, body.Name)
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, labelJSON{
		ID: id, NodeID: globalID("Label", id), Name: body.Name, Color: color,
		URL: "https://api.github.com/repos/" + repo.fullName() + "/labels/" + body.Name,
	})
}

// --- issues ---

func handleIssues(w http.ResponseWriter, r *http.Request, owner, name string, baseURL string) {
	repo := repoFromPath(w, owner, name)
	if repo == nil {
		return
	}
	s := theStore
	if r.Method == http.MethodGet {
		q := r.URL.Query()
		p := parsePage(q)
		state := firstQuery(q, "state")
		if state == "" {
			state = "open"
		}
		sortDir := firstQuery(q, "direction")
		if sortDir == "" {
			sortDir = "desc"
		}
		s.mu.RLock()
		var list []*Issue
		for _, iss := range repo.Issues {
			if state == "all" || iss.State == state {
				list = append(list, iss)
			}
		}
		if sortDir == "asc" {
			sort.Slice(list, func(a, b int) bool { return list[a].Seq < list[b].Seq })
		} else {
			sort.Slice(list, func(a, b int) bool { return list[a].Seq > list[b].Seq })
		}
		total := len(list)
		page := paginate(list, p)
		out := make([]issueJSON, 0, len(page))
		for _, iss := range page {
			out = append(out, s.issueToJSON(repo, iss))
		}
		s.mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-GitHub-Request-Id", nextRequestID())
		w.Header().Set("Link", linkHeader(baseURL+"/api/v3/repos/"+owner+"/"+name+"/issues", p, total))
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(out)
		return
	}
	// POST create
	var body struct {
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		Labels []string `json:"labels"`
	}
	if !decodeBody(r, &body) || body.Title == "" {
		restError(w, http.StatusBadRequest, "Bad Request")
		return
	}
	s.mu.Lock()
	for _, l := range body.Labels {
		if _, ok := repo.Labels[l]; !ok {
			s.mu.Unlock()
			restError(w, http.StatusUnprocessableEntity, "Unknown label: "+l)
			return
		}
	}
	iss := &Issue{
		ID: s.nextIssueID, Number: repo.nextIssue, Title: body.Title,
		Body: body.Body, State: "open", Labels: body.Labels, Author: viewerLogin,
		Seq: uint64(s.nextIssueID),
	}
	s.nextIssueID++
	repo.nextIssue++
	repo.Issues = append(repo.Issues, iss)
	out := s.issueToJSON(repo, iss)
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, out)
}

func findIssue(repo *Repo, number int) *Issue {
	for _, iss := range repo.Issues {
		if iss.Number == number {
			return iss
		}
	}
	return nil
}

func handleIssue(w http.ResponseWriter, r *http.Request, owner, name string, number int) {
	repo := repoFromPath(w, owner, name)
	if repo == nil {
		return
	}
	s := theStore
	if r.Method == http.MethodGet {
		s.mu.RLock()
		iss := findIssue(repo, number)
		if iss == nil {
			s.mu.RUnlock()
			restError(w, http.StatusNotFound, "Not Found")
			return
		}
		out := s.issueToJSON(repo, iss)
		s.mu.RUnlock()
		writeJSON(w, http.StatusOK, out)
		return
	}
	if r.Method == http.MethodPatch {
		var body struct {
			Title *string  `json:"title"`
			Body  *string  `json:"body"`
			State *string  `json:"state"`
			Labels []string `json:"labels"`
		}
		if !decodeBody(r, &body) {
			restError(w, http.StatusBadRequest, "Bad Request")
			return
		}
		s.mu.Lock()
		iss := findIssue(repo, number)
		if iss == nil {
			s.mu.Unlock()
			restError(w, http.StatusNotFound, "Not Found")
			return
		}
		if body.Title != nil {
			iss.Title = *body.Title
		}
		if body.Body != nil {
			iss.Body = *body.Body
		}
		if body.State != nil && (*body.State == "open" || *body.State == "closed") {
			iss.State = *body.State
		}
		if body.Labels != nil {
			iss.Labels = body.Labels
		}
		out := s.issueToJSON(repo, iss)
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, out)
		return
	}
	restError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
}

func handleIssueComments(w http.ResponseWriter, r *http.Request, owner, name string, number int) {
	repo := repoFromPath(w, owner, name)
	if repo == nil {
		return
	}
	s := theStore
	if r.Method == http.MethodGet {
		s.mu.RLock()
		iss := findIssue(repo, number)
		if iss == nil {
			s.mu.RUnlock()
			restError(w, http.StatusNotFound, "Not Found")
			return
		}
		type commentJSON struct {
			ID        int      `json:"id"`
			NodeID    string   `json:"node_id"`
			Body      string   `json:"body"`
			User      userJSON `json:"user"`
			CreatedAt string   `json:"created_at"`
			UpdatedAt string   `json:"updated_at"`
		}
		out := make([]commentJSON, 0, len(iss.Comments))
		for _, c := range iss.Comments {
			out = append(out, commentJSON{
				ID: c.ID, NodeID: gqlCommentID(repo.ID, number, c.ID), Body: c.Body,
				User: viewerUserJSON(), CreatedAt: fixedTimeISO, UpdatedAt: fixedTimeISO,
			})
		}
		s.mu.RUnlock()
		writeJSON(w, http.StatusOK, out)
		return
	}
	var body struct {
		Body string `json:"body"`
	}
	if !decodeBody(r, &body) || body.Body == "" {
		restError(w, http.StatusBadRequest, "Bad Request")
		return
	}
	s.mu.Lock()
	iss := findIssue(repo, number)
	if iss == nil {
		s.mu.Unlock()
		restError(w, http.StatusNotFound, "Not Found")
		return
	}
	c := &Comment{ID: s.nextCmtID, Body: body.Body}
	s.nextCmtID++
	iss.Comments = append(iss.Comments, c)
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": c.ID, "node_id": gqlCommentID(repo.ID, number, c.ID), "body": c.Body,
		"user": viewerUserJSON(), "created_at": fixedTimeISO, "updated_at": fixedTimeISO,
	})
}

// --- pulls ---

func findPull(repo *Repo, number int) *Pull {
	for _, pr := range repo.Pulls {
		if pr.Number == number {
			return pr
		}
	}
	return nil
}

func handlePulls(w http.ResponseWriter, r *http.Request, owner, name string, baseURL string) {
	repo := repoFromPath(w, owner, name)
	if repo == nil {
		return
	}
	s := theStore
	if r.Method == http.MethodGet {
		q := r.URL.Query()
		p := parsePage(q)
		state := firstQuery(q, "state")
		if state == "" {
			state = "open"
		}
		s.mu.RLock()
		var list []*Pull
		for _, pr := range repo.Pulls {
			if state == "all" || pr.State == state {
				list = append(list, pr)
			}
		}
		sort.Slice(list, func(a, b int) bool { return list[a].Seq > list[b].Seq })
		total := len(list)
		page := paginate(list, p)
		out := make([]pullJSON, 0, len(page))
		for _, pr := range page {
			out = append(out, s.pullToJSON(repo, pr))
		}
		s.mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-GitHub-Request-Id", nextRequestID())
		w.Header().Set("Link", linkHeader(baseURL+"/api/v3/repos/"+owner+"/"+name+"/pulls", p, total))
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(out)
		return
	}
	// POST create
	var body struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Head  string `json:"head"`
		Base  string `json:"base"`
	}
	if !decodeBody(r, &body) || body.Title == "" || body.Head == "" || body.Base == "" {
		restError(w, http.StatusBadRequest, "Bad Request")
		return
	}
	s.mu.Lock()
	headSHA, headOK := s.resolveRef(repo, body.Head)
	baseSHA, baseOK := s.resolveRef(repo, body.Base)
	if !headOK || !baseOK {
		s.mu.Unlock()
		restError(w, http.StatusUnprocessableEntity, "No commits between "+body.Base+" and "+body.Head)
		return
	}
	_ = headSHA
	_ = baseSHA
	for _, pr := range repo.Pulls {
		if pr.Head == body.Head && pr.Base == body.Base && pr.State == "open" {
			s.mu.Unlock()
			restError(w, http.StatusUnprocessableEntity, "A pull request already exists for "+body.Head)
			return
		}
	}
	pr := &Pull{
		Number: repo.nextPull, Title: body.Title, Body: body.Body,
		Head: body.Head, Base: body.Base, State: "open",
		Seq: uint64(repo.nextPull),
	}
	repo.nextPull++
	repo.Pulls = append(repo.Pulls, pr)
	out := s.pullToJSON(repo, pr)
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, out)
}

func handlePull(w http.ResponseWriter, r *http.Request, owner, name string, number int) {
	repo := repoFromPath(w, owner, name)
	if repo == nil {
		return
	}
	s := theStore
	s.mu.RLock()
	pr := findPull(repo, number)
	if pr == nil {
		s.mu.RUnlock()
		restError(w, http.StatusNotFound, "Not Found")
		return
	}
	out := s.pullToJSON(repo, pr)
	s.mu.RUnlock()
	writeJSON(w, http.StatusOK, out)
}

func handlePullFiles(w http.ResponseWriter, r *http.Request, owner, name string, number int) {
	repo := repoFromPath(w, owner, name)
	if repo == nil {
		return
	}
	s := theStore
	s.mu.RLock()
	pr := findPull(repo, number)
	if pr == nil {
		s.mu.RUnlock()
		restError(w, http.StatusNotFound, "Not Found")
		return
	}
	headTip, ok1 := s.resolveRef(repo, pr.Head)
	baseTip, ok2 := s.resolveRef(repo, pr.Base)
	if !ok1 || !ok2 {
		s.mu.RUnlock()
		restError(w, http.StatusNotFound, "Not Found")
		return
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
	changes := s.diffTrees(mbTree, headTree)
	type fileJSON struct {
		Filename  string `json:"filename"`
		Status    string `json:"status"`
		Additions int    `json:"additions"`
		Deletions int    `json:"deletions"`
		Changes   int    `json:"changes"`
	}
	out := make([]fileJSON, 0, len(changes))
	for _, c := range changes {
		add, del := 0, 0
		if c.Status != "removed" {
			if o := s.objects[c.NewOID]; o != nil {
				add = countLines(o.Data)
			}
		}
		if c.Status != "added" {
			if o := s.objects[c.OldOID]; o != nil {
				del = countLines(o.Data)
			}
		}
		if c.Status == "added" {
			del = 0
		}
		if c.Status == "removed" {
			add = 0
		}
		if add == 0 && del == 0 && c.Status == "modified" {
			add = 1 // a changed path reports at least one addition
		}
		out = append(out, fileJSON{
			Filename: c.Path, Status: c.Status,
			Additions: add, Deletions: del, Changes: add + del,
		})
	}
	s.mu.RUnlock()
	writeJSON(w, http.StatusOK, out)
}

func countLines(b []byte) int {
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	if len(b) > 0 && b[len(b)-1] != '\n' {
		n++
	}
	return n
}

func handlePullMerge(w http.ResponseWriter, r *http.Request, owner, name string, number int) {
	repo := repoFromPath(w, owner, name)
	if repo == nil {
		return
	}
	s := theStore
	var body struct {
		MergeMethod string `json:"merge_method"`
	}
	decodeBody(r, &body) // method is ignored: squash is the only merge we do
	s.mu.Lock()
	pr := findPull(repo, number)
	if pr == nil {
		s.mu.Unlock()
		restError(w, http.StatusNotFound, "Not Found")
		return
	}
	if pr.State != "open" {
		s.mu.Unlock()
		restError(w, http.StatusMethodNotAllowed, "Pull request is not open")
		return
	}
	headTip, ok1 := s.resolveRef(repo, pr.Head)
	baseTip, ok2 := s.resolveRef(repo, pr.Base)
	if !ok1 || !ok2 {
		s.mu.Unlock()
		restError(w, http.StatusNotFound, "Not Found")
		return
	}
	if len(s.mergeConflict(headTip, baseTip)) > 0 {
		s.mu.Unlock()
		restError(w, http.StatusMethodNotAllowed, "Merge conflict")
		return
	}
	sha := s.squashMerge(repo, pr)
	if sha == "" {
		s.mu.Unlock()
		restError(w, http.StatusMethodNotAllowed, "Merge conflict")
		return
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"sha": sha, "merged": true,
		"message": "Pull request successfully merged",
	})
}
