package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/internal/searchtest"
	"github.com/sourcegraph/zoekt/query"
)

func callMCPHTTP(t testing.TB, endpoint, tool string, args any) (mcpToolResult, *mcpError) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", endpoint, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("MCP-Protocol-Version", defaultMCPProtocolVersion)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("HTTP %d", res.StatusCode)
	}
	var body struct {
		Result mcpToolResult
		Error  *mcpError
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body.Result, body.Error
}

func TestMCPRealIndexSearch(t *testing.T) {
	h := newMCPHandler(searchtest.New(t), "test", "dev", nil)
	srv := httptest.NewServer(h)
	defer srv.Close() // OS-assigned port; never uses the running service.
	for _, tt := range []struct {
		name         string
		args         map[string]any
		want, absent []string
		files        int
	}{
		{"scoped OR", map[string]any{"pattern": `\b(Alpha|Beta)\s*\(`, "repos": []string{"repo.a"}, "path_regex": `^src/target[.]go$`, "context_lines": 0}, []string{`repo="repo.a" path="Src/Target.go"`, "1>| Alpha()", "3>| Beta()", "search_complete=true output_truncated=false"}, []string{"Other/", "repo-b", "5>|"}, 1},
		{"space is regex", map[string]any{"pattern": "Alpha Beta", "path_regex": "^Src/Target[.]go$", "repos": []string{"repo.a"}, "context_lines": 0}, []string{"5>| Alpha Beta"}, []string{"1>|", "3>|"}, 1},
		{"default context first line", map[string]any{"pattern": "Alpha\\(\\)", "path_regex": "^Src/Target[.]go$", "repos": []string{"repo.a"}}, []string{"lines=1-6", "1>| Alpha()", "6 | space + & # 中文"}, []string{"7 |"}, 1},
		{"last line", map[string]any{"pattern": "Tail", "repos": []string{"repo.a"}, "path_regex": "^Src/Target[.]go$", "context_lines": 2}, []string{"lines=5-7", "7>| Tail()"}, []string{"4 |"}, 1},
		{"merged adjacent", map[string]any{"pattern": "Alpha\\(\\)|Beta\\(\\)", "repos": []string{"repo.a"}, "path_regex": "^Src/Target[.]go$", "context_lines": 1, "max_snippets_per_file": 1}, []string{"lines=1-4", "output_truncated=false", "1>| Alpha()", "2 | context two", "3>| Beta()"}, []string{"snippet_limit"}, 1},
		{"snippet cap", map[string]any{"pattern": "Alpha\\(\\)|Tail\\(\\)", "repos": []string{"repo.a"}, "path_regex": "^Src/Target[.]go$", "context_lines": 0, "max_snippets_per_file": 1}, []string{"search_complete=true output_truncated=true", "snippet_limit"}, nil, 1},
		{"cross line", map[string]any{"pattern": `(?s:Alpha\(\).{0,30}Beta\(\))`, "repos": []string{"repo.a"}, "path_regex": "^Src/Target[.]go$", "context_lines": 0}, []string{"lines=1-3", "1>| Alpha()", "2>| context two", "3>| Beta()"}, nil, 1},
		{"files", map[string]any{"mode": "files", "pattern": "target[.]go$", "repos": []string{"repo.a"}, "path_regex": "^src/"}, []string{`path="Src/Target.go"`}, []string{"Alpha()", "lines="}, 1},
		{"content only", map[string]any{"pattern": "Alpha", "repos": []string{"repo.a"}, "path_regex": "^Src/Alpha[.]go$"}, []string{"search_complete=true output_truncated=false"}, []string{"repo="}, 0},
		{"case sensitive", map[string]any{"pattern": "alpha", "case": "sensitive", "repos": []string{"repo.a"}}, nil, []string{"repo="}, 0},
		{"word boundary folding", map[string]any{"pattern": `\balpha\b`, "repos": []string{"repo.a"}, "context_lines": 0}, []string{"1>| Alpha()", "5>| Alpha Beta"}, nil, 1},
		{"inline sensitive", map[string]any{"pattern": `(?-i:alpha)`, "repos": []string{"repo.a"}}, nil, []string{"repo="}, 0},
		{"inline insensitive", map[string]any{"pattern": `(?i:alpha)`, "case": "sensitive", "repos": []string{"repo.a"}, "context_lines": 0}, []string{"1>| Alpha()", "5>| Alpha Beta"}, nil, 1},
		{"path case sensitive", map[string]any{"pattern": "Alpha", "case": "sensitive", "path_regex": "^src/", "repos": []string{"repo.a"}}, nil, []string{"repo="}, 0},
		{"exact repo", map[string]any{"pattern": "Beta", "repos": []string{"repo"}}, nil, []string{"repo="}, 0},
		{"repo OR", map[string]any{"pattern": "Beta", "repos": []string{"repo.a", "repo-b"}, "path_regex": "^Src/Target[.]go$"}, []string{`repo="repo.a"`, `repo="repo-b"`}, nil, 2},
		{"short CRLF", map[string]any{"pattern": "Second", "repos": []string{"repo.a"}}, []string{"lines=1-3", "1 | First", "2>| Second", "3 | Last"}, nil, 1},
		{"adjacent windows", map[string]any{"pattern": "First|Second", "repos": []string{"repo.a"}, "context_lines": 0, "max_snippets_per_file": 1}, []string{"lines=1-2", "1>| First", "2>| Second", "output_truncated=false"}, nil, 1},
		{"empty file zero width", map[string]any{"pattern": `\A\z`, "repos": []string{"repo.a"}, "path_regex": "Empty[.]go$", "context_lines": 0}, []string{"lines=1-1", "1>| "}, nil, 1},
		{"EOF zero width", map[string]any{"pattern": `\z`, "repos": []string{"repo.a"}, "path_regex": "Short[.]go$", "context_lines": 0}, []string{"3>| Last"}, nil, 1},
		{"EOF after newline", map[string]any{"pattern": `\z`, "repos": []string{"repo.a"}, "path_regex": "Unicode[.]go$"}, []string{"lines=1-2", "1 | é 中文()", "2>| "}, nil, 1},
		{"unicode", map[string]any{"pattern": `中文\(`, "repos": []string{"repo.a"}}, []string{"1>| é 中文()"}, nil, 1},
		{"file cap observed", map[string]any{"mode": "files", "pattern": "[.]go$", "max_files": 1}, []string{"output_truncated=true", "file_limit"}, nil, 1},
		{"file cap exact", map[string]any{"mode": "files", "pattern": "Empty[.]go$", "repos": []string{"repo.a"}, "max_files": 1}, []string{"search_complete=true output_truncated=false"}, nil, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			result, e := callMCPHTTP(t, srv.URL, "search", tt.args)
			if e != nil || result.IsError || len(result.Content) != 1 {
				t.Fatalf("error: %+v %+v", e, result)
			}
			text := result.Content[0].Text
			for _, want := range tt.want {
				if !strings.Contains(text, want) {
					t.Errorf("missing %q in %s", want, text)
				}
			}
			for _, absent := range tt.absent {
				if strings.Contains(text, absent) {
					t.Errorf("unexpected %q in %s", absent, text)
				}
			}
			if got := strings.Count(text, "\nrepo="); got != tt.files {
				t.Errorf("files=%d want=%d: %s", got, tt.files, text)
			}
		})
	}
}

