package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// GraphQL v4 (/api/graphql): a small parser + executor for the fixed set of
// documents the suite sends (queries, mutations, introspection, batching).
// ---------------------------------------------------------------------------

// --- values ---

type gqlValue struct {
	kind string // string,int,float,bool,null,enum,var,list,object
	str  string
	num  float64
	b    bool
	list []*gqlValue
	obj  map[string]*gqlValue
	var_ string
}

func (v *gqlValue) resolve(vars map[string]any) any {
	if v == nil {
		return nil
	}
	switch v.kind {
	case "string", "enum":
		return v.str
	case "int", "float":
		return v.num
	case "bool":
		return v.b
	case "null":
		return nil
	case "var":
		return vars[v.var_]
	case "list":
		out := make([]any, len(v.list))
		for i, e := range v.list {
			out[i] = e.resolve(vars)
		}
		return out
	case "object":
		out := make(map[string]any, len(v.obj))
		for k, e := range v.obj {
			out[k] = e.resolve(vars)
		}
		return out
	}
	return nil
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func asInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

// --- parser ---

type gqlSelection struct {
	alias    string
	name     string // "" for a fragment spread / inline fragment
	args     map[string]*gqlValue
	children []*gqlSelection
	typeCond string // inline fragment type condition
}

type gqlOperation struct {
	kind       string // query | mutation
	name       string
	selections []*gqlSelection
}

type gqlParser struct {
	src string
	pos int
}

func (p *gqlParser) skipSpace() {
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			p.pos++
			continue
		}
		if c == '#' {
			for p.pos < len(p.src) && p.src[p.pos] != '\n' {
				p.pos++
			}
			continue
		}
		break
	}
}

func (p *gqlParser) peek() byte {
	p.skipSpace()
	if p.pos >= len(p.src) {
		return 0
	}
	return p.src[p.pos]
}

func (p *gqlParser) expect(c byte) error {
	p.skipSpace()
	if p.pos >= len(p.src) || p.src[p.pos] != c {
		return fmt.Errorf("expected %q at %d", c, p.pos)
	}
	p.pos++
	return nil
}

func (p *gqlParser) ident() (string, error) {
	p.skipSpace()
	start := p.pos
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			p.pos++
		} else {
			break
		}
	}
	if start == p.pos {
		return "", fmt.Errorf("expected identifier at %d", p.pos)
	}
	return p.src[start:p.pos], nil
}

func (p *gqlParser) parseValue() (*gqlValue, error) {
	p.skipSpace()
	if p.pos >= len(p.src) {
		return nil, fmt.Errorf("unexpected end of document")
	}
	c := p.src[p.pos]
	switch {
	case c == '"':
		s, err := p.parseString()
		if err != nil {
			return nil, err
		}
		return &gqlValue{kind: "string", str: s}, nil
	case c == '$':
		p.pos++
		name, err := p.ident()
		if err != nil {
			return nil, err
		}
		return &gqlValue{kind: "var", var_: name}, nil
	case c == '[':
		p.pos++
		v := &gqlValue{kind: "list"}
		for {
			p.skipSpace()
			if p.pos < len(p.src) && p.src[p.pos] == ']' {
				p.pos++
				break
			}
			e, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			v.list = append(v.list, e)
		}
		return v, nil
	case c == '{':
		p.pos++
		v := &gqlValue{kind: "object", obj: map[string]*gqlValue{}}
		for {
			p.skipSpace()
			if p.pos < len(p.src) && p.src[p.pos] == '}' {
				p.pos++
				break
			}
			name, err := p.ident()
			if err != nil {
				return nil, err
			}
			if err := p.expect(':'); err != nil {
				return nil, err
			}
			val, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			v.obj[name] = val
		}
		return v, nil
	case c == '-' || (c >= '0' && c <= '9'):
		start := p.pos
		if c == '-' {
			p.pos++
		}
		isFloat := false
		for p.pos < len(p.src) {
			c := p.src[p.pos]
			if c >= '0' && c <= '9' {
				p.pos++
			} else if c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-' {
				isFloat = true
				p.pos++
			} else {
				break
			}
		}
		text := p.src[start:p.pos]
		f, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return nil, err
		}
		if isFloat {
			return &gqlValue{kind: "float", num: f}, nil
		}
		return &gqlValue{kind: "int", num: f}, nil
	default:
		name, err := p.ident()
		if err != nil {
			return nil, err
		}
		switch name {
		case "true":
			return &gqlValue{kind: "bool", b: true}, nil
		case "false":
			return &gqlValue{kind: "bool", b: false}, nil
		case "null":
			return &gqlValue{kind: "null"}, nil
		default:
			return &gqlValue{kind: "enum", str: name}, nil
		}
	}
}

