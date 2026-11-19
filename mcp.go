package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// ---------------------------------------------------------------------------
// MCP (Model Context Protocol) over Streamable HTTP at /mcp.
// Modern era (2026-07-28): server/discover, no handshake, no session.
// Legacy era is also answered: initialize + notifications/initialized.
// ---------------------------------------------------------------------------

const mcpServerName = "github-core-clone"
const mcpServerVersion = "1.0.0"

var mcpSupportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

type mcpToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func mcpTools() []mcpToolDef {
	str := map[string]any{"type": "string"}
	boolean := map[string]any{"type": "boolean"}
	return []mcpToolDef{
		{
			Name:        "create_repo",
			Description: "Create a new repository for the authenticated user.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":        str,
					"description": str,
					"private":     boolean,
				},
				"required": []string{"name"},
			},
		},
		{
			Name:        "get_repo",
			Description: "Get a repository by owner and name.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"owner": str,
					"repo":  str,
				},
				"required": []string{"owner", "repo"},
			},
		},
		{
			Name:        "create_issue",
			Description: "Create an issue in a repository.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"owner": str,
					"repo":  str,
					"title": str,
					"body":  str,
				},
				"required": []string{"owner", "repo", "title"},
			},
		},
		{
			Name:        "list_issues",
			Description: "List issues in a repository.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"owner": str,
					"repo":  str,
					"state": str,
				},
				"required": []string{"owner", "repo"},
			},
		},
		{
			Name:        "create_pull_request",
			Description: "Create a pull request in a repository.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"owner": str,
					"repo":  str,
					"title": str,
					"body":  str,
					"head":  str,
					"base":  str,
				},
				"required": []string{"owner", "repo", "title", "head", "base"},
			},
		},
		{
			Name:        "merge_pull_request",
			Description: "Merge a pull request in a repository.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"owner":      str,
					"repo":       str,
					"pull_number": map[string]any{"type": "number"},
				},
				"required": []string{"owner", "repo", "pull_number"},
			},
		},
	}
}

