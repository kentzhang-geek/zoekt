package index

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/internal/searchbudget"
	"github.com/sourcegraph/zoekt/query"
)

func TestAgentIndexedPaths(t *testing.T) {
	sub := filepath.Join("vendor", "nested")
	repo := &zoekt.Repository{Name: "fixture", SubRepoMap: map[string]*zoekt.Repository{sub: {Name: "sub"}}, FileTombstones: map[string]struct{}{filepath.Join("gone", "old.go"): {}}}
	b, err := NewShardBuilder(repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range []Document{
		{Name: filepath.Join(sub, "file.go"), SubRepositoryPath: sub, Content: []byte("Needle")},
		{Name: filepath.Join("gone", "old.go"), Content: []byte("Needle")},
	} {
		if err := b.Add(doc); err != nil {
			t.Fatal(err)
		}
	}
	s := searcherForTest(t, b)
	defer s.Close()
	q, err := query.Parse(`file:^vendor/nested/file[.]go$ content:Needle`)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Search(context.Background(), q, &zoekt.SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Files) != 1 || r.Files[0].FileName != "vendor/nested/file.go" || r.Files[0].SubRepositoryPath != "vendor/nested" {
		t.Fatalf("paths: %+v", r.Files)
	}
	q, _ = query.Parse(`content:Needle`)
	r, err = s.Search(context.Background(), q, &zoekt.SearchOptions{})
	if err != nil || len(r.Files) != 1 {
		t.Fatalf("tombstone: %+v %v", r, err)
	}
	if _, ok := repo.SubRepoMap[sub]; !ok {
		t.Fatal("input metadata was mutated")
	}
	if runtime.GOOS != "windows" {
		b := testShardBuilder(t, nil, Document{Name: `literal\name.go`, Content: []byte("Needle")})
		r := searchForTest(t, b, &query.Substring{Pattern: "Needle", Content: true})
		if len(r.Files) != 1 || r.Files[0].FileName != `literal\name.go` {
			t.Fatal("Unix literal backslash was rewritten")
		}
	}
}

func TestAgentSearchBudget(t *testing.T) {
	b := testShardBuilder(t, nil, Document{Name: "a.go", Content: []byte("Needle\n" + strings.Repeat("large context\n", 100))}, Document{Name: "b.go", Content: []byte("Needle\n")})
	s := searcherForTest(t, b)
	defer s.Close()
	q := &query.Substring{Pattern: "Needle", Content: true}
	t.Run("memory", func(t *testing.T) {
		ctx, budget := searchbudget.With(context.Background(), 512)
		r, err := s.Search(ctx, q, &zoekt.SearchOptions{ChunkMatches: true, NumContextLines: 50})
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Files) != 0 || r.FileCount != 1 || budget.Flags()&searchbudget.ResponseLimit == 0 {
			t.Fatalf("%+v flags=%d", r, budget.Flags())
		}
	})
	t.Run("match limit", func(t *testing.T) {
		ctx, budget := searchbudget.With(context.Background(), 1<<20)
		r, err := s.Search(ctx, q, &zoekt.SearchOptions{ShardMaxMatchCount: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Files) != 1 || budget.Flags()&searchbudget.MatchLimit == 0 {
			t.Fatalf("%+v flags=%d", r, budget.Flags())
		}
	})
	t.Run("files never load contents", func(t *testing.T) {
		ctx, budget := searchbudget.With(context.Background(), 1<<20)
		r, err := s.Search(ctx, &query.Substring{Pattern: ".go", FileName: true}, &zoekt.SearchOptions{ChunkMatches: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Files) != 2 || r.FilesLoaded != 0 || budget.Flags() != searchbudget.Known {
			t.Fatalf("%+v flags=%d", r, budget.Flags())
		}
	})
}
