package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/grafana/regexp"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/query"
)

const (
	defaultMCPProtocolVersion = "2025-03-26"
	mcpContentTypeJSON        = "application/json"
	mcpContentTypeSSE         = "text/event-stream"
)

var supportedMCPProtocolVersions = []string{
	"2025-11-25",
	"2025-06-18",
	"2025-03-26",
	"2024-11-05",
}

type mcpHandler struct {
	searcher       zoekt.Streamer
	serverName     string
	serverVersion  string
	allowedOrigins map[string]struct{}
}

func newMCPHandler(searcher zoekt.Streamer, serverName, serverVersion string, allowedOrigins []string) http.Handler {
	originSet := make(map[string]struct{}, len(allowedOrigins))
	for _, origin := range allowedOrigins {
		origin = strings.TrimSpace(origin)
		if origin == "" {
			continue
		}
		originSet[origin] = struct{}{}
	}

	return &mcpHandler{
		searcher:       searcher,
		serverName:     serverName,
		serverVersion:  serverVersion,
		allowedOrigins: originSet,
	}
}

func (h *mcpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.isOriginAllowed(r.Header.Get("Origin")) {
		h.writeHTTPError(w, http.StatusForbidden, newMCPErrorResponse(nil, -32000, "forbidden origin", nil))
		return
	}

	switch r.Method {
	case http.MethodGet:
		h.serveSSE(w, r)
	case http.MethodPost:
		h.servePOST(w, r)
	case http.MethodDelete:
		w.WriteHeader(http.StatusMethodNotAllowed)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (h *mcpHandler) serveSSE(w http.ResponseWriter, r *http.Request) {
	if _, ok := validateMCPProtocolVersion(r.Header.Get("MCP-Protocol-Version")); !ok {
		h.writeHTTPError(w, http.StatusBadRequest, newMCPErrorResponse(nil, -32602, "unsupported protocol version", map[string]any{
			"supported": supportedMCPProtocolVersions,
		}))
		return
	}

	w.Header().Set("Content-Type", mcpContentTypeSSE)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	_, _ = w.Write([]byte(": zoekt mcp stream\n\n"))
	flusher.Flush()

	<-r.Context().Done()
}

func (h *mcpHandler) servePOST(w http.ResponseWriter, r *http.Request) {
	version, ok := validateMCPProtocolVersion(r.Header.Get("MCP-Protocol-Version"))
	if !ok {
		h.writeHTTPError(w, http.StatusBadRequest, newMCPErrorResponse(nil, -32602, "unsupported protocol version", map[string]any{
			"supported": supportedMCPProtocolVersions,
		}))
		return
	}

	body := http.MaxBytesReader(w, r.Body, 1<<20)
	defer body.Close()

	payload, err := ioReadAll(body)
	if err != nil {
		h.writeHTTPError(w, http.StatusBadRequest, newMCPErrorResponse(nil, -32700, "failed to read request body", nil))
		return
	}

	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 {
		h.writeHTTPError(w, http.StatusBadRequest, newMCPErrorResponse(nil, -32700, "empty request body", nil))
		return
	}

	var responses []mcpResponse
	if payload[0] == '[' {
		var messages []json.RawMessage
		if err := json.Unmarshal(payload, &messages); err != nil {
			h.writeHTTPError(w, http.StatusBadRequest, newMCPErrorResponse(nil, -32700, "invalid JSON", nil))
			return
		}
		for _, raw := range messages {
			resp, shouldRespond := h.handleMessage(r.Context(), version, raw)
			if shouldRespond {
				responses = append(responses, resp)
			}
		}
		if len(responses) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		h.writeJSON(w, http.StatusOK, version, responses)
		return
	}

	resp, shouldRespond := h.handleMessage(r.Context(), version, payload)
	if !shouldRespond {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if negotiated, ok := negotiatedVersionFromResult(resp.Result); ok {
		version = negotiated
	}
	h.writeJSON(w, http.StatusOK, version, resp)
}

func (h *mcpHandler) handleMessage(ctx context.Context, version string, raw json.RawMessage) (mcpResponse, bool) {
	var req mcpRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return newMCPErrorResponse(nil, -32700, "invalid JSON", nil), true
	}

	if req.Method == "" {
		if req.isResponse() {
			return mcpResponse{}, false
		}
		return newMCPErrorResponse(req.responseID(), -32600, "invalid request", nil), req.hasID()
	}

	if req.JSONRPC != "" && req.JSONRPC != "2.0" {
		return newMCPErrorResponse(req.responseID(), -32600, "invalid request", nil), req.hasID()
	}

	switch req.Method {
	case "initialize":
		return h.handleInitialize(req)
	case "notifications/initialized":
		return mcpResponse{}, false
	case "ping":
		if !req.hasID() {
			return mcpResponse{}, false
		}
		return newMCPResultResponse(req.ID, map[string]any{}), true
	case "tools/list":
		if !req.hasID() {
			return mcpResponse{}, false
		}
		return newMCPResultResponse(req.ID, map[string]any{
			"tools": h.tools(version),
		}), true
	case "tools/call":
		if !req.hasID() {
			return mcpResponse{}, false
		}
		result, err := h.handleToolCall(ctx, req.Params)
		if err != nil {
			return newMCPErrorResponse(req.ID, -32602, err.Error(), nil), true
		}
		return newMCPResultResponse(req.ID, result), true
	default:
		if !req.hasID() {
			return mcpResponse{}, false
		}
		return newMCPErrorResponse(req.ID, -32601, fmt.Sprintf("method not found: %s", req.Method), nil), true
	}
}

func (h *mcpHandler) handleInitialize(req mcpRequest) (mcpResponse, bool) {
	if !req.hasID() {
		return mcpResponse{}, false
	}

	var params struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    map[string]any `json:"capabilities"`
		ClientInfo      map[string]any `json:"clientInfo"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return newMCPErrorResponse(req.ID, -32602, "invalid initialize params", nil), true
		}
	}
	if params.ProtocolVersion == "" {
		return newMCPErrorResponse(req.ID, -32602, "missing protocolVersion", nil), true
	}

	version := negotiateMCPProtocolVersion(params.ProtocolVersion)
	return newMCPResultResponse(req.ID, map[string]any{
		"protocolVersion": version,
		"capabilities": map[string]any{
			"tools": map[string]any{
				"listChanged": false,
			},
		},
		"serverInfo": map[string]any{
			"name":    h.serverName,
			"version": h.serverVersion,
		},
		"instructions": "Use list_repos to discover repository filters and use search for code search. Regex queries are preferred.",
	}), true
}

func (h *mcpHandler) handleToolCall(ctx context.Context, params json.RawMessage) (mcpToolResult, error) {
	var req struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return mcpToolResult{}, fmt.Errorf("invalid tool call params")
	}
	switch req.Name {
	case "list_repos":
		return h.callListRepos(ctx)
	case "search":
		var args struct {
			Query  string `json:"query"`
			Prefix string `json:"prefix"`
		}
		if len(req.Arguments) > 0 {
			if err := json.Unmarshal(req.Arguments, &args); err != nil {
				return mcpToolResult{}, fmt.Errorf("invalid arguments for search")
			}
		}
		if strings.TrimSpace(args.Query) == "" {
			return mcpToolResult{}, fmt.Errorf("missing required argument: query")
		}
		return h.callSearch(ctx, args.Query, args.Prefix), nil
	default:
		return mcpToolResult{}, fmt.Errorf("unknown tool: %s", req.Name)
	}
}

func (h *mcpHandler) callListRepos(ctx context.Context) (mcpToolResult, error) {
	repos, err := h.searcher.List(ctx, &query.Const{Value: true}, &zoekt.ListOptions{Field: zoekt.RepoListFieldRepos})
	if err != nil {
		return mcpToolResult{
			Content: []mcpToolTextContent{{
				Type: "text",
				Text: fmt.Sprintf("Error listing repos: %v", err),
			}},
			IsError: true,
		}, nil
	}

	if repos == nil {
		return mcpToolResult{
			Content: []mcpToolTextContent{{Type: "text", Text: "No repositories found."}},
		}, nil
	}

	names := make([]string, 0, len(repos.Repos))
	for _, repo := range repos.Repos {
		names = append(names, repo.Repository.Name)
	}
	sort.Strings(names)

	if len(names) == 0 {
		return mcpToolResult{
			Content: []mcpToolTextContent{{Type: "text", Text: "No repositories found."}},
		}, nil
	}

	return mcpToolResult{
		Content: []mcpToolTextContent{{
			Type: "text",
			Text: "Available Repositories:\n" + strings.Join(names, "\n"),
		}},
	}, nil
}

func (h *mcpHandler) callSearch(ctx context.Context, queryText, prefix string) mcpToolResult {
	fullQuery := strings.TrimSpace(strings.Join([]string{strings.TrimSpace(prefix), strings.TrimSpace(queryText)}, " "))
	q, err := query.Parse(fullQuery)
	if err != nil {
		return mcpToolResult{
			Content: []mcpToolTextContent{{
				Type: "text",
				Text: fmt.Sprintf("Error searching zoekt: %v", err),
			}},
			IsError: true,
		}
	}

	opts := &zoekt.SearchOptions{
		MaxWallTime:          10 * time.Second,
		MaxDocDisplayCount:   50,
		MaxMatchDisplayCount: 200,
	}
	opts.SetDefaults()

	result, err := h.searcher.Search(ctx, q, opts)
	if err != nil {
		return mcpToolResult{
			Content: []mcpToolTextContent{{
				Type: "text",
				Text: fmt.Sprintf("Error searching zoekt: %v", err),
			}},
			IsError: true,
		}
	}

	if result == nil || len(result.Files) == 0 {
		return mcpToolResult{
			Content: []mcpToolTextContent{{
				Type: "text",
				Text: fmt.Sprintf("No results found for query: %s", fullQuery),
			}},
		}
	}

	reposByName, err := repoMetadataByName(ctx, h.searcher, result.Files)
	if err != nil {
		return mcpToolResult{
			Content: []mcpToolTextContent{{
				Type: "text",
				Text: fmt.Sprintf("Error resolving local file paths: %v", err),
			}},
			IsError: true,
		}
	}

	var output strings.Builder
	fmt.Fprintf(&output, "Found %d matches in %d files (Query: %s).\n\n", result.Stats.MatchCount, result.Stats.FileCount, fullQuery)

	for _, file := range result.Files {
		fileName := file.FileName
		if repo := reposByName[file.Repository]; repo != nil {
			if abs := zoekt.ResolveFileSystemPath(repo, file.FileName); abs != "" {
				fileName = abs
			}
		}

		fmt.Fprintf(&output, "File: %s (Repo: %s)\n", fileName, file.Repository)
		for _, match := range file.LineMatches {
			line := strings.TrimRight(string(match.Line), "\r\n")
			fmt.Fprintf(&output, "%d: %s\n", match.LineNumber, line)
		}
		output.WriteByte('\n')
	}

	return mcpToolResult{
		Content: []mcpToolTextContent{{
			Type: "text",
			Text: strings.TrimRight(output.String(), "\n"),
		}},
	}
}

func (h *mcpHandler) tools(version string) []map[string]any {
	tools := []map[string]any{
		{
			"name":        "list_repos",
			"description": "List all repositories available on the Zoekt server. Use this to find r:repo_name filters.",
			"inputSchema": map[string]any{
				"type":                 "object",
				"properties":           map[string]any{},
				"additionalProperties": false,
			},
		},
		{
			"name":        "search",
			"description": "Search code using Zoekt. Strongly prefer regex queries. To filter by repo, use prefix like r:my-repo.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "The search query. Prefer regex for fuzzy matching, for example '(init|setup).+context'.",
					},
					"prefix": map[string]any{
						"type":        "string",
						"description": "Optional prefix to prepend to the query, for example 'r:my-repo'.",
					},
				},
				"required":             []string{"query"},
				"additionalProperties": false,
			},
		},
	}

	if slices.Contains(supportedMCPProtocolVersions[:2], version) {
		for _, tool := range tools {
			tool["annotations"] = map[string]any{
				"readOnlyHint": true,
			}
		}
	}

	return tools
}

func repoMetadataByName(ctx context.Context, searcher zoekt.Searcher, files []zoekt.FileMatch) (map[string]*zoekt.Repository, error) {
	repoNames := map[string]struct{}{}
	for _, file := range files {
		if file.Repository == "" {
			continue
		}
		repoNames[file.Repository] = struct{}{}
	}

	if len(repoNames) == 0 {
		return nil, nil
	}

	qs := make([]query.Q, 0, len(repoNames))
	for repoName := range repoNames {
		repoRe, err := regexp.Compile("^" + regexp.QuoteMeta(repoName) + "$")
		if err != nil {
			return nil, err
		}
		qs = append(qs, &query.Repo{Regexp: repoRe})
	}

	repoList, err := searcher.List(ctx, query.NewOr(qs...), &zoekt.ListOptions{Field: zoekt.RepoListFieldRepos})
	if err != nil || repoList == nil {
		return nil, err
	}

	repos := make(map[string]*zoekt.Repository, len(repoList.Repos))
	for _, entry := range repoList.Repos {
		repo := entry.Repository
		repos[repo.Name] = &repo
	}

	return repos, nil
}

func (h *mcpHandler) isOriginAllowed(origin string) bool {
	if origin == "" {
		return true
	}
	if _, ok := h.allowedOrigins[origin]; ok {
		return true
	}

	u, err := url.Parse(origin)
	if err != nil {
		return false
	}

	switch strings.ToLower(u.Hostname()) {
	case "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}

func validateMCPProtocolVersion(version string) (string, bool) {
	if version == "" {
		return defaultMCPProtocolVersion, true
	}
	for _, supported := range supportedMCPProtocolVersions {
		if version == supported {
			return version, true
		}
	}
	return "", false
}

func negotiateMCPProtocolVersion(requested string) string {
	if version, ok := validateMCPProtocolVersion(requested); ok {
		return version
	}
	return supportedMCPProtocolVersions[0]
}

func (h *mcpHandler) writeJSON(w http.ResponseWriter, status int, version string, payload any) {
	w.Header().Set("Content-Type", mcpContentTypeJSON)
	w.Header().Set("MCP-Protocol-Version", version)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func (h *mcpHandler) writeHTTPError(w http.ResponseWriter, status int, payload mcpResponse) {
	w.Header().Set("Content-Type", mcpContentTypeJSON)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

func (r mcpRequest) hasID() bool {
	return len(bytes.TrimSpace(r.ID)) > 0
}

func (r mcpRequest) responseID() json.RawMessage {
	if r.hasID() {
		return r.ID
	}
	return nil
}

func (r mcpRequest) isResponse() bool {
	return r.Method == "" && (len(r.Result) > 0 || r.Error != nil)
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type mcpToolResult struct {
	Content []mcpToolTextContent `json:"content"`
	IsError bool                 `json:"isError,omitempty"`
}

type mcpToolTextContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func newMCPResultResponse(id json.RawMessage, result any) mcpResponse {
	return mcpResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
}

func newMCPErrorResponse(id json.RawMessage, code int, message string, data any) mcpResponse {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return mcpResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &mcpError{
			Code:    code,
			Message: message,
			Data:    data,
		},
	}
}

func negotiatedVersionFromResult(result any) (string, bool) {
	if result == nil {
		return "", false
	}
	resultMap, ok := result.(map[string]any)
	if !ok {
		return "", false
	}
	version, ok := resultMap["protocolVersion"].(string)
	if !ok || version == "" {
		return "", false
	}
	return version, true
}

func ioReadAll(body io.Reader) ([]byte, error) {
	return io.ReadAll(body)
}
