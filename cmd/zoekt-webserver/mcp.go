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

const mcpSearchDescription = `Search indexed source code using the native Zoekt query language. Prefer precise regular expressions.

Repository and path filters:
- Use list_repos to discover repository names. repo:<regex> (alias r:) matches repository names; anchor and escape the name for an exact match, e.g. r:^zoekt$.
- file:<regex> (alias f:) filters the entire indexed relative path, including directories and the filename. It does not match the displayed absolute filesystem path. The local index root itself is omitted: if D:/TnT is indexed with filenames such as Code/main.cpp, use file:^Code/, not file:TnT/Code/.
- Indexed paths normally use /, including local-file indexes on Windows. / is a literal regex character and needs no escaping. A backslash is a regex escape, not an interchangeable path separator; do not paste Windows filesystem paths into file:. An unquoted [\\/] matches either stored separator if an index retains backslashes.
- Path matching is unanchored unless you add anchors. file:TnT/Code matches any path containing that text, including TnT/CodeExtra/. Use file:TnT/Code/ for a trailing directory boundary, file:^TnT/Code/ for a directory at the index root, or file:(^|/)TnT/Code/ for complete directory names at any depth.
- Use file:, not path: or dir:, which are not supported filters. Use type:filename to inspect indexed relative paths when a path filter unexpectedly returns no results.

Content and regex syntax:
- content:<regex> (alias c:) searches file contents only. A bare pattern or regex:<regex> searches both paths and contents. file: alone finds matching paths; add content: to search within those files, e.g. file:TnT/Code/ content:Render.
- Patterns use Go/RE2-style regex syntax, not filesystem globs. Write foo.*bar, not /foo.*bar/: enclosing slashes are matched literally. Use ^ and $ as anchors, | for regex alternatives, and [.] or an unquoted \. for a literal dot, e.g. file:[.]cpp$.
- Double quotes group text containing spaces; they do not make regex metacharacters literal. Quoted text consumes one level of backslash escaping. Prefer unquoted regexes when no spaces are needed. Examples here show query text after JSON decoding; JSON string escaping is a separate layer.
- case:auto is the default: a pattern containing uppercase letters is case-sensitive. Add case:no for case-insensitive matching or case:yes to require exact case.

Boolean operators and prefix:
- Spaces mean AND, lowercase or means OR, and - negates the following term or group. Group alternatives with parentheses. The words and, not, AND, OR, and NOT are not Boolean operators.
- prefix is prepended to query as text with a space; it is not a separately enforced scope. Group alternatives in both arguments so repo/path filters apply to every branch. file:^src/ content:foo or content:bar means (file:^src/ AND content:foo) OR content:bar, allowing bar outside src/.
- Safe example: prefix="r:^zoekt$ file:^query/", query="(content:Parse or content:RegexpQuery)". With no prefix, write r:^zoekt$ file:^query/ (content:Parse or content:RegexpQuery).

Other supported filters include lang:go, sym:Name, branch:main (alias b:), archived:yes/no, fork:yes/no, public:yes/no, and meta.<field>:<regex>. type:filename (alias type:file) returns path matches; type:repo returns repositories; type:filematch is the default. Results are limited, so refine broad searches with repository, path, and content filters.`

var supportedMCPProtocolVersions = []string{
	"2025-11-25",
	"2025-06-18",
	"2025-03-26",
	"2024-11-05",
}

type mcpHandler struct {
	searcher         zoekt.Streamer
	serverName       string
	serverVersion    string
	allowedOrigins   map[string]struct{}
	maxResponseBytes int
}

func newMCPHandler(searcher zoekt.Streamer, serverName, serverVersion string, allowedOrigins []string) *mcpHandler {
	originSet := make(map[string]struct{}, len(allowedOrigins))
	for _, origin := range allowedOrigins {
		origin = strings.TrimSpace(origin)
		if origin == "" {
			continue
		}
		originSet[origin] = struct{}{}
	}

	return &mcpHandler{
		searcher:         searcher,
		serverName:       serverName,
		serverVersion:    serverVersion,
		allowedOrigins:   originSet,
		maxResponseBytes: defaultMCPMaxResponseBytes,
	}
}

