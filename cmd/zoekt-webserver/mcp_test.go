package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/query"
)

func TestMCPInitializeAndToolsList(t *testing.T) {
	handler := newMCPHandler(fakeStreamer{}, "zoekt-webserver", "dev", nil)

	initResp := postMCP(t, handler, `{
		"jsonrpc":"2.0",
		"id":1,
		"method":"initialize",
		"params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}
	}`)
	if got := initResp.Code; got != http.StatusOK {
		t.Fatalf("initialize status=%d want %d", got, http.StatusOK)
	}
	if got := initResp.Header().Get("MCP-Protocol-Version"); got != "2025-06-18" {
		t.Fatalf("initialize MCP-Protocol-Version=%q want %q", got, "2025-06-18")
	}

	var initBody struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	decodeBody(t, initResp, &initBody)
	if initBody.Result.ProtocolVersion != "2025-06-18" {
		t.Fatalf("protocolVersion=%q want %q", initBody.Result.ProtocolVersion, "2025-06-18")
	}

	listResp := postMCPWithVersion(t, handler, "2025-06-18", `{
		"jsonrpc":"2.0",
		"id":2,
		"method":"tools/list",
		"params":{}
	}`)
	if got := listResp.Code; got != http.StatusOK {
		t.Fatalf("tools/list status=%d want %d", got, http.StatusOK)
	}

	var listBody struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	decodeBody(t, listResp, &listBody)
	if len(listBody.Result.Tools) != 2 {
		t.Fatalf("tool count=%d want 2", len(listBody.Result.Tools))
	}
	if listBody.Result.Tools[0].Name != "list_repos" || listBody.Result.Tools[1].Name != "search" {
		t.Fatalf("unexpected tools=%v", listBody.Result.Tools)
	}
}

func TestMCPListReposToolCall(t *testing.T) {
	handler := newMCPHandler(fakeStreamer{
		listResult: &zoekt.RepoList{
			Repos: []*zoekt.RepoListEntry{
				{Repository: zoekt.Repository{Name: "repo-b"}},
				{Repository: zoekt.Repository{Name: "repo-a"}},
			},
		},
	}, "zoekt-webserver", "dev", nil)

	resp := postMCPWithVersion(t, handler, "2025-03-26", `{
		"jsonrpc":"2.0",
		"id":1,
		"method":"tools/call",
		"params":{"name":"list_repos","arguments":{}}
	}`)
	if got := resp.Code; got != http.StatusOK {
		t.Fatalf("tools/call status=%d want %d", got, http.StatusOK)
	}

	var body struct {
		Result mcpToolResult `json:"result"`
	}
	decodeBody(t, resp, &body)

	if body.Result.IsError {
		t.Fatalf("expected non-error result")
	}
	if len(body.Result.Content) != 1 {
		t.Fatalf("content len=%d want 1", len(body.Result.Content))
	}
	want := "Available Repositories:\nrepo-a\nrepo-b"
	if got := body.Result.Content[0].Text; got != want {
		t.Fatalf("content=%q want %q", got, want)
	}
}

func TestMCPSearchToolCall(t *testing.T) {
	handler := newMCPHandler(fakeStreamer{
		searchResult: &zoekt.SearchResult{
			Stats: zoekt.Stats{
				FileCount:  1,
				MatchCount: 2,
			},
			Files: []zoekt.FileMatch{
				{
					FileName:   "main.go",
					Repository: "repo-a",
					LineMatches: []zoekt.LineMatch{
						{LineNumber: 12, Line: []byte("func initContext() {}\n")},
						{LineNumber: 18, Line: []byte("setupContext()\n")},
					},
				},
			},
		},
	}, "zoekt-webserver", "dev", nil)

	resp := postMCPWithVersion(t, handler, "2025-03-26", `{
		"jsonrpc":"2.0",
		"id":1,
		"method":"tools/call",
		"params":{"name":"search","arguments":{"query":"init","prefix":"r:repo-a"}}
	}`)
	if got := resp.Code; got != http.StatusOK {
		t.Fatalf("tools/call status=%d want %d", got, http.StatusOK)
	}

	var body struct {
		Result mcpToolResult `json:"result"`
	}
	decodeBody(t, resp, &body)

	if body.Result.IsError {
		t.Fatalf("expected non-error result")
	}
	text := body.Result.Content[0].Text
	for _, want := range []string{
		"Found 2 matches in 1 files (Query: r:repo-a init).",
		"File: main.go (Repo: repo-a)",
		"12: func initContext() {}",
		"18: setupContext()",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("result missing %q in %q", want, text)
		}
	}
}