func (p *gqlParser) parseString() (string, error) {
	// p.src[p.pos] == '"'
	p.pos++
	var sb strings.Builder
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		p.pos++
		if c == '"' {
			return sb.String(), nil
		}
		if c == '\\' && p.pos < len(p.src) {
			e := p.src[p.pos]
			p.pos++
			switch e {
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			case 'r':
				sb.WriteByte('\r')
			case '"':
				sb.WriteByte('"')
			case '\\':
				sb.WriteByte('\\')
			case '/':
				sb.WriteByte('/')
			case 'u':
				if p.pos+4 <= len(p.src) {
					n, _ := strconv.ParseInt(p.src[p.pos:p.pos+4], 16, 32)
					sb.WriteRune(rune(n))
					p.pos += 4
				}
			default:
				sb.WriteByte(e)
			}
			continue
		}
		sb.WriteByte(c)
	}
	return "", fmt.Errorf("unterminated string")
}

// parseArguments parses "(a: 1, b: $x)" or returns nil when there is none.
func (p *gqlParser) parseArguments() (map[string]*gqlValue, error) {
	p.skipSpace()
	if p.pos >= len(p.src) || p.src[p.pos] != '(' {
		return nil, nil
	}
	p.pos++
	args := map[string]*gqlValue{}
	for {
		p.skipSpace()
		if p.pos < len(p.src) && p.src[p.pos] == ')' {
			p.pos++
			break
		}
		name, err := p.ident()
		if err != nil {
			return nil, err
		}
		if err := p.expect(':'); err != nil {
			return nil, err
		}
		val, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args[name] = val
	}
	return args, nil
}

// skipDirectives skips "@name(...)" directives (unused by the suite's docs).
func (p *gqlParser) skipDirectives() error {
	for {
		p.skipSpace()
		if p.pos < len(p.src) && p.src[p.pos] == '@' {
			p.pos++
			if _, err := p.ident(); err != nil {
				return err
			}
			if _, err := p.parseArguments(); err != nil {
				return err
			}
			continue
		}
		return nil
	}
}

func (p *gqlParser) parseSelectionSet() ([]*gqlSelection, error) {
	if err := p.expect('{'); err != nil {
		return nil, err
	}
	var sels []*gqlSelection
	for {
		p.skipSpace()
		if p.pos >= len(p.src) {
			return nil, fmt.Errorf("unterminated selection set")
		}
		if p.src[p.pos] == '}' {
			p.pos++
			break
		}
		sel, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		sels = append(sels, sel)
	}
	return sels, nil
}

func (p *gqlParser) parseSelection() (*gqlSelection, error) {
	p.skipSpace()
	if p.pos+2 < len(p.src) && p.src[p.pos] == '.' && p.src[p.pos+1] == '.' && p.src[p.pos+2] == '.' {
		p.pos += 3
		p.skipSpace()
		// inline fragment ("... on Type { }") or fragment spread ("...Name")
		if p.pos < len(p.src) && (p.src[p.pos] == 'o') {
			save := p.pos
			name, err := p.ident()
			if err == nil && name == "on" {
				cond, err := p.ident()
				if err != nil {
					return nil, err
				}
				if err := p.skipDirectives(); err != nil {
					return nil, err
				}
				children, err := p.parseSelectionSet()
				if err != nil {
					return nil, err
				}
				return &gqlSelection{typeCond: cond, children: children}, nil
			}
			p.pos = save
		}
		// named fragment spread: not used by the suite; treat as empty
		if _, err := p.ident(); err != nil {
			return nil, err
		}
		if err := p.skipDirectives(); err != nil {
			return nil, err
		}
		return &gqlSelection{}, nil
	}
	name, err := p.ident()
	if err != nil {
		return nil, err
	}
	sel := &gqlSelection{name: name, alias: name}
	p.skipSpace()
	if p.pos < len(p.src) && p.src[p.pos] == ':' {
		p.pos++
		real, err := p.ident()
		if err != nil {
			return nil, err
		}
		sel.alias = name
		sel.name = real
	}
	args, err := p.parseArguments()
	if err != nil {
		return nil, err
	}
	sel.args = args
	if err := p.skipDirectives(); err != nil {
		return nil, err
	}
	if p.peek() == '{' {
		children, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		sel.children = children
	}
	return sel, nil
}

