package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp/syntax"
	"sort"
	"strings"
	"time"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/internal/searchbudget"
	"github.com/sourcegraph/zoekt/query"
)

const defaultMCPMaxResponseBytes = 16 << 20

const mcpAgentSearchDescription = `Search indexed code with regex, case-insensitive by default.
mode=files searches indexed relative paths and returns filenames; mode=snippets searches content and returns matching lines with context (default: 5 before and 5 after, clipped at file boundaries).
Paths use /, also on Windows. path_regex matches any part of the indexed relative path unless anchored; it is not a filesystem path or literal prefix. Example: (^|/)TnT/Code/ matches that directory at any depth. The index root itself is omitted. Use list_repos to discover exact repository names. case sets the initial regex case mode; explicit inline flags such as (?-i:Name) retain their normal meaning.
pattern and path_regex are complete Go/RE2-style regex strings, not native Zoekt expressions, glob patterns, or /regex/ literals. Spaces are literal. Backslashes remain regex escapes; use / for path separators. JSON escaping is a separate layer.
Use related alternatives such as Foo|Bar, (Create|Init|Setup).*Renderer, or \b(CreateRenderer|DestroyRenderer)\s*\( to cover naming variants in one call. The latter matches textual call/declaration shapes, not semantic references. xx.+|yy.+ is valid but requires at least one trailing character and its short fixed strings may scan broadly. Prefer distinctive literal terms and narrow repo/path scope; a weak or wildcard-only OR branch can make the entire search expensive. Lookaround and backreferences are unsupported. Use (?s:BeginFrame.{0,300}EndFrame) only when bounded cross-line matching is needed.
Every regex alternative stays inside the explicit repos/path_regex scope. For file-level Boolean combinations or native query syntax, use search_raw. Renderer.*Cache requires order on a line; it does not mean both words anywhere in a file.
Check search_complete, output_truncated and partial_reasons before drawing absence conclusions. Large limits permit larger responses but do not guarantee completion within the deadline. If the host supports and authorizes subagents, independent investigations can be delegated by scope with concise evidence returned; this tool does not create subagents.`

type mcpRequestStart struct{}

type mcpSearchArgs struct {
	Pattern            string   `json:"pattern"`
	Mode               string   `json:"mode"`
	Repos              []string `json:"repos"`
	PathRegex          string   `json:"path_regex"`
	Case               string   `json:"case"`
	ContextLines       int      `json:"context_lines"`
	MaxFiles           int      `json:"max_files"`
	MaxSnippetsPerFile int      `json:"max_snippets_per_file"`
	TimeoutMS          int      `json:"timeout_ms"`
}

func parseMCPSearchArgs(raw json.RawMessage) (mcpSearchArgs, query.Q, error) {
	a := mcpSearchArgs{Mode: "snippets", Case: "insensitive", ContextLines: 5, MaxFiles: 200, MaxSnippetsPerFile: 20, TimeoutMS: 3000}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return a, nil, fmt.Errorf("search arguments must be an object")
	}
	for name, value := range fields {
		switch name {
		case "pattern", "mode", "repos", "path_regex", "case", "context_lines", "max_files", "max_snippets_per_file", "timeout_ms":
		default:
			return a, nil, fmt.Errorf("unknown search argument %q", name)
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return a, nil, fmt.Errorf("%s must not be null", name)
		}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&a); err != nil {
		return a, nil, fmt.Errorf("invalid search arguments: %w", err)
	}
	if a.Pattern == "" {
		return a, nil, fmt.Errorf("pattern must not be empty")
	}
	if a.Mode != "files" && a.Mode != "snippets" {
		return a, nil, fmt.Errorf("mode must be files or snippets")
	}
	if a.Case != "insensitive" && a.Case != "sensitive" {
		return a, nil, fmt.Errorf("case must be insensitive or sensitive")
	}
	for _, n := range []struct {
		name             string
		value, low, high int
	}{
		{"context_lines", a.ContextLines, 0, 50}, {"max_files", a.MaxFiles, 1, 20000},
		{"max_snippets_per_file", a.MaxSnippetsPerFile, 1, 200}, {"timeout_ms", a.TimeoutMS, 100, 10000},
	} {
		if n.value < n.low || n.value > n.high {
			return a, nil, fmt.Errorf("%s must be between %d and %d", n.name, n.low, n.high)
		}
	}
	if a.Mode == "files" {
		for _, name := range []string{"context_lines", "max_snippets_per_file"} {
			if _, ok := fields[name]; ok {
				return a, nil, fmt.Errorf("%s is only valid in snippets mode", name)
			}
		}
		a.ContextLines = 0
	}
	if _, ok := fields["repos"]; ok && len(a.Repos) == 0 {
		return a, nil, fmt.Errorf("repos must not be empty")
	}
	regex := func(pattern string, content, file bool) (query.Q, error) {
		flags := syntax.ClassNL | syntax.PerlX | syntax.UnicodeGroups
		if a.Case == "insensitive" {
			flags |= syntax.FoldCase
		}
		re, err := syntax.Parse(pattern, flags)
		if err != nil {
			return nil, err
		}
		// Keep scoped flags in the syntax tree, including (?-i:...). Turning
		// a folded literal into Substring here would discard its regex flags.
		return &query.Regexp{Regexp: query.OptimizeRegexp(re, flags), Content: content, FileName: file, CaseSensitive: true}, nil
	}
	pattern, err := regex(a.Pattern, a.Mode == "snippets", a.Mode == "files")
	if err != nil {
		return a, nil, fmt.Errorf("invalid pattern: %w", err)
	}
	qs := []query.Q{pattern}
	if a.PathRegex != "" {
		path, err := regex(a.PathRegex, false, true)
		if err != nil {
			return a, nil, fmt.Errorf("invalid path_regex: %w", err)
		}
		qs = append(qs, path)
	}
	if a.Repos != nil {
		repos := &query.RepoSet{Set: map[string]bool{}}
		for _, name := range a.Repos {
			if name == "" {
				return a, nil, fmt.Errorf("repository names must not be empty")
			}
			repos.Set[name] = true
		}
		qs = append(qs, repos)
	}
	return a, query.NewAnd(qs...), nil
}