func TestMCPNewArguments(t *testing.T) {
	for _, raw := range []string{
		`null`, `[]`, `{}`, `{"pattern":null}`, `{"pattern":""}`, `{"pattern":1}`, `{"pattern":"["}`,
		`{"pattern":"foo","query":"bar"}`, `{"pattern":"foo","case":"auto"}`, `{"pattern":"foo","mode":"summary"}`,
		`{"Pattern":"foo"}`, `{"pattern":"foo","MODE":"files"}`,
		`{"pattern":"foo","repos":[]}`, `{"pattern":"foo","repos":[""]}`, `{"pattern":"foo","repos":null}`,
		`{"pattern":"foo","context_lines":-1}`, `{"pattern":"foo","context_lines":51}`, `{"pattern":"foo","max_files":0}`,
		`{"pattern":"foo","max_files":20001}`, `{"pattern":"foo","max_files":1.5}`, `{"pattern":"foo","timeout_ms":99}`,
		`{"pattern":"foo","timeout_ms":10001}`, `{"pattern":"foo","max_snippets_per_file":201}`,
		`{"pattern":"foo","mode":"files","context_lines":0}`, `{"pattern":"foo","mode":"files","max_snippets_per_file":20}`,
		`{"pattern":"foo","path_regex":"(?=x)"}`, `{"pattern":"(x)\\1"}`,
	} {
		t.Run(raw, func(t *testing.T) {
			if _, _, err := parseMCPSearchArgs([]byte(raw)); err == nil {
				t.Fatal("expected invalid args")
			}
		})
	}
	a, _, err := parseMCPSearchArgs([]byte(`{"pattern":" "}`))
	if err != nil || a.Case != "insensitive" || a.ContextLines != 5 || a.MaxFiles != 200 || a.MaxSnippetsPerFile != 20 || a.TimeoutMS != 3000 {
		t.Fatalf("defaults: %+v %v", a, err)
	}
}

