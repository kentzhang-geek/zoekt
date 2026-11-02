package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/index"
	"github.com/sourcegraph/zoekt/query"
)

func pipelineFixture(t *testing.T, count int) (string, index.Options, *index.Builder) {
	t.Helper()
	root := t.TempDir()
	for i := 0; i < count; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("%03d.go", i)), []byte("package test\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	opts := index.Options{IndexDir: t.TempDir(), DisableCTags: true, Parallelism: 4, RepositoryDescription: zoekt.Repository{Name: "test"}}
	opts.SetDefaults()
	b, err := index.NewBuilder(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := b.Abort(); err != nil {
			t.Error(err)
		}
	})
	return root, opts, b
}

func TestInputPipelineOverlapsReadsAndPreservesOrder(t *testing.T) {
	root, opts, b := pipelineFixture(t, 12)
	if err := os.Mkdir(filepath.Join(root, "ignored"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ignored", "bad.go"), []byte("ignored"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "omit.txt"), []byte("filtered"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	secondPrepared := make(chan struct{})
	var active, peak atomic.Int64
	read := func(path string) ([]byte, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		switch filepath.Base(path) {
		case "000.go":
			select {
			case <-secondPrepared:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		case "omit.txt", "bad.go":
			return nil, errors.New("filter leaked")
		}
		return os.ReadFile(path)
	}
	var prepared atomic.Int64
	prepare := func(doc index.Document, checker *index.DocChecker) index.PreparedDocument {
		p := b.PrepareDocument(doc, checker)
		prepared.Add(1)
		if doc.Name == "001.go" {
			close(secondPrepared)
		}
		return p
	}
	err := streamPreparedFiles(ctx, []inputRoot{{path: root}}, opts, buildIgnoreDirMap("ignored"), buildFileExtMap("go"), 4,
		read, prepare, b.AddPrepared)
	if err != nil {
		t.Fatal(err)
	}
	if peak.Load() < 2 || peak.Load() > 4 {
		t.Fatalf("unexpected concurrent readers: %d", peak.Load())
	}
	if prepared.Load() != 12 {
		t.Fatalf("prepared %d files", prepared.Load())
	}
	if err := b.Finish(); err != nil {
		t.Fatal(err)
	}
	shards, err := filepath.Glob(filepath.Join(opts.IndexDir, "*.zoekt"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(shards[0])
	if err != nil {
		t.Fatal(err)
	}
	inf, err := index.NewIndexFile(f)
	if err != nil {
		t.Fatal(err)
	}
	searcher, err := index.NewSearcher(inf)
	if err != nil {
		inf.Close()
		t.Fatal(err)
	}
	defer searcher.Close()
	result, err := searcher.Search(ctx, &query.Const{Value: true}, &zoekt.SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var got, want []string
	for _, file := range result.Files {
		got = append(got, file.FileName)
	}
	// Equal-size files have equal rank components except original input order.
	for i := 0; i < 12; i++ {
		want = append(want, fmt.Sprintf("%03d.go", i))
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("input order changed: %v", got)
	}

}

func TestInputPipelineErrorsAndCancellation(t *testing.T) {
	for _, mode := range []string{"read", "commit", "walk", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			root, opts, b := pipelineFixture(t, 100)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			want := errors.New("injected failure")
			var active atomic.Int64
			read := func(path string) ([]byte, error) {
				active.Add(1)
				defer active.Add(-1)
				if mode == "read" {
					return nil, want
				}
				if mode == "cancel" {
					cancel()
					return nil, ctx.Err()
				}
				return os.ReadFile(path)
			}
			commit := func(doc index.PreparedDocument) error {
				if mode == "commit" {
					return want
				}
				return b.AddPrepared(doc)
			}
			if mode == "walk" {
				root = filepath.Join(root, "missing")
			}
			err := streamPreparedFiles(ctx, []inputRoot{{path: root}}, opts, nil, nil, 4, read, b.PrepareDocument, commit)
			if err == nil {
				t.Fatal("expected error")
			}
			if (mode == "read" || mode == "commit") && !errors.Is(err, want) {
				t.Fatalf("error lost: %v", err)
			}
			if active.Load() != 0 {
				t.Fatal("returned while readers were running")
			}
		})
	}
}

func TestInputWorkers(t *testing.T) {
	for _, configured := range []int{-1, 1, 4} {
		got, err := inputWorkers(configured, 20)
		if configured < 0 {
			if err == nil {
				t.Fatal("negative count accepted")
			}
		} else if err != nil || got != configured {
			t.Fatalf("workers=%d error=%v", got, err)
		}
	}
	if got, err := inputWorkers(0, 1); err != nil || got != 1 {
		t.Fatalf("serial setting: %d %v", got, err)
	}
}

func TestInputFailureDoesNotPublish(t *testing.T) {
	root, opts, _ := pipelineFixture(t, 1)
	if err := indexConfigPaths(&IndexConfig{Paths: []string{root}}, opts, nil, nil); err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(opts.IndexDir, "*.zoekt"))
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	opts.ShardMax = 1 // ensure a shard is already in flight before the later walk error
	err = indexConfigPaths(&IndexConfig{Paths: []string{root, filepath.Join(root, "missing")}}, opts, nil, nil)
	if err == nil {
		t.Fatal("expected missing root error")
	}
	after, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed input replaced published index")
	}
}
