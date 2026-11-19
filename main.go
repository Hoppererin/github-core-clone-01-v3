package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var theStore = NewStore()

func readAllBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	return io.ReadAll(r.Body)
}

// ---------------------------------------------------------------------------
// Auth: any non-empty Bearer token is accepted; missing/empty -> 401 JSON.
// ---------------------------------------------------------------------------

func authorized(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return false
	}
	return strings.TrimSpace(h[len("Bearer "):]) != ""
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-GitHub-Request-Id", nextRequestID())
	w.WriteHeader(http.StatusUnauthorized)
	io.WriteString(w, `{"message":"Requires authentication"}`+"\n")
}

func jsonNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-GitHub-Request-Id", nextRequestID())
	w.WriteHeader(http.StatusNotFound)
	io.WriteString(w, `{"message":"Not Found"}`+"\n")
}

// ---------------------------------------------------------------------------
// Router
// ---------------------------------------------------------------------------

// splitPath splits an escaped URL path into decoded segments.
func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	raw := strings.Split(p, "/")
	out := make([]string, 0, len(raw))
	for _, seg := range raw {
		if s, err := url.PathUnescape(seg); err == nil {
			out = append(out, s)
		} else {
			out = append(out, seg)
		}
	}
	return out
}

func serveHTTP(w http.ResponseWriter, r *http.Request) {
	// Every reply carries a non-empty X-GitHub-Request-Id.
	w.Header().Set("X-GitHub-Request-Id", nextRequestID())

	path := r.URL.EscapedPath()
	segments := splitPath(path)

	// health check (no auth)
	if len(segments) == 1 && segments[0] == "healthz" && r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"ok":true}`+"\n")
		return
	}

	// GraphQL
	if len(segments) == 2 && segments[0] == "api" && segments[1] == "graphql" {
		if !authorized(r) {
			unauthorized(w)
			return
		}
		handleGraphQL(w, r)
		return
	}

	// MCP
	if len(segments) == 1 && segments[0] == "mcp" {
		if !authorized(r) {
			unauthorized(w)
			return
		}
		handleMCP(w, r)
		return
	}

	// git smart HTTP: /{owner}/{repo}.git/{info/refs|git-upload-pack|git-receive-pack}
	if len(segments) == 3 && strings.HasSuffix(segments[1], ".git") {
		if !authorized(r) {
			unauthorized(w)
			return
		}
		owner := segments[0]
		repoName := strings.TrimSuffix(segments[1], ".git")
		switch segments[2] {
		case "info":
			// /info/refs is handled below via query; fall through
		case "git-upload-pack":
			if r.Method == http.MethodPost {
				handleUploadPack(w, r, owner, repoName)
				return
			}
		case "git-receive-pack":
			if r.Method == http.MethodPost {
				handleReceivePack(w, r, owner, repoName)
				return
			}
		}
	}
	if len(segments) == 4 && segments[2] == "info" && segments[3] == "refs" && strings.HasSuffix(segments[1], ".git") {
		if !authorized(r) {
			unauthorized(w)
			return
		}
		handleGitInfoRefs(w, r, segments[0], strings.TrimSuffix(segments[1], ".git"))
		return
	}

	// REST v3
	if len(segments) >= 2 && segments[0] == "api" && segments[1] == "v3" {
		if !authorized(r) {
			unauthorized(w)
			return
		}
		handleREST(w, r, segments[2:])
		return
	}

	jsonNotFound(w)
}

func baseURLOf(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func handleREST(w http.ResponseWriter, r *http.Request, rest []string) {
	baseURL := baseURLOf(r)
	if len(rest) == 0 {
		// GET /api/v3/
		if r.Method == http.MethodGet {
			handleRestRoot(w, r)
			return
		}
		jsonNotFound(w)
		return
	}
	switch rest[0] {
	case "meta":
		if r.Method == http.MethodGet {
			handleRestMeta(w, r)
			return
		}
	case "user":
		if len(rest) == 1 {
			if r.Method == http.MethodGet {
				handleRestUser(w, r)
				return
			}
		}
		if len(rest) == 2 && rest[1] == "repos" {
			handleUserRepos(w, r, baseURL)
			return
		}
	case "repos":
		if len(rest) >= 3 {
			handleRepoRoutes(w, r, rest[1], rest[2], rest[3:], baseURL)
			return
		}
	}
	jsonNotFound(w)
}

func handleRepoRoutes(w http.ResponseWriter, r *http.Request, owner, name string, tail []string, baseURL string) {
	if len(tail) == 0 {
		handleRepo(w, r, owner, name)
		return
	}
	switch tail[0] {
	case "branches":
		if len(tail) == 1 {
			handleBranches(w, r, owner, name)
			return
		}
		if len(tail) == 2 {
			handleBranch(w, r, owner, name, tail[1])
			return
		}
	case "git":
		if len(tail) >= 2 && tail[1] == "refs" {
			handleGitRefs(w, r, owner, name, strings.Join(tail[2:], "/"))
			return
		}
	case "commits":
		if len(tail) == 1 {
			handleCommits(w, r, owner, name)
			return
		}
	case "contents":
		handleContents(w, r, owner, name, strings.Join(tail[1:], "/"))
		return
	case "labels":
		if len(tail) == 1 {
			handleLabels(w, r, owner, name)
			return
		}
	case "issues":
		if len(tail) == 1 {
			handleIssues(w, r, owner, name, baseURL)
			return
		}
		if n, err := strconv.Atoi(tail[1]); err == nil {
			if len(tail) == 2 {
				handleIssue(w, r, owner, name, n)
				return
			}
			if len(tail) == 3 && tail[2] == "comments" {
				handleIssueComments(w, r, owner, name, n)
				return
			}
		}
	case "pulls":
		if len(tail) == 1 {
			handlePulls(w, r, owner, name, baseURL)
			return
		}
		if n, err := strconv.Atoi(tail[1]); err == nil {
			if len(tail) == 2 {
				handlePull(w, r, owner, name, n)
				return
			}
			if len(tail) == 3 && tail[2] == "files" {
				handlePullFiles(w, r, owner, name, n)
				return
			}
			if len(tail) == 3 && tail[2] == "merge" {
				if r.Method == http.MethodPut {
					handlePullMerge(w, r, owner, name, n)
					return
				}
			}
		}
	}
	jsonNotFound(w)
}

// ---------------------------------------------------------------------------
// Lifecycle: register with the env, serve, poll /done, exit 0 when done.
// ---------------------------------------------------------------------------

func main() {
	log.SetFlags(0)
	envURL := strings.TrimSuffix(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" {
		log.Fatal("MAKEFUN_ENV_URL is not set")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	log.Printf("forge listening on 127.0.0.1:%d", port)

	server := &http.Server{Handler: http.HandlerFunc(serveHTTP)}
	go func() {
		if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("serve: %v", err)
		}
	}()

	// register: POST {"port": N} to $MAKEFUN_ENV_URL/register (first wins)
	client := &http.Client{Timeout: 5 * time.Second}
	registered := false
	for i := 0; i < 240; i++ {
		body, _ := json.Marshal(map[string]int{"port": port})
		resp, err := client.Post(envURL+"/register", "application/json", bytes.NewReader(body))
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 || resp.StatusCode == http.StatusConflict {
				registered = true
				break
			}
			log.Printf("register: HTTP %d, retrying", resp.StatusCode)
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !registered {
		log.Fatal("could not register with the env")
	}
	log.Printf("registered with %s", envURL)

	// poll /done with exponential backoff 250ms -> 2000ms
	interval := 250 * time.Millisecond
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	doneCh := make(chan struct{})
	go func() {
		for {
			time.Sleep(interval)
			if interval < 2000*time.Millisecond {
				interval *= 2
				if interval > 2000*time.Millisecond {
					interval = 2000 * time.Millisecond
				}
			}
			resp, err := client.Get(envURL + "/done")
			if err != nil {
				continue
			}
			var d struct {
				Done bool `json:"done"`
			}
			decErr := json.NewDecoder(resp.Body).Decode(&d)
			resp.Body.Close()
			if decErr == nil && d.Done {
				close(doneCh)
				return
			}
		}
	}()
	select {
	case <-doneCh:
		log.Print("done: exiting 0")
	case sig := <-sigCh:
		log.Printf("signal %v: exiting 0", sig)
	}
	server.Close()
	os.Exit(0)
}