func TestMCPProtocolErrorsAndSchema(t *testing.T) {
	h := newMCPHandler(fakeStreamer{}, "test", "dev", nil)
	for _, tt := range []struct {
		body         string
		status, code int
	}{
		{`{`, 200, -32700}, // Existing single-message JSON-RPC error contract.
		{`{"jsonrpc":"2.0","id":1}`, 200, -32600},
		{`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"missing"}}`, 200, -32602},
		{`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search","arguments":{"pattern":"x","path":"Src"}}}`, 200, -32602},
	} {
		r := postMCP(t, h, tt.body)
		var body mcpResponse
		if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if r.Code != tt.status || body.Error == nil || body.Error.Code != tt.code {
			t.Fatalf("%s: %d %+v", tt.body, r.Code, body)
		}
	}
	schema := h.tools(defaultMCPProtocolVersion)[1]["inputSchema"].(map[string]any)
	properties := schema["properties"].(map[string]any)
	defaults, _, err := parseMCPSearchArgs([]byte(`{"pattern":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]any{"context_lines": defaults.ContextLines, "max_files": defaults.MaxFiles, "max_snippets_per_file": defaults.MaxSnippetsPerFile, "timeout_ms": defaults.TimeoutMS, "mode": defaults.Mode, "case": defaults.Case} {
		if got := properties[name].(map[string]any)["default"]; got != want {
			t.Errorf("%s default=%v want=%v", name, got, want)
		}
	}
	if schema["additionalProperties"] != false {
		t.Fatal("unknown args must be rejected")
	}
}

func TestMCPRawRealIndex(t *testing.T) {
	srv := httptest.NewServer(newMCPHandler(searchtest.New(t), "test", "dev", nil))
	defer srv.Close()
	for _, tt := range []struct {
		query, prefix string
		files         int
	}{
		{`content:Alpha content:Beta case:no`, `r:^repo[.]a$ file:^Src/Target[.]go$`, 1},
		{`(content:NoSuchNeedle or content:Beta) case:no`, `r:^repo[.]a$ file:^Src/Target[.]go$`, 1},
		{`content:NoSuchNeedle or content:Beta case:no`, `r:^repo[.]a$ file:^Src/Target[.]go$`, 4},
		{`file:Target[.]go$ type:filename case:no`, `r:^repo[.]a$`, 3},
		{`content:alpha case:yes`, `r:^repo[.]a$`, 0},
		{`content:alpha case:no`, `r:^repo[.]a$`, 1},
		{`content:"Alpha Beta"`, `r:^repo[.]a$`, 1},
	} {
		result, e := callMCPHTTP(t, srv.URL, "search_raw", map[string]string{"query": tt.query, "prefix": tt.prefix})
		if e != nil || result.IsError {
			t.Fatalf("%s: %+v %+v", tt.query, e, result)
		}
		if got := strings.Count(result.Content[0].Text, "File: "); got != tt.files {
			t.Errorf("%s files=%d want=%d: %s", tt.query, got, tt.files, result.Content[0].Text)
		}
	}
	result, e := callMCPHTTP(t, srv.URL, "search_raw", map[string]string{"query": "content:["})
	if e != nil || !result.IsError {
		t.Fatal("invalid raw regex must be a tool error")
	}
}

type blockedSearcher struct{ fakeStreamer }

func (blockedSearcher) Search(ctx context.Context, _ query.Q, _ *zoekt.SearchOptions) (*zoekt.SearchResult, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestMCPDeadlineAndResponseBudget(t *testing.T) {
	t.Run("deadline", func(t *testing.T) {
		h := newMCPHandler(blockedSearcher{}, "test", "dev", nil)
		a, q, err := parseMCPSearchArgs([]byte(`{"pattern":"Alpha","timeout_ms":100}`))
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.WithValue(context.Background(), mcpRequestStart{}, time.Now().Add(-time.Second))
		start := time.Now()
		r := h.callAgentSearch(ctx, a, q)
		if time.Since(start) > time.Second || !strings.Contains(r.Content[0].Text, "search_complete=false") || !strings.Contains(r.Content[0].Text, "timeout") {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("response", func(t *testing.T) {
		h := newMCPHandler(searchtest.New(t), "test", "dev", nil)
		h.maxResponseBytes = 1100
		srv := httptest.NewServer(h)
		defer srv.Close()
		r, e := callMCPHTTP(t, srv.URL, "search", map[string]any{"pattern": "Alpha", "repos": []string{"repo.a"}})
		if e != nil || !strings.Contains(r.Content[0].Text, "output_truncated=true") || !strings.Contains(r.Content[0].Text, "response_limit") {
			t.Fatalf("%+v %+v", r, e)
		}
		encoded, _ := json.Marshal(r)
		if len(encoded) > h.maxResponseBytes {
			t.Fatalf("response too large: %d", len(encoded))
		}
	})
}

func BenchmarkMCPSearch(b *testing.B) {
	h := newMCPHandler(searchtest.New(b), "benchmark", "dev", nil)
	for _, tool := range []string{"search", "search_raw"} {
		b.Run(tool, func(b *testing.B) {
			args := `{"pattern":"Alpha|Beta","repos":["repo.a"],"path_regex":"^Src/"}`
			if tool == "search_raw" {
				args = `{"query":"r:^repo[.]a$ file:^Src/ content:Alpha|Beta case:no"}`
			}
			payload := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, tool, args)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest("POST", "/mcp", strings.NewReader(payload))
				h.ServeHTTP(rec, req)
				if rec.Code != 200 || strings.Contains(rec.Body.String(), `"isError":true`) {
					b.Fatal(rec.Body.String())
				}
			}
		})
	}
}