type mcpSearchStatus struct {
	Complete  string
	Truncated bool
	Reasons   map[string]bool
}

func (s *mcpSearchStatus) reason(reason string, incomplete, truncated bool) {
	s.Reasons[reason] = true
	if incomplete {
		s.Complete = "false"
	}
	s.Truncated = s.Truncated || truncated
}

func (s *mcpSearchStatus) contextError(ctx context.Context, truncated bool) {
	reason := "timeout"
	if ctx.Err() == context.Canceled {
		reason = "cancelled"
	}
	s.reason(reason, true, truncated)
}

func (s mcpSearchStatus) header() string {
	reasons := make([]string, 0, len(s.Reasons))
	for r := range s.Reasons {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)
	return fmt.Sprintf("search_complete=%s output_truncated=%t partial_reasons=%s\n", s.Complete, s.Truncated, strings.Join(reasons, ","))
}

func (h *mcpHandler) callAgentSearch(ctx context.Context, a mcpSearchArgs, q query.Q) mcpToolResult {
	start, ok := ctx.Value(mcpRequestStart{}).(time.Time)
	if !ok {
		start = time.Now()
	}
	ctx, cancel := context.WithDeadline(ctx, start.Add(time.Duration(a.TimeoutMS)*time.Millisecond))
	defer cancel()
	maxBytes := h.maxResponseBytes
	if maxBytes <= 0 {
		maxBytes = defaultMCPMaxResponseBytes
	}
	ctx, budget := searchbudget.With(ctx, int64(maxBytes)*4)
	opts := &zoekt.SearchOptions{MaxDocDisplayCount: a.MaxFiles + 1, ChunkMatches: true, NumContextLines: a.ContextLines}
	opts.SetDefaults()
	result, err := h.searcher.Search(ctx, q, opts)
	searchError := err != nil && ctx.Err() == nil
	status := &mcpSearchStatus{Complete: "unknown", Reasons: map[string]bool{}}
	flags := budget.Flags()
	if flags&searchbudget.Known != 0 {
		status.Complete = "true"
	}
	for _, f := range []struct {
		flag uint32
		name string
	}{
		{searchbudget.Timeout, "timeout"}, {searchbudget.MatchLimit, "match_limit"},
		{searchbudget.Cancelled, "cancelled"},
		{searchbudget.ResponseLimit, "response_limit"}, {searchbudget.ShardError, "shard_error"}, {searchbudget.IndexNotReady, "index_not_ready"},
	} {
		if flags&f.flag != 0 {
			status.reason(f.name, true, f.flag == searchbudget.ResponseLimit)
		}
	}
	if ctx.Err() != nil {
		status.contextError(ctx, false)
	}
	if err != nil && ctx.Err() == nil {
		status.reason("search_error", true, false)
	}
	if result == nil {
		result = &zoekt.SearchResult{}
	}
	if result.Crashes > 0 && (flags&searchbudget.IndexNotReady == 0 || result.Crashes > 1) {
		status.reason("shard_error", true, false)
	}
	if (result.FilesSkipped > 0 || result.ShardsSkipped > 0) && status.Complete != "false" {
		status.reason("search_incomplete", true, false)
	}
	if len(result.Files) > a.MaxFiles || (result.FileCount > len(result.Files) && flags&searchbudget.ResponseLimit == 0) {
		status.reason("file_limit", false, true)
	}
	files := result.Files
	if len(files) > a.MaxFiles {
		files = files[:a.MaxFiles]
	}
	var repos map[string]*zoekt.Repository
	if ctx.Err() == nil && len(files) > 0 {
		repos, err = repoMetadataByName(ctx, h.searcher, files)
		if err != nil {
			status.reason("path_metadata_unavailable", false, false)
		}
	}
	text := formatMCPSearch(ctx, files, repos, a, status, maxBytes)
	if ctx.Err() != nil {
		status.contextError(ctx, false)
	}
	deadline, _ := ctx.Deadline()
	return mcpToolResult{Content: []mcpToolTextContent{{Type: "text", Text: status.header() + text}}, IsError: searchError, searchStatus: status, searchDeadline: deadline}
}