func TestMCPSearchToolCallResolvesLocalFilePath(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Engine")
	repo := zoekt.Repository{Name: "repo-a"}
	if err := zoekt.SetFileSystemRoot(&repo, root); err != nil {
		t.Fatal(err)
	}

	handler := newMCPHandler(fakeStreamer{
		searchResult: &zoekt.SearchResult{
			Stats: zoekt.Stats{
				FileCount:  1,
				MatchCount: 1,
			},
			Files: []zoekt.FileMatch{
				{
					FileName:   "src/main.go",
					Repository: "repo-a",
					LineMatches: []zoekt.LineMatch{
						{LineNumber: 12, Line: []byte("func initContext() {}\n")},
					},
				},
			},
		},
		listResult: &zoekt.RepoList{
			Repos: []*zoekt.RepoListEntry{
				{Repository: repo},
			},
		},
	}, "zoekt-webserver", "dev", nil)

	resp := postMCPWithVersion(t, handler, "2025-03-26", `{
		"jsonrpc":"2.0",
		"id":1,
		"method":"tools/call",
		"params":{"name":"search","arguments":{"query":"init","prefix":"r:repo-a"}}
	}`)
	if got := resp.Code; got != http.StatusOK {
		t.Fatalf("tools/call status=%d want %d", got, http.StatusOK)
	}

	var body struct {
		Result mcpToolResult `json:"result"`
	}
	decodeBody(t, resp, &body)

	if body.Result.IsError {
		t.Fatalf("expected non-error result")
	}

	want := filepath.Join(root, "src", "main.go")
	if text := body.Result.Content[0].Text; !strings.Contains(text, "File: "+want+" (Repo: repo-a)") {
		t.Fatalf("result missing absolute path %q in %q", want, text)
	}
}

func TestMCPRejectsInvalidOrigin(t *testing.T) {
	handler := newMCPHandler(fakeStreamer{}, "zoekt-webserver", "dev", nil)

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("MCP-Protocol-Version", "2025-03-26")
	resp := httptest.NewRecorder()

	handler.ServeHTTP(resp, req)

	if got := resp.Code; got != http.StatusForbidden {
		t.Fatalf("status=%d want %d", got, http.StatusForbidden)
	}
}

type fakeStreamer struct {
	searchResult *zoekt.SearchResult
	searchErr    error
	listResult   *zoekt.RepoList
	listErr      error
}

func (f fakeStreamer) Search(_ context.Context, _ query.Q, _ *zoekt.SearchOptions) (*zoekt.SearchResult, error) {
	return f.searchResult, f.searchErr
}

func (f fakeStreamer) StreamSearch(context.Context, query.Q, *zoekt.SearchOptions, zoekt.Sender) error {
	return nil
}

func (f fakeStreamer) List(_ context.Context, _ query.Q, _ *zoekt.ListOptions) (*zoekt.RepoList, error) {
	return f.listResult, f.listErr
}

func (fakeStreamer) Close() {}

func (fakeStreamer) String() string { return "fakeStreamer" }

func postMCP(t *testing.T, handler http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	return postMCPWithVersion(t, handler, "", body)
}

func postMCPWithVersion(t *testing.T, handler http.Handler, version, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	if version != "" {
		req.Header.Set("MCP-Protocol-Version", version)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	return resp
}

func decodeBody(t *testing.T, resp *httptest.ResponseRecorder, out any) {
	t.Helper()
	if err := json.Unmarshal(resp.Body.Bytes(), out); err != nil {
		t.Fatalf("decode body: %v", err)
	}
}
