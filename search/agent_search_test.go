package search

import (
	"context"
	"testing"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/index"
	"github.com/sourcegraph/zoekt/internal/searchbudget"
	"github.com/sourcegraph/zoekt/query"
)

func TestAgentShardedCompletion(t *testing.T) {
	ss := newShardedSearcher(1)
	defer ss.Close()
	for i, repo := range reposForTest(3) {
		shard := searcherForTest(t, testShardBuilder(t, repo, index.Document{Name: "file.go", Content: []byte("Needle Needle Needle")}))
		ss.replace(map[string]zoekt.Searcher{string(rune('a' + i)): shard})
	}
	ss.markReady()
	ctx, budget := searchbudget.With(context.Background(), 1<<20)
	r, err := ss.Search(ctx, &query.Substring{Pattern: "Needle", Content: true}, &zoekt.SearchOptions{})
	if err != nil || len(r.Files) != 3 || budget.Flags() != searchbudget.Known {
		t.Fatalf("complete: %+v %v flags=%d", r, err, budget.Flags())
	}
	ctx, budget = searchbudget.With(context.Background(), 1)
	r, err = ss.Search(ctx, &query.Substring{Pattern: "Needle", Content: true}, &zoekt.SearchOptions{})
	if err != nil || len(r.Files) != 0 || budget.Flags()&searchbudget.ResponseLimit == 0 {
		t.Fatalf("budget: %+v %v flags=%d", r, err, budget.Flags())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ctx, budget = searchbudget.With(ctx, 1<<20)
	_, _ = ss.Search(ctx, &query.Substring{Pattern: "Needle", Content: true}, &zoekt.SearchOptions{})
	if budget.Flags()&searchbudget.Cancelled == 0 {
		t.Fatalf("cancelled search was not marked: %d", budget.Flags())
	}
}