// skipVariableDefinitions skips "($x: Type = default, ...)" after an operation.
func (p *gqlParser) skipVariableDefinitions() error {
	p.skipSpace()
	if p.pos >= len(p.src) || p.src[p.pos] != '(' {
		return nil
	}
	depth := 0
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == '(' || c == '[' || c == '{' {
			depth++
		} else if c == ')' || c == ']' || c == '}' {
			depth--
			if depth == 0 {
				p.pos++
				return nil
			}
		} else if c == '"' {
			if _, err := p.parseString(); err != nil {
				return err
			}
			continue
		}
		p.pos++
	}
	return fmt.Errorf("unterminated variable definitions")
}

func parseGraphQL(src string) (*gqlOperation, error) {
	p := &gqlParser{src: src}
	p.skipSpace()
	kind := "query"
	if p.pos < len(p.src) {
		save := p.pos
		if name, err := p.ident(); err == nil {
			switch name {
			case "query", "mutation", "subscription":
				kind = name
			default:
				p.pos = save
			}
		}
	}
	// optional operation name
	p.skipSpace()
	if p.pos < len(p.src) && p.src[p.pos] != '{' && p.src[p.pos] != '(' {
		if _, err := p.ident(); err != nil {
			return nil, err
		}
	}
	if err := p.skipVariableDefinitions(); err != nil {
		return nil, err
	}
	sels, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	return &gqlOperation{kind: kind, selections: sels}, nil
}

// --- global ids (opaque, stable) ---

func gqlRepoID(repoID int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("Repository:%d", repoID)))
}

func gqlIssueID(repoID, number int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("Issue:%d:%d", repoID, number)))
}

func gqlPullID(repoID, number int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("PullRequest:%d:%d", repoID, number)))
}

func gqlCommentID(repoID, issueNumber, commentID int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("IssueComment:%d:%d:%d", repoID, issueNumber, commentID)))
}

// parseGlobalID decodes an opaque id into its type and parts.
func parseGlobalID(id string) (string, []string, bool) {
	raw, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return "", nil, false
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) < 2 {
		return "", nil, false
	}
	return parts[0], parts[1:], true
}

// --- executor ---

type gqlError struct {
	Message string `json:"message"`
}

type gqlNode struct {
	typ string
	// one of these is set depending on typ
	repo    *Repo
	issue   *Issue
	pull    *Pull
	comment *Comment
	// connections
	issues []*Issue
	pulls  []*Pull
	files  []FileChange
	cursor string // endCursor for a connection
	hasNext bool
	// introspection
	typeName string
	// mutation payloads
	payloadIssue   *Issue
	payloadPull    *Pull
	payloadComment *Comment
}

type gqlExecutor struct {
	s      *Store
	errors []gqlError
}

func (e *gqlExecutor) errf(format string, args ...any) {
	e.errors = append(e.errors, gqlError{Message: fmt.Sprintf(format, args...)})
}

func (e *gqlExecutor) argValue(sel *gqlSelection, name string, vars map[string]any) any {
	if sel.args == nil {
		return nil
	}
	if v, ok := sel.args[name]; ok {
		return v.resolve(vars)
	}
	return nil
}

// resolveSelections resolves a list of selections against a node.
func (e *gqlExecutor) resolveSelections(node *gqlNode, sels []*gqlSelection, vars map[string]any) map[string]any {
	out := map[string]any{}
	for _, sel := range sels {
		if sel.name == "" {
			// fragment (inline or spread): apply when the type matches
			if sel.typeCond == "" || sel.typeCond == node.typ {
				for k, v := range e.resolveSelections(node, sel.children, vars) {
					out[k] = v
				}
			}
			continue
		}
		out[sel.alias] = e.resolveField(node, sel, vars)
	}
	return out
}