// mcpTextWriter accounts for JSON string escaping before growing the response.
// Each append is a complete file header or snippet, so truncation cannot split
// UTF-8, JSON, or a source snippet.
type mcpTextWriter struct {
	text      strings.Builder
	remaining int
}

func (w *mcpTextWriter) append(s string) bool {
	encoded, _ := json.Marshal(s)
	n := len(encoded) - 2
	if n > w.remaining {
		return false
	}
	w.remaining -= n
	w.text.WriteString(s)
	return true
}

func formatMCPSearch(ctx context.Context, files []zoekt.FileMatch, repos map[string]*zoekt.Repository, a mcpSearchArgs, status *mcpSearchStatus, maxBytes int) string {
	w := mcpTextWriter{remaining: max(0, maxBytes-1024)} // envelope + completion header
	quote := func(s string) string {
		encoded, _ := json.Marshal(s)
		return string(encoded)
	}
	seen := map[string]bool{}
	for _, f := range files {
		if ctx.Err() != nil {
			status.contextError(ctx, true)
			break
		}
		key := f.Repository + "\x00" + f.FileName
		if seen[key] {
			continue
		}
		seen[key] = true
		header := "\nrepo=" + quote(f.Repository) + " path=" + quote(f.FileName) + "\n"
		if repo := repos[f.Repository]; repo != nil {
			if abs := zoekt.ResolveFileSystemPath(repo, f.FileName); abs != "" {
				header += "absolute_path=" + quote(abs) + "\n"
			}
		}
		if a.Mode == "files" {
			if !w.append(header) {
				status.reason("response_limit", false, true)
				break
			}
			continue
		}
		chunks := mergeMCPChunks(f.ChunkMatches)
		// Select the most relevant merged snippets, then render in source order.
		if len(chunks) > a.MaxSnippetsPerFile {
			sort.SliceStable(chunks, func(i, j int) bool { return chunks[i].Score > chunks[j].Score })
			chunks = chunks[:a.MaxSnippetsPerFile]
			status.reason("snippet_limit", false, true)
			sort.Slice(chunks, func(i, j int) bool { return chunks[i].ContentStart.ByteOffset < chunks[j].ContentStart.ByteOffset })
		}
		for _, c := range chunks {
			if ctx.Err() != nil {
				status.contextError(ctx, true)
				return w.text.String()
			}
			// Refuse an oversized snippet before allocating its formatted copy.
			if len(c.Content) > w.remaining {
				status.reason("response_limit", false, true)
				return w.text.String()
			}
			var snippet strings.Builder
			snippet.WriteString(header)
			last := chunkLastLine(c)
			fmt.Fprintf(&snippet, "lines=%d-%d\n", c.ContentStart.LineNumber, last)
			lineNo, data, offset := c.ContentStart.LineNumber, c.Content, c.ContentStart.ByteOffset
			rangeIndex := 0
			if len(data) == 0 && len(c.Ranges) > 0 {
				fmt.Fprintf(&snippet, "%d>| \n", lineNo)
			}
			for len(data) > 0 {
				if ctx.Err() != nil {
					status.contextError(ctx, true)
					return w.text.String()
				}
				n := bytes.IndexByte(data, '\n')
				if n < 0 {
					n = len(data)
				} else {
					n++
				}
				for rangeIndex < len(c.Ranges) && c.Ranges[rangeIndex].End.ByteOffset <= offset && c.Ranges[rangeIndex].Start.ByteOffset < offset {
					rangeIndex++
				}
				hit := rangeIndex < len(c.Ranges) && c.Ranges[rangeIndex].Start.ByteOffset < offset+uint32(n)
				if !hit && rangeIndex < len(c.Ranges) && n == len(data) && data[n-1] != '\n' {
					r := c.Ranges[rangeIndex]
					hit = r.Start.ByteOffset == r.End.ByteOffset && r.Start.ByteOffset == offset+uint32(n)
				}
				marker := " | "
				if hit {
					marker = ">| "
				}
				fmt.Fprintf(&snippet, "%d%s%s\n", lineNo, marker, bytes.TrimSuffix(bytes.TrimSuffix(data[:n], []byte("\n")), []byte("\r")))
				lineNo++
				offset += uint32(n)
				data = data[n:]
				if snippet.Len() > w.remaining {
					status.reason("response_limit", false, true)
					return w.text.String()
				}
			}
			// A zero-width EOF match after a final newline belongs to the next
			// empty line, which has no source bytes for the loop above to visit.
			if len(c.Content) > 0 && lineNo <= last {
				fmt.Fprintf(&snippet, "%d>| \n", lineNo)
			}
			if !w.append(snippet.String()) {
				status.reason("response_limit", false, true)
				return w.text.String()
			}
			header = "" // one file header, even with several snippets
		}
	}
	return w.text.String()
}

