package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/internal/searchtest"
)

func fixtureSearchMux(t testing.TB) http.Handler {
	t.Helper()
	mux, err := NewMux(&Server{Searcher: searchtest.New(t), Top: Top, HTML: true, RPC: true})
	if err != nil {
		t.Fatal(err)
	}
	return mux
}

func TestHTTPRealIndexSearchContracts(t *testing.T) {
	srv := httptest.NewServer(fixtureSearchMux(t))
	defer srv.Close()
	for _, tt := range []struct {
		name, q string
		lines   []int
	}{
		{"regex scope", `r:^repo[.]a$ file:^Src/Target[.]go$ content:\b(Alpha|Beta)\s*\( case:no`, []int{1, 3}},
		{"cross line", `r:^repo[.]a$ file:^Src/Target[.]go$ content:(?s:Alpha\(\).{0,30}Beta\(\)) case:no`, []int{1, 2, 3}},
		{"file AND", `r:^repo[.]a$ file:^Src/Target[.]go$ content:Alpha content:Beta case:no`, []int{1, 3, 5}},
		{"URL and JSON encoding", `r:^repo[.]a$ content:"space \\+ & # 中文" case:no`, []int{6}},
		{"last line", `r:^repo[.]a$ content:Tail case:no`, []int{7}},
		{"no match", `r:^repo[.]a$ file:^Src/Alpha[.]go$ content:Alpha`, []int{}},
		{"case sensitive", `r:^repo[.]a$ content:alpha case:yes`, []int{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			postBody, _ := json.Marshal(map[string]any{"Q": tt.q, "Opts": map[string]int{"NumContextLines": 2, "MaxDocDisplayCount": 20}})
			res, err := http.Post(srv.URL+"/api/search", "application/json", bytes.NewReader(postBody))
			if err != nil {
				t.Fatal(err)
			}
			postRaw, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode != 200 {
				t.Fatalf("POST: %s", postRaw)
			}
			var post struct{ Result *zoekt.SearchResult }
			if err := json.Unmarshal(postRaw, &post); err != nil {
				t.Fatal(err)
			}
			postLines := []int{}
			for _, f := range post.Result.Files {
				if f.Repository != "repo.a" || !strings.HasSuffix(strings.ReplaceAll(f.FileName, `\`, "/"), "Src/Target.go") {
					t.Fatalf("scope/path: %+v", f)
				}
				for _, m := range f.LineMatches {
					postLines = append(postLines, m.LineNumber)
					if len(m.Line) == 0 {
						t.Fatal("empty decoded match")
					}
				}
			}
			// LineMatch results are ranked, so compare source-line sets.
			sortInts(postLines)
			if !reflect.DeepEqual(postLines, tt.lines) {
				t.Fatalf("POST lines=%v want=%v", postLines, tt.lines)
			}
			query := url.Values{"q": {tt.q}, "format": {"json"}, "ctx": {"2"}, "num": {"20"}}
			res, err = http.Get(srv.URL + "/search?" + query.Encode())
			if err != nil {
				t.Fatal(err)
			}
			getRaw, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode != 200 {
				t.Fatalf("GET: %s", getRaw)
			}
			var get ApiSearchResult
			if err := json.Unmarshal(getRaw, &get); err != nil {
				t.Fatal(err)
			}
			getLines := []int{}
			for _, f := range get.Result.FileMatches {
				if f.Repo != "repo.a" {
					t.Fatal("repo leak")
				}
				for _, m := range f.Matches {
					getLines = append(getLines, m.LineNum)
				}
			}
			sortInts(getLines)
			if !reflect.DeepEqual(getLines, tt.lines) {
				t.Fatalf("GET lines=%v want=%v", getLines, tt.lines)
			}
			if len(tt.lines) > 0 {
				// Keep Vim's two wire contracts distinct: POST byte slices are base64;
				// GET exposes string fragments, never the new MCP text layout.
				var wire map[string]json.RawMessage
				json.Unmarshal(postRaw, &wire)
				if _, ok := wire["Result"]; !ok {
					t.Fatal("POST envelope changed")
				}
				json.Unmarshal(getRaw, &wire)
				if _, ok := wire["result"]; !ok {
					t.Fatal("GET envelope changed")
				}
				if !bytes.Contains(getRaw, []byte(`"Fragments"`)) || !bytes.Contains(postRaw, []byte(`"LineMatches"`)) {
					t.Fatal("wire shape changed")
				}
			}
		})
	}
}

func sortInts(values []int) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func TestHTTPOptionsAndErrors(t *testing.T) {
	srv := httptest.NewServer(fixtureSearchMux(t))
	defer srv.Close()
	for _, tt := range []struct {
		ids  any
		want int
	}{{nil, 2}, {[]int{}, 0}, {[]int{2}, 1}} {
		body := map[string]any{"Q": `file:^Src/Target[.]go$ content:Beta case:no`}
		if tt.ids != nil {
			body["RepoIDs"] = tt.ids
		}
		payload, _ := json.Marshal(body)
		res, err := http.Post(srv.URL+"/api/search", "application/json", bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		var reply struct{ Result *zoekt.SearchResult }
		err = json.NewDecoder(res.Body).Decode(&reply)
		res.Body.Close()
		if err != nil || reply.Result == nil {
			t.Fatalf("%+v %v", reply, err)
		}
		if len(reply.Result.Files) != tt.want {
			t.Fatalf("RepoIDs=%v files=%d want=%d", tt.ids, len(reply.Result.Files), tt.want)
		}
	}
	for _, tt := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/api/search", "", 405}, {"POST", "/api/search", "{", 400},
		{"POST", "/api/search", `{}`, 400}, {"POST", "/api/search", `{"Q":"content:["}`, 400},
		{"GET", "/search?format=json&q=Alpha&ctx=20", "", 418},
		{"GET", "/search?format=json&q=Alpha&ctx=-1", "", 418},
		{"GET", "/search?format=json&q=Alpha&ctx=foo", "", 418},
	} {
		req, _ := http.NewRequest(tt.method, srv.URL+tt.path, strings.NewReader(tt.body))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != tt.status {
			t.Errorf("%s %s = %d want %d", tt.method, tt.path, res.StatusCode, tt.status)
		}
	}
}

func BenchmarkHTTPJSONSearch(b *testing.B) {
	mux := fixtureSearchMux(b)
	for _, method := range []string{"POST", "GET"} {
		b.Run(method, func(b *testing.B) {
			q := `r:^repo[.]a$ file:^Src/ content:(Alpha|Beta) case:no`
			body, _ := json.Marshal(map[string]any{"Q": q, "Opts": map[string]int{"NumContextLines": 2, "MaxDocDisplayCount": 20}})
			path := "/api/search"
			if method == "GET" {
				path = "/search?" + url.Values{"q": {q}, "format": {"json"}, "ctx": {"2"}, "num": {"20"}}.Encode()
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				req := httptest.NewRequest(method, path, bytes.NewReader(body))
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, req)
				if rec.Code != 200 {
					b.Fatal(fmt.Sprint(rec.Code), rec.Body.String())
				}
			}
		})
	}
}