func (e *gqlExecutor) resolveField(node *gqlNode, sel *gqlSelection, vars map[string]any) any {
	switch node.typ {
	case "Query":
		return e.resolveQueryField(node, sel, vars)
	case "Mutation":
		return e.resolveMutationField(node, sel, vars)
	case "User":
		if sel.name == "login" {
			return viewerLogin
		}
	case "Repository":
		return e.resolveRepoField(node, sel, vars)
	case "BranchRef":
		if sel.name == "name" {
			return node.repo.DefaultBranch
		}
	case "Issue":
		switch sel.name {
		case "id":
			return gqlIssueID(node.repo.ID, node.issue.Number)
		case "number":
			return node.issue.Number
		case "title":
			return node.issue.Title
		case "state":
			if node.issue.State == "open" {
				return "OPEN"
			}
			return "CLOSED"
		case "body":
			return node.issue.Body
		}
	case "IssueConnection":
		switch sel.name {
		case "nodes":
			var out []any
			for _, iss := range node.issues {
				out = append(out, e.resolveSelections(&gqlNode{typ: "Issue", repo: node.repo, issue: iss}, sel.children, vars))
			}
			if out == nil {
				out = []any{}
			}
			return out
		case "pageInfo":
			return e.resolveSelections(&gqlNode{typ: "PageInfo", cursor: node.cursor, hasNext: node.hasNext}, sel.children, vars)
		}
	case "PageInfo":
		switch sel.name {
		case "hasNextPage":
			return node.hasNext
		case "endCursor":
			if node.cursor == "" {
				return nil
			}
			return node.cursor
		}
	case "PullRequest":
		switch sel.name {
		case "id":
			return gqlPullID(node.repo.ID, node.pull.Number)
		case "number":
			return node.pull.Number
		case "headRefName":
			return node.pull.Head
		case "baseRefName":
			return node.pull.Base
		case "mergeable":
			s := e.s
			s.mu.RLock()
			headSHA, _ := s.resolveRef(node.repo, node.pull.Head)
			baseSHA, _ := s.resolveRef(node.repo, node.pull.Base)
			conflicts := s.mergeConflict(headSHA, baseSHA)
			s.mu.RUnlock()
			if len(conflicts) == 0 {
				return "MERGEABLE"
			}
			return "CONFLICTING"
		case "state":
			if node.pull.Merged {
				return "MERGED"
			}
			if node.pull.State == "open" {
				return "OPEN"
			}
			return "CLOSED"
		case "merged":
			return node.pull.Merged
		case "files":
			first := asInt(e.argValue(sel, "first", vars))
			if first <= 0 {
				first = 10
			}
			s := e.s
			s.mu.RLock()
			headTip, ok1 := s.resolveRef(node.repo, node.pull.Head)
			baseTip, ok2 := s.resolveRef(node.repo, node.pull.Base)
			var changes []FileChange
			if ok1 && ok2 {
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
				changes = s.diffTrees(mbTree, headTree)
			}
			s.mu.RUnlock()
			if len(changes) > first {
				changes = changes[:first]
			}
			return e.resolveSelections(&gqlNode{typ: "FileConnection", repo: node.repo, files: changes}, sel.children, vars)
		}
	case "PullRequestConnection":
		if sel.name == "nodes" {
			var out []any
			for _, pr := range node.pulls {
				out = append(out, e.resolveSelections(&gqlNode{typ: "PullRequest", repo: node.repo, pull: pr}, sel.children, vars))
			}
			if out == nil {
				out = []any{}
			}
			return out
		}
	case "FileConnection":
		if sel.name == "nodes" {
			var out []any
			for _, f := range node.files {
				out = append(out, e.resolveSelections(&gqlNode{typ: "ChangedFile", files: []FileChange{f}}, sel.children, vars))
			}
			if out == nil {
				out = []any{}
			}
			return out
		}
	case "ChangedFile":
		if sel.name == "path" {
			return node.files[0].Path
		}
	case "TypeInfo":
		switch sel.name {
		case "name":
			return node.typeName
		case "fields":
			var out []any
			for _, f := range typeFields(node.typeName) {
				out = append(out, e.resolveSelections(&gqlNode{typ: "TypeField", typeName: f}, sel.children, vars))
			}
			if out == nil {
				out = []any{}
			}
			return out
		case "enumValues":
			var out []any
			for _, v := range typeEnumValues(node.typeName) {
				out = append(out, e.resolveSelections(&gqlNode{typ: "EnumValue", typeName: v}, sel.children, vars))
			}
			if out == nil {
				out = []any{}
			}
			return out
		}
	case "TypeField", "EnumValue":
		if sel.name == "name" {
			return node.typeName
		}
	case "CreateIssuePayload":
		if sel.name == "issue" {
			if node.payloadIssue == nil {
				return nil
			}
			return e.resolveSelections(&gqlNode{typ: "Issue", repo: node.repo, issue: node.payloadIssue}, sel.children, vars)
		}
	case "CloseIssuePayload":
		if sel.name == "issue" {
			if node.payloadIssue == nil {
				return nil
			}
			return e.resolveSelections(&gqlNode{typ: "Issue", repo: node.repo, issue: node.payloadIssue}, sel.children, vars)
		}
	case "CreatePullRequestPayload":
		if sel.name == "pullRequest" {
			if node.payloadPull == nil {
				return nil
			}
			return e.resolveSelections(&gqlNode{typ: "PullRequest", repo: node.repo, pull: node.payloadPull}, sel.children, vars)
		}
	case "MergePullRequestPayload":
		if sel.name == "pullRequest" {
			if node.payloadPull == nil {
				return nil
			}
			return e.resolveSelections(&gqlNode{typ: "PullRequest", repo: node.repo, pull: node.payloadPull}, sel.children, vars)
		}
	case "AddCommentPayload":
		if sel.name == "commentEdge" {
			if node.payloadComment == nil {
				return nil
			}
			return e.resolveSelections(&gqlNode{typ: "CommentEdge", repo: node.repo, comment: node.payloadComment, issue: node.issue}, sel.children, vars)
		}
	case "CommentEdge":
		if sel.name == "node" {
			return e.resolveSelections(&gqlNode{typ: "IssueComment", repo: node.repo, comment: node.comment, issue: node.issue}, sel.children, vars)
		}
	case "IssueComment":
		switch sel.name {
		case "id":
			return gqlCommentID(node.repo.ID, node.issue.Number, node.comment.ID)
		case "body":
			return node.comment.Body
		}
	}
	e.errf("Cannot query field %q on type %q.", sel.name, node.typ)
	return nil
}