// jsonRPCResponse writes one JSON-RPC response object.
func jsonRPCResponse(w http.ResponseWriter, status int, id any, result any, rpcErr *jsonRPCError) {
	body := map[string]any{"jsonrpc": "2.0"}
	if id != nil {
		body["id"] = id
	} else {
		body["id"] = nil
	}
	if rpcErr != nil {
		body["error"] = map[string]any{"code": rpcErr.Code, "message": rpcErr.Message}
	} else {
		body["result"] = result
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-GitHub-Request-Id", nextRequestID())
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

type jsonRPCError struct {
	Code    int
	Message string
}

func toolResultJSON(obj any) map[string]any {
	text, _ := json.Marshal(obj)
	return map[string]any{
		"content": []any{
			map[string]any{"type": "text", "text": string(text)},
		},
		"structuredContent": obj,
		"isError":           false,
	}
}

func toolErrorResult(msg string) map[string]any {
	return map[string]any{
		"content": []any{
			map[string]any{"type": "text", "text": msg},
		},
		"isError": true,
	}
}

func argStr(args map[string]any, name string) string {
	if v, ok := args[name]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func argBool(args map[string]any, name string) bool {
	if v, ok := args[name]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

func argNum(args map[string]any, name string) (int, bool) {
	if v, ok := args[name]; ok {
		switch n := v.(type) {
		case float64:
			return int(n), true
		case int:
			return n, true
		case json.Number:
			i, err := n.Int64()
			if err == nil {
				return int(i), true
			}
		}
	}
	return 0, false
}

// callMCPTool executes one tool call against the store. It returns the
// tool result object, or (nil, errMsg) for a tool execution error.
func callMCPTool(name string, args map[string]any) (any, string) {
	s := theStore
	switch name {
	case "create_repo":
		repoName := argStr(args, "name")
		if repoName == "" {
			return nil, "missing required argument: name"
		}
		s.mu.Lock()
		if _, exists := s.repos[viewerLogin+"/"+repoName]; exists {
			s.mu.Unlock()
			return nil, "name already exists on this account"
		}
		repo := &Repo{
			ID: s.nextRepoID, Name: repoName, Owner: viewerLogin,
			Description: argStr(args, "description"), Private: argBool(args, "private"),
			DefaultBranch: "main", Refs: map[string]string{},
			Labels: map[string]*Label{}, nextIssue: 1, nextPull: 1,
		}
		s.nextRepoID++
		s.repos[viewerLogin+"/"+repoName] = repo
		out := repo.toJSON()
		s.mu.Unlock()
		return out, ""
	case "get_repo":
		owner := argStr(args, "owner")
		repoName := argStr(args, "repo")
		s.mu.RLock()
		repo := s.getRepo(owner, repoName)
		if repo == nil {
			s.mu.RUnlock()
			return nil, "Not Found"
		}
		out := repo.toJSON()
		s.mu.RUnlock()
		return out, ""
	case "create_issue":
		owner := argStr(args, "owner")
		repoName := argStr(args, "repo")
		title := argStr(args, "title")
		if owner == "" || repoName == "" || title == "" {
			return nil, "missing required argument"
		}
		s.mu.Lock()
		repo := s.getRepo(owner, repoName)
		if repo == nil {
			s.mu.Unlock()
			return nil, "Not Found"
		}
		iss := &Issue{
			ID: s.nextIssueID, Number: repo.nextIssue, Title: title,
			Body: argStr(args, "body"), State: "open", Author: viewerLogin,
			Seq: uint64(s.nextIssueID),
		}
		s.nextIssueID++
		repo.nextIssue++
		repo.Issues = append(repo.Issues, iss)
		out := s.issueToJSON(repo, iss)
		s.mu.Unlock()
		return out, ""
	case "list_issues":
		owner := argStr(args, "owner")
		repoName := argStr(args, "repo")
		state := argStr(args, "state")
		if state == "" {
			state = "open"
		}
		s.mu.RLock()
		repo := s.getRepo(owner, repoName)
		if repo == nil {
			s.mu.RUnlock()
			return nil, "Not Found"
		}
		var out []issueJSON
		for _, iss := range repo.Issues {
			if state == "all" || iss.State == state {
				out = append(out, s.issueToJSON(repo, iss))
			}
		}
		s.mu.RUnlock()
		if out == nil {
			out = []issueJSON{}
		}
		return out, ""
	case "create_pull_request":
		owner := argStr(args, "owner")
		repoName := argStr(args, "repo")
		title := argStr(args, "title")
		head := argStr(args, "head")
		base := argStr(args, "base")
		if owner == "" || repoName == "" || title == "" || head == "" || base == "" {
			return nil, "missing required argument"
		}
		s.mu.Lock()
		repo := s.getRepo(owner, repoName)
		if repo == nil {
			s.mu.Unlock()
			return nil, "Not Found"
		}
		if _, ok := s.resolveRef(repo, head); !ok {
			s.mu.Unlock()
			return nil, "Head branch " + head + " does not exist"
		}
		if _, ok := s.resolveRef(repo, base); !ok {
			s.mu.Unlock()
			return nil, "Base branch " + base + " does not exist"
		}
		for _, pr := range repo.Pulls {
			if pr.Head == head && pr.Base == base && pr.State == "open" {
				out := s.pullToJSON(repo, pr)
				s.mu.Unlock()
				return out, ""
			}
		}
		pr := &Pull{
			Number: repo.nextPull, Title: title, Body: argStr(args, "body"),
			Head: head, Base: base, State: "open", Seq: uint64(repo.nextPull),
		}
		repo.nextPull++
		repo.Pulls = append(repo.Pulls, pr)
		out := s.pullToJSON(repo, pr)
		s.mu.Unlock()
		return out, ""
	case "merge_pull_request":
		owner := argStr(args, "owner")
		repoName := argStr(args, "repo")
		number, ok := argNum(args, "pull_number")
		if owner == "" || repoName == "" || !ok {
			return nil, "missing required argument"
		}
		s.mu.Lock()
		repo := s.getRepo(owner, repoName)
		if repo == nil {
			s.mu.Unlock()
			return nil, "Not Found"
		}
		pr := findPull(repo, number)
		if pr == nil {
			s.mu.Unlock()
			return nil, "Not Found"
		}
		if pr.State != "open" {
			s.mu.Unlock()
			return nil, "Pull request is not open"
		}
		headTip, ok1 := s.resolveRef(repo, pr.Head)
		baseTip, ok2 := s.resolveRef(repo, pr.Base)
		if !ok1 || !ok2 || len(s.mergeConflict(headTip, baseTip)) > 0 {
			s.mu.Unlock()
			return nil, "Pull request is not mergeable"
		}
		if s.squashMerge(repo, pr) == "" {
			s.mu.Unlock()
			return nil, "Pull request is not mergeable"
		}
		out := s.pullToJSON(repo, pr)
		s.mu.Unlock()
		return out, ""
	}
	return nil, "unknown tool: " + name
}

var mcpToolRequired = map[string][]string{
	"create_repo":        {"name"},
	"get_repo":           {"owner", "repo"},
	"create_issue":       {"owner", "repo", "title"},
	"list_issues":        {"owner", "repo"},
	"create_pull_request": {"owner", "repo", "title", "head", "base"},
	"merge_pull_request": {"owner", "repo", "pull_number"},
}

func handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		// session close (legacy clients): nothing held
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	raw, err := readAllBody(r)
	if err != nil {
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}
	// Parse error: a body that is not JSON -> HTTP 400, JSON-RPC -32700.
	var req map[string]any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&req); err != nil {
		jsonRPCResponse(w, http.StatusBadRequest, nil, nil, &jsonRPCError{Code: -32700, Message: "Parse error"})
		return
	}
	method, _ := req["method"].(string)
	id := req["id"] // may be nil (notification)
	params, _ := req["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}

	// Notifications (no id) are answered with HTTP 202 and no body.
	if id == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	switch method {
	case "server/discover":
		serverInfo := map[string]any{"name": mcpServerName, "version": mcpServerVersion}
		result := map[string]any{
			"supportedVersions": mcpSupportedVersions,
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"serverInfo":        serverInfo,
			"_meta": map[string]any{
				"io.modelcontextprotocol/serverInfo": serverInfo,
			},
		}
		jsonRPCResponse(w, http.StatusOK, id, result, nil)
	case "initialize":
		version := "2026-07-28"
		if pv, ok := params["protocolVersion"].(string); ok && pv != "" {
			for _, v := range mcpSupportedVersions {
				if v == pv {
					version = pv
					break
				}
			}
		}
		result := map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": mcpServerName, "version": mcpServerVersion},
		}
		jsonRPCResponse(w, http.StatusOK, id, result, nil)
	case "tools/list":
		jsonRPCResponse(w, http.StatusOK, id, map[string]any{"tools": mcpTools()}, nil)
	case "tools/call":
		toolName, _ := params["name"].(string)
		args, _ := params["arguments"].(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		known := false
		for _, t := range mcpTools() {
			if t.Name == toolName {
				known = true
				break
			}
		}
		if !known {
			jsonRPCResponse(w, http.StatusOK, id, nil, &jsonRPCError{Code: -32602, Message: fmt.Sprintf("Unknown tool: %s", toolName)})
			return
		}
		// missing required arguments -> JSON-RPC -32602
		for _, reqArg := range mcpToolRequired[toolName] {
			if _, ok := args[reqArg]; !ok {
				jsonRPCResponse(w, http.StatusOK, id, nil, &jsonRPCError{Code: -32602, Message: fmt.Sprintf("Missing required argument: %s", reqArg)})
				return
			}
		}
		result, errMsg := callMCPTool(toolName, args)
		if errMsg != "" {
			jsonRPCResponse(w, http.StatusOK, id, toolErrorResult(errMsg), nil)
			return
		}
		jsonRPCResponse(w, http.StatusOK, id, toolResultJSON(result), nil)
	case "ping":
		jsonRPCResponse(w, http.StatusOK, id, map[string]any{}, nil)
	default:
		jsonRPCResponse(w, http.StatusOK, id, nil, &jsonRPCError{Code: -32601, Message: fmt.Sprintf("Method not found: %s", method)})
	}
}
