package web

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/index"
)

func TestFormatResultsUsesAbsolutePathForCodeLink(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Engine")
	repo := &zoekt.Repository{Name: "local-repo"}
	if err := zoekt.SetFileSystemRoots(repo, map[string]string{"Engine": root}); err != nil {
		t.Fatal(err)
	}

	s := &Server{}
	result := &zoekt.SearchResult{
		Files: []zoekt.FileMatch{{
			FileName:   "Engine/src/main.go",
			Repository: "local-repo",
			LineMatches: []zoekt.LineMatch{{
				Line:       []byte("hello world"),
				LineNumber: 12,
				LineFragments: []zoekt.LineFragmentMatch{{
					LineOffset:  0,
					Offset:      0,
					MatchLength: 5,
				}},
			}},
		}},
	}

	matches, err := s.formatResults(result, "hello", true, map[string]*zoekt.Repository{
		"local-repo": repo,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || len(matches[0].Matches) != 1 {
		t.Fatalf("unexpected result shape: %+v", matches)
	}

	if got, want := matches[0].FileName, "Engine/src/main.go"; got != want {
		t.Fatalf("display file name = %q, want %q", got, want)
	}
	if got, want := matches[0].DisplayName, filepath.Join(root, "src", "main.go"); got != want {
		t.Fatalf("display path = %q, want %q", got, want)
	}

	if got, want := matches[0].Matches[0].FileName, filepath.Join(root, "src", "main.go"); got != want {
		t.Fatalf("code link file name = %q, want %q", got, want)
	}
}

func TestPrintDisplaysAbsolutePathForLocalIndex(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Engine")
	repo := zoekt.Repository{Name: "local-repo", Branches: []zoekt.RepositoryBranch{{Name: "HEAD", Version: "deadbeef"}}}
	if err := zoekt.SetFileSystemRoot(&repo, root); err != nil {
		t.Fatal(err)
	}

	b, err := index.NewShardBuilder(&repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Add(index.Document{
		Name:     "src/main.go",
		Content:  []byte("package main\n"),
		Branches: []string{"HEAD"},
	}); err != nil {
		t.Fatal(err)
	}

	srv := Server{
		Searcher: searcherForTest(t, b),
		Top:      Top,
		HTML:     true,
		Print:    true,
	}

	mux, err := NewMux(&srv)
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/print?r=local-repo&f=src/main.go", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	want := filepath.Join(root, "src", "main.go")
	if body := rec.Body.String(); !strings.Contains(body, want) {
		t.Fatalf("print page did not contain absolute path %q: %s", want, body)
	}
}