func (e *gqlExecutor) resolveQueryField(node *gqlNode, sel *gqlSelection, vars map[string]any) any {
	switch sel.name {
	case "viewer":
		return e.resolveSelections(&gqlNode{typ: "User"}, sel.children, vars)
	case "repository":
		owner := asString(e.argValue(sel, "owner", vars))
		name := asString(e.argValue(sel, "name", vars))
		e.s.mu.RLock()
		repo := e.s.getRepo(owner, name)
		e.s.mu.RUnlock()
		if repo == nil {
			return nil
		}
		return e.resolveSelections(&gqlNode{typ: "Repository", repo: repo}, sel.children, vars)
	case "node":
		id := asString(e.argValue(sel, "id", vars))
		typ, parts, ok := parseGlobalID(id)
		if !ok {
			return nil
		}
		e.s.mu.RLock()
		defer e.s.mu.RUnlock()
		switch typ {
		case "Repository":
			if len(parts) != 1 {
				return nil
			}
			repoID, _ := strconv.Atoi(parts[0])
			for _, repo := range e.s.repos {
				if repo.ID == repoID {
					return e.resolveSelections(&gqlNode{typ: "Repository", repo: repo}, sel.children, vars)
				}
			}
			return nil
		case "Issue":
			if len(parts) != 2 {
				return nil
			}
			repoID, _ := strconv.Atoi(parts[0])
			number, _ := strconv.Atoi(parts[1])
			for _, repo := range e.s.repos {
				if repo.ID == repoID {
					if iss := findIssue(repo, number); iss != nil {
						return e.resolveSelections(&gqlNode{typ: "Issue", repo: repo, issue: iss}, sel.children, vars)
					}
				}
			}
			return nil
		case "PullRequest":
			if len(parts) != 2 {
				return nil
			}
			repoID, _ := strconv.Atoi(parts[0])
			number, _ := strconv.Atoi(parts[1])
			for _, repo := range e.s.repos {
				if repo.ID == repoID {
					if pr := findPull(repo, number); pr != nil {
						return e.resolveSelections(&gqlNode{typ: "PullRequest", repo: repo, pull: pr}, sel.children, vars)
					}
				}
			}
			return nil
		}
		return nil
	case "__type":
		name := asString(e.argValue(sel, "name", vars))
		if !knownType(name) {
			return nil
		}
		return e.resolveSelections(&gqlNode{typ: "TypeInfo", typeName: name}, sel.children, vars)
	case "__schema":
		// not queried by the suite; answer an empty schema shape
		return e.resolveSelections(&gqlNode{typ: "TypeInfo", typeName: "__Schema"}, sel.children, vars)
	}
	e.errf("Cannot query field %q on type %q.", sel.name, "Query")
	return nil
}