func (h *mcpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(context.WithValue(r.Context(), mcpRequestStart{}, time.Now()))
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
		"instructions": "Use list_repos for exact repository names. Use search with a regex pattern, repos and optional path_regex: files searches paths, snippets searches content with context. It defaults to case-insensitive matching. Paths use / and are relative to the index root. Use search_raw for native Zoekt query/prefix syntax and file-level Boolean combinations; it retains native case:auto and text-prefix semantics. Check completion and truncation status before concluding a result is absent.",
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
		args, q, err := parseMCPSearchArgs(req.Arguments)
		if err != nil {
			return mcpToolResult{}, err
		}
		return h.callAgentSearch(ctx, args, q), nil
	case "search_raw":
		var args struct {
			Query  string `json:"query"`
			Prefix string `json:"prefix"`
		}
		if len(req.Arguments) > 0 {
			if err := json.Unmarshal(req.Arguments, &args); err != nil {
				return mcpToolResult{}, fmt.Errorf("invalid arguments for search_raw")
			}
		}
		if strings.TrimSpace(args.Query) == "" {
			return mcpToolResult{}, fmt.Errorf("missing required argument: query")
		}
		return h.callSearchRaw(ctx, args.Query, args.Prefix), nil
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

func (h *mcpHandler) callSearchRaw(ctx context.Context, queryText, prefix string) mcpToolResult {
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
			"description": "List all repositories available on the Zoekt server. Use these names with repo:<regex> (alias r:); anchor and escape a name for exact matching, e.g. r:^zoekt$.",
			"inputSchema": map[string]any{
				"type":                 "object",
				"properties":           map[string]any{},
				"additionalProperties": false,
			},
		},
		mcpAgentSearchTool(),
		{
			"name":        "search_raw",
			"description": mcpSearchDescription,
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "A native Zoekt query. Use file:<regex> for indexed relative paths and content:<regex> for contents, e.g. 'file:TnT/Code/ content:Render'. Use / for normalized paths, without /regex/ delimiters. Group Boolean alternatives, e.g. '(content:init or content:setup)', when combined with filters or prefix. See the tool description for quoting, case sensitivity, and other filters.",
					},
					"prefix": map[string]any{
						"type":        "string",
						"description": "Optional native Zoekt expressions prepended verbatim to query with a space, e.g. 'r:^zoekt$ file:^query/'. This is text composition, not a separately enforced filter: parenthesize any or alternatives in both prefix and query to keep repository/path restrictions on every branch. Example: prefix 'r:^zoekt$ file:^query/' with query '(content:Parse or content:RegexpQuery)'.",
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
	// Encode before writing any bytes so a deadline cannot produce half a JSON
	// response. The new search formatter already bounds the escaped text size.
	encoded, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "failed to encode response", http.StatusInternalServerError)
		return
	}
	refresh := func(response *mcpResponse) bool {
		result, ok := response.Result.(mcpToolResult)
		if !ok || result.searchStatus == nil || time.Now().Before(result.searchDeadline) {
			return false
		}
		result.searchStatus.reason("timeout", true, false)
		text := result.Content[0].Text
		if end := strings.IndexByte(text, '\n'); end >= 0 {
			text = text[end+1:]
		}
		result.Content[0].Text = result.searchStatus.header() + text
		response.Result = result
		return true
	}
	changed := false
	switch response := payload.(type) {
	case mcpResponse:
		changed = refresh(&response)
		payload = response
	case []mcpResponse:
		for i := range response {
			if refresh(&response[i]) {
				changed = true
			}
		}
		payload = response
	}
	if changed {
		encoded, _ = json.Marshal(payload)
	}
	w.Header().Set("Content-Type", mcpContentTypeJSON)
	w.Header().Set("MCP-Protocol-Version", version)
	w.WriteHeader(status)
	_, _ = w.Write(append(encoded, '\n'))
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
	Content        []mcpToolTextContent `json:"content"`
	IsError        bool                 `json:"isError,omitempty"`
	searchStatus   *mcpSearchStatus
	searchDeadline time.Time
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