func chunkLastLine(c zoekt.ChunkMatch) uint32 {
	n := bytes.Count(c.Content, []byte("\n"))
	if len(c.Content) > 0 && c.Content[len(c.Content)-1] != '\n' {
		n++
	}
	last := c.ContentStart.LineNumber + uint32(max(1, n)) - 1
	for _, r := range c.Ranges {
		if r.Start.ByteOffset == r.End.ByteOffset {
			last = max(last, r.Start.LineNumber)
		}
	}
	return last
}

func mergeMCPChunks(input []zoekt.ChunkMatch) []zoekt.ChunkMatch {
	chunks := append([]zoekt.ChunkMatch(nil), input...)
	sort.Slice(chunks, func(i, j int) bool { return chunks[i].ContentStart.ByteOffset < chunks[j].ContentStart.ByteOffset })
	merged := chunks[:0]
	owned := false
	for _, c := range chunks {
		if c.FileName {
			continue
		}
		if len(merged) > 0 {
			prev := &merged[len(merged)-1]
			end := prev.ContentStart.ByteOffset + uint32(len(prev.Content))
			if c.ContentStart.ByteOffset <= end {
				// Copy only when merging: index-owned buffers must never be mutated.
				if !owned {
					prev.Content = append([]byte(nil), prev.Content...)
					prev.Ranges = append([]zoekt.Range(nil), prev.Ranges...)
					owned = true
				}
				overlap := int(end - c.ContentStart.ByteOffset)
				if overlap < len(c.Content) {
					prev.Content = append(prev.Content, c.Content[overlap:]...)
				}
				prev.Ranges = append(prev.Ranges, c.Ranges...)
				prev.Score = max(prev.Score, c.Score)
				continue
			}
		}
		merged = append(merged, c)
		owned = false
	}
	return merged
}

func mcpAgentSearchTool() map[string]any {
	integer := func(low, high, value int, description string) map[string]any {
		return map[string]any{"type": "integer", "minimum": low, "maximum": high, "default": value, "description": description}
	}
	return map[string]any{"name": "search", "description": mcpAgentSearchDescription, "inputSchema": map[string]any{
		"type": "object", "additionalProperties": false, "required": []string{"pattern"}, "properties": map[string]any{
			"pattern":               map[string]any{"type": "string", "minLength": 1, "description": "Complete regex: path in files mode, content in snippets mode. Not native Zoekt syntax."},
			"mode":                  map[string]any{"type": "string", "enum": []string{"files", "snippets"}, "default": "snippets"},
			"repos":                 map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "string", "minLength": 1}, "description": "Exact repository names, OR. Omit to search all repositories."},
			"path_regex":            map[string]any{"type": "string", "description": "Regex on indexed relative paths using /. Unanchored unless you add ^ or $. AND with pattern and repos."},
			"case":                  map[string]any{"type": "string", "enum": []string{"insensitive", "sensitive"}, "default": "insensitive", "description": "Applies to pattern and path_regex, not repository names."},
			"context_lines":         integer(0, 50, 5, "Snippets only: lines before and after each hit; clipped at file boundaries."),
			"max_files":             integer(1, 20000, 200, "Maximum returned files; does not guarantee full coverage before timeout."),
			"max_snippets_per_file": integer(1, 200, 20, "Snippets only: maximum merged snippets per file."),
			"timeout_ms":            integer(100, 10000, 3000, "Request budget for queueing, search and result preparation."),
		},
	}}
}