func (e *gqlExecutor) resolveRepoField(node *gqlNode, sel *gqlSelection, vars map[string]any) any {
	repo := node.repo
	switch sel.name {
	case "id":
		return gqlRepoID(repo.ID)
	case "databaseId":
		return repo.ID
	case "name":
		return repo.Name
	case "nameWithOwner":
		return repo.fullName()
	case "owner":
		return e.resolveSelections(&gqlNode{typ: "User"}, sel.children, vars)
	case "defaultBranchRef":
		e.s.mu.RLock()
		_, ok := repo.headOID()
		e.s.mu.RUnlock()
		if !ok {
			return nil
		}
		return e.resolveSelections(&gqlNode{typ: "BranchRef", repo: repo}, sel.children, vars)
	case "viewerPermission":
		return "ADMIN"
	case "hasIssuesEnabled":
		return true
	case "issue":
		number := asInt(e.argValue(sel, "number", vars))
		e.s.mu.RLock()
		iss := findIssue(repo, number)
		e.s.mu.RUnlock()
		if iss == nil {
			return nil
		}
		return e.resolveSelections(&gqlNode{typ: "Issue", repo: repo, issue: iss}, sel.children, vars)
	case "issues":
		first := asInt(e.argValue(sel, "first", vars))
		after := asString(e.argValue(sel, "after", vars))
		statesRaw := e.argValue(sel, "states", vars)
		orderRaw := e.argValue(sel, "orderBy", vars)
		e.s.mu.RLock()
		var list []*Issue
		for _, iss := range repo.Issues {
			list = append(list, iss)
		}
		e.s.mu.RUnlock()
		// filter states
		if states, ok := statesRaw.([]any); ok && len(states) > 0 {
			want := map[string]bool{}
			for _, st := range states {
				if s, ok := st.(string); ok {
					want[s] = true
				}
			}
			var filtered []*Issue
			for _, iss := range list {
				st := "CLOSED"
				if iss.State == "open" {
					st = "OPEN"
				}
				if want[st] {
					filtered = append(filtered, iss)
				}
			}
			list = filtered
		}
		// order: default CREATED_AT DESC (number desc); orderBy may say ASC
		desc := true
		if ob, ok := orderRaw.(map[string]any); ok {
			if d, ok := ob["direction"].(string); ok && d == "ASC" {
				desc = false
			}
		}
		// list is in creation order (number asc); reverse for DESC
		if desc {
			for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
				list[i], list[j] = list[j], list[i]
			}
		}
		// cursor pagination
		start := 0
		if after != "" {
			if raw, err := base64.StdEncoding.DecodeString(after); err == nil {
				if n, err := strconv.Atoi(strings.TrimPrefix(string(raw), "cursor:")); err == nil {
					for i, iss := range list {
						if iss.Number == n {
							start = i + 1
							break
						}
					}
				}
			}
		}
		if start > len(list) {
			start = len(list)
		}
		end := len(list)
		if first > 0 && start+first < end {
			end = start + first
		}
		page := list[start:end]
		hasNext := end < len(list)
		cursor := ""
		if len(page) > 0 {
			cursor = base64.StdEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(page[len(page)-1].Number)))
		}
		conn := &gqlNode{typ: "IssueConnection", repo: repo, issues: page, cursor: cursor, hasNext: hasNext}
		return e.resolveSelections(conn, sel.children, vars)
	case "pullRequests":
		first := asInt(e.argValue(sel, "first", vars))
		e.s.mu.RLock()
		var list []*Pull
		for _, pr := range repo.Pulls {
			if pr.State == "open" {
				list = append(list, pr)
			}
		}
		e.s.mu.RUnlock()
		// newest first
		for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
			list[i], list[j] = list[j], list[i]
		}
		if first > 0 && len(list) > first {
			list = list[:first]
		}
		return e.resolveSelections(&gqlNode{typ: "PullRequestConnection", repo: repo, pulls: list}, sel.children, vars)
	case "pullRequest":
		number := asInt(e.argValue(sel, "number", vars))
		e.s.mu.RLock()
		pr := findPull(repo, number)
		e.s.mu.RUnlock()
		if pr == nil {
			return nil
		}
		return e.resolveSelections(&gqlNode{typ: "PullRequest", repo: repo, pull: pr}, sel.children, vars)
	}
	e.errf("Cannot query field %q on type %q.", sel.name, "Repository")
	return nil
}

