// Package searchtest provides a deterministic real index for interface tests.
package searchtest

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/index"
	"github.com/sourcegraph/zoekt/query"
)

const Source = "Alpha()\ncontext two\nBeta()\ncontext four\nAlpha Beta\nspace + & # 中文\nTail()"

// New creates two repositories with same-named files and deliberately different
// evidence outside the intended scope. It never starts a webserver or indexer.
func New(t testing.TB) zoekt.Streamer {
	t.Helper()
	dir := t.TempDir()
	s := &fixture{}
	for i, name := range []string{"repo.a", "repo-b"} {
		repo := &zoekt.Repository{Name: name, ID: uint32(i + 1), Branches: []zoekt.RepositoryBranch{{Name: "main", Version: "fixture"}}}
		if err := zoekt.SetFileSystemRoot(repo, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
		builder, err := index.NewShardBuilder(repo)
		if err != nil {
			t.Fatal(err)
		}
		docs := []index.Document{
			{Name: filepath.Join("Src", "Target.go"), Content: []byte(Source)},
			{Name: "Other/Target.go", Content: []byte("Beta()\noutside\n")},
			{Name: "SrcExtra/Target.go", Content: []byte("Beta()\n")},
			{Name: "Src/Alpha.go", Content: []byte("filename only\n")},
			{Name: "Src/Empty.go", Content: nil},
			{Name: "Src/Short.go", Content: []byte("First\r\nSecond\r\nLast")},
			{Name: "Src/Unicode.go", Content: []byte("é 中文()\n")},
		}
		if i == 1 {
			docs = docs[:1]
			docs[0].Content = []byte("Beta()\nother repository\n")
		}
		for _, doc := range docs {
			doc.Branches = []string{"main"}
			if err := builder.Add(doc); err != nil {
				t.Fatal(err)
			}
		}
		var data bytes.Buffer
		if err := builder.Write(&data); err != nil {
			t.Fatal(err)
		}
		shard, err := index.NewSearcher(&memoryIndex{data.Bytes()})
		if err != nil {
			t.Fatal(err)
		}
		s.shards = append(s.shards, shard)
	}
	t.Cleanup(s.Close)
	return s
}

type memoryIndex struct{ data []byte }

func (m *memoryIndex) Read(off, size uint32) ([]byte, error) { return m.data[off : off+size], nil }
func (m *memoryIndex) Size() (uint32, error)                 { return uint32(len(m.data)), nil }
func (m *memoryIndex) Close()                                {}
func (m *memoryIndex) Name() string                          { return "interface-fixture" }

type fixture struct{ shards []zoekt.Searcher }

func (s *fixture) Search(ctx context.Context, q query.Q, opts *zoekt.SearchOptions) (*zoekt.SearchResult, error) {
	r := &zoekt.SearchResult{}
	for _, shard := range s.shards {
		part, err := shard.Search(ctx, q, opts)
		if err != nil {
			return nil, err
		}
		r.Stats.Add(part.Stats)
		r.Files = append(r.Files, part.Files...)
	}
	r.Files = index.SortAndTruncateFiles(r.Files, opts)
	return r, nil
}
func (s *fixture) StreamSearch(ctx context.Context, q query.Q, opts *zoekt.SearchOptions, sender zoekt.Sender) error {
	r, err := s.Search(ctx, q, opts)
	if err == nil {
		sender.Send(r)
	}
	return err
}
func (s *fixture) List(ctx context.Context, q query.Q, opts *zoekt.ListOptions) (*zoekt.RepoList, error) {
	r := &zoekt.RepoList{}
	for _, shard := range s.shards {
		part, err := shard.List(ctx, q, opts)
		if err != nil {
			return nil, err
		}
		r.Repos = append(r.Repos, part.Repos...)
		r.Stats.Add(&part.Stats)
	}
	return r, nil
}
func (s *fixture) Close() {
	for _, shard := range s.shards {
		shard.Close()
	}
}
func (s *fixture) String() string { return "interface-fixture" }