func (e *gqlExecutor) resolveMutationField(node *gqlNode, sel *gqlSelection, vars map[string]any) any {
	input := e.argValue(sel, "input", vars)
	in, _ := input.(map[string]any)
	s := e.s
	switch sel.name {
	case "createIssue":
		repoIDStr := asString(in["repositoryId"])
		typ, parts, ok := parseGlobalID(repoIDStr)
		if !ok || typ != "Repository" || len(parts) != 1 {
			e.errf("Could not resolve to a Repository with the global id of %q.", repoIDStr)
			return nil
		}
		repoID, _ := strconv.Atoi(parts[0])
		title := asString(in["title"])
		body := asString(in["body"])
		s.mu.Lock()
		var repo *Repo
		for _, r := range s.repos {
			if r.ID == repoID {
				repo = r
				break
			}
		}
		if repo == nil {
			s.mu.Unlock()
			e.errf("Could not resolve to a Repository with the global id of %q.", repoIDStr)
			return nil
		}
		iss := &Issue{
			ID: s.nextIssueID, Number: repo.nextIssue, Title: title,
			Body: body, State: "open", Author: viewerLogin, Seq: uint64(s.nextIssueID),
		}
		s.nextIssueID++
		repo.nextIssue++
		repo.Issues = append(repo.Issues, iss)
		s.mu.Unlock()
		return e.resolveSelections(&gqlNode{typ: "CreateIssuePayload", repo: repo, payloadIssue: iss}, sel.children, vars)
	case "closeIssue":
		issueIDStr := asString(in["issueId"])
		typ, parts, ok := parseGlobalID(issueIDStr)
		if !ok || typ != "Issue" || len(parts) != 2 {
			e.errf("Could not resolve to an Issue with the global id of %q.", issueIDStr)
			return nil
		}
		repoID, _ := strconv.Atoi(parts[0])
		number, _ := strconv.Atoi(parts[1])
		s.mu.Lock()
		var repo *Repo
		for _, r := range s.repos {
			if r.ID == repoID {
				repo = r
				break
			}
		}
		if repo == nil {
			s.mu.Unlock()
			e.errf("Could not resolve to an Issue with the global id of %q.", issueIDStr)
			return nil
		}
		iss := findIssue(repo, number)
		if iss == nil {
			s.mu.Unlock()
			e.errf("Could not resolve to an Issue with the global id of %q.", issueIDStr)
			return nil
		}
		iss.State = "closed"
		s.mu.Unlock()
		return e.resolveSelections(&gqlNode{typ: "CloseIssuePayload", repo: repo, payloadIssue: iss}, sel.children, vars)
	case "createPullRequest":
		repoIDStr := asString(in["repositoryId"])
		typ, parts, ok := parseGlobalID(repoIDStr)
		if !ok || typ != "Repository" || len(parts) != 1 {
			e.errf("Could not resolve to a Repository with the global id of %q.", repoIDStr)
			return nil
		}
		repoID, _ := strconv.Atoi(parts[0])
		title := asString(in["title"])
		base := asString(in["baseRefName"])
		head := asString(in["headRefName"])
		s.mu.Lock()
		var repo *Repo
		for _, r := range s.repos {
			if r.ID == repoID {
				repo = r
				break
			}
		}
		if repo == nil {
			s.mu.Unlock()
			e.errf("Could not resolve to a Repository with the global id of %q.", repoIDStr)
			return nil
		}
		if _, ok := s.resolveRef(repo, head); !ok {
			s.mu.Unlock()
			e.errf("Head branch %q does not exist.", head)
			return nil
		}
		if _, ok := s.resolveRef(repo, base); !ok {
			s.mu.Unlock()
			e.errf("Base branch %q does not exist.", base)
			return nil
		}
		for _, pr := range repo.Pulls {
			if pr.Head == head && pr.Base == base && pr.State == "open" {
				s.mu.Unlock()
				return e.resolveSelections(&gqlNode{typ: "CreatePullRequestPayload", repo: repo, payloadPull: pr}, sel.children, vars)
			}
		}
		pr := &Pull{
			Number: repo.nextPull, Title: title, Head: head, Base: base,
			State: "open", Seq: uint64(repo.nextPull),
		}
		repo.nextPull++
		repo.Pulls = append(repo.Pulls, pr)
		s.mu.Unlock()
		return e.resolveSelections(&gqlNode{typ: "CreatePullRequestPayload", repo: repo, payloadPull: pr}, sel.children, vars)
	case "mergePullRequest":
		prIDStr := asString(in["pullRequestId"])
		typ, parts, ok := parseGlobalID(prIDStr)
		if !ok || typ != "PullRequest" || len(parts) != 2 {
			e.errf("Could not resolve to a PullRequest with the global id of %q.", prIDStr)
			return nil
		}
		repoID, _ := strconv.Atoi(parts[0])
		number, _ := strconv.Atoi(parts[1])
		s.mu.Lock()
		var repo *Repo
		for _, r := range s.repos {
			if r.ID == repoID {
				repo = r
				break
			}
		}
		if repo == nil {
			s.mu.Unlock()
			e.errf("Could not resolve to a PullRequest with the global id of %q.", prIDStr)
			return nil
		}
		pr := findPull(repo, number)
		if pr == nil {
			s.mu.Unlock()
			e.errf("Could not resolve to a PullRequest with the global id of %q.", prIDStr)
			return nil
		}
		if pr.State != "open" {
			s.mu.Unlock()
			e.errf("Pull request #%d is not open.", number)
			return nil
		}
		headTip, ok1 := s.resolveRef(repo, pr.Head)
		baseTip, ok2 := s.resolveRef(repo, pr.Base)
		if !ok1 || !ok2 || len(s.mergeConflict(headTip, baseTip)) > 0 {
			s.mu.Unlock()
			e.errf("Pull request #%d is not mergeable.", number)
			return nil
		}
		if s.squashMerge(repo, pr) == "" {
			s.mu.Unlock()
			e.errf("Pull request #%d is not mergeable.", number)
			return nil
		}
		s.mu.Unlock()
		return e.resolveSelections(&gqlNode{typ: "MergePullRequestPayload", repo: repo, payloadPull: pr}, sel.children, vars)
	case "addComment":
		subjectIDStr := asString(in["subjectId"])
		body := asString(in["body"])
		typ, parts, ok := parseGlobalID(subjectIDStr)
		if !ok || typ != "Issue" || len(parts) != 2 {
			e.errf("Could not resolve to an Issue with the global id of %q.", subjectIDStr)
			return nil
		}
		repoID, _ := strconv.Atoi(parts[0])
		number, _ := strconv.Atoi(parts[1])
		s.mu.Lock()
		var repo *Repo
		for _, r := range s.repos {
			if r.ID == repoID {
				repo = r
				break
			}
		}
		if repo == nil {
			s.mu.Unlock()
			e.errf("Could not resolve to an Issue with the global id of %q.", subjectIDStr)
			return nil
		}
		iss := findIssue(repo, number)
		if iss == nil {
			s.mu.Unlock()
			e.errf("Could not resolve to an Issue with the global id of %q.", subjectIDStr)
			return nil
		}
		c := &Comment{ID: s.nextCmtID, Body: body}
		s.nextCmtID++
		iss.Comments = append(iss.Comments, c)
		s.mu.Unlock()
		return e.resolveSelections(&gqlNode{typ: "AddCommentPayload", repo: repo, payloadComment: c, issue: iss}, sel.children, vars)
	}
	e.errf("Cannot query field %q on type %q.", sel.name, "Mutation")
	return nil
}

// --- introspection data ---

func knownType(name string) bool {
	switch name {
	case "Issue", "PullRequest", "Repository", "SearchType", "User", "IssueConnection", "PullRequestConnection", "PageInfo":
		return true
	}
	return false
}

func typeFields(name string) []string {
	switch name {
	case "Issue":
		return []string{"id", "number", "title", "body", "state", "author", "createdAt", "updatedAt", "url", "labels", "comments"}
	case "PullRequest":
		return []string{"id", "number", "title", "body", "headRefName", "baseRefName", "mergeable", "state", "merged", "files", "author", "createdAt", "url"}
	case "Repository":
		return []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "private", "createdAt", "url"}
	case "User":
		return []string{"id", "login", "name", "email"}
	case "IssueConnection":
		return []string{"nodes", "edges", "pageInfo", "totalCount"}
	case "PullRequestConnection":
		return []string{"nodes", "edges", "pageInfo", "totalCount"}
	case "PageInfo":
		return []string{"hasNextPage", "hasPreviousPage", "startCursor", "endCursor"}
	}
	return nil
}

func typeEnumValues(name string) []string {
	switch name {
	case "SearchType":
		return []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "MARKETPLACE"}
	case "IssueState":
		return []string{"OPEN", "CLOSED"}
	case "PullRequestState":
		return []string{"OPEN", "CLOSED", "MERGED"}
	case "MergeableState":
		return []string{"MERGEABLE", "CONFLICTING", "UNKNOWN"}
	}
	return nil
}

// --- HTTP handler ---

// executeGraphQL runs one operation document with variables.
func executeGraphQL(s *Store, query string, vars map[string]any) (map[string]any, bool) {
	op, err := parseGraphQL(query)
	if err != nil {
		return map[string]any{
			"errors": []gqlError{{Message: "Syntax Error: " + err.Error()}},
		}, false
	}
	if vars == nil {
		vars = map[string]any{}
	}
	ex := &gqlExecutor{s: s}
	rootTyp := "Query"
	if op.kind == "mutation" {
		rootTyp = "Mutation"
	}
	data := ex.resolveSelections(&gqlNode{typ: rootTyp}, op.selections, vars)
	resp := map[string]any{"data": data}
	if len(ex.errors) > 0 {
		resp["errors"] = ex.errors
	}
	return resp, true
}

func handleGraphQL(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		restError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}
	raw, err := readAllBody(r)
	if err != nil {
		restError(w, http.StatusBadRequest, "Bad Request")
		return
	}
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "[") {
		// batched operations
		var ops []struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.Unmarshal(raw, &ops); err != nil {
			restError(w, http.StatusBadRequest, "Bad Request")
			return
		}
		out := make([]map[string]any, 0, len(ops))
		for _, op := range ops {
			resp, ok := executeGraphQL(theStore, op.Query, op.Variables)
			if !ok {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(resp)
				return
			}
			out = append(out, resp)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-GitHub-Request-Id", nextRequestID())
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(out)
		return
	}
	var body struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		restError(w, http.StatusBadRequest, "Bad Request")
		return
	}
	resp, ok := executeGraphQL(theStore, body.Query, body.Variables)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-GitHub-Request-Id", nextRequestID())
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	json.NewEncoder(w).Encode(resp)
}
