package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/sourcegraph/zoekt/index"
)

var readParallelism = flag.Int("read_parallelism", 0, "parallel file readers/checkers (0: min of GOMAXPROCS and shard parallelism)")

const inputBufferBytes int64 = 64 << 20

type inputRoot struct {
	path, prefix string
}

type inputTask struct {
	path, name string
	size       int64
	weight     int64
	ready      chan index.PreparedDocument
}

type inputStats struct {
	files, bytes, active, readNanos, prepareNanos atomic.Int64
}

func inputWorkers(configured, shardWorkers int) (int, error) {
	if configured < 0 {
		return 0, fmt.Errorf("read_parallelism must not be negative")
	}
	if configured == 0 {
		return max(1, min(runtime.GOMAXPROCS(0), shardWorkers)), nil
	}
	return configured, nil
}

func addPathsToBuilder(builder *index.Builder, roots []inputRoot, opts index.Options, ignore, extensions map[string]struct{}, workers int) error {
	return streamPreparedFiles(context.Background(), roots, opts, ignore, extensions, workers,
		os.ReadFile, builder.PrepareDocument, builder.AddPrepared)
}

// streamPreparedFiles keeps a bounded window of files in flight and commits in
// traversal order. Workers never mutate the shared Builder. All goroutines are
// joined before returning, including when walking, reading or committing fails.
func streamPreparedFiles(parent context.Context, roots []inputRoot, opts index.Options,
	ignore, extensions map[string]struct{}, workers int,
	readFile func(string) ([]byte, error),
	prepare func(index.Document, *index.DocChecker) index.PreparedDocument,
	commit func(index.PreparedDocument) error,
) error {
	if workers < 1 {
		return fmt.Errorf("input workers must be positive")
	}
	ctx, cancel := context.WithCancelCause(parent)
	var wg sync.WaitGroup

	// Limit both file count and estimated content bytes, including completed
	// results waiting for an earlier file. An explicitly allowed oversized file
	// reserves the entire byte budget so it cannot overlap other buffered files.
	window := make(chan struct{}, workers*2)
	budget := semaphore.NewWeighted(inputBufferBytes)
	jobs := make(chan inputTask)
	ordered := make(chan inputTask, workers)
	var stats inputStats
	start := time.Now()
	var enqueueTime time.Duration
	defer func() {
		cancel(nil)
		wg.Wait()
		log.Printf("input timing: files=%d bytes=%d elapsed=%s read_work=%s prepare_work=%s enqueue=%s",
			stats.files.Load(), stats.bytes.Load(), time.Since(start).Round(time.Millisecond),
			time.Duration(stats.readNanos.Load()).Round(time.Millisecond),
			time.Duration(stats.prepareNanos.Load()).Round(time.Millisecond), enqueueTime.Round(time.Millisecond))
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				log.Printf("index input progress: prepared=%d bytes=%d active=%d in_flight=%d elapsed=%s",
					stats.files.Load(), stats.bytes.Load(), stats.active.Load(), len(window), time.Since(start).Round(time.Second))
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(jobs)
		defer close(ordered)
		for _, root := range roots {
			dir, err := filepath.Abs(filepath.Clean(root.path))
			if err != nil {
				cancel(err)
				return
			}
			log.Printf("adding path to repository %s: %s", opts.RepositoryDescription.Name, dir)
			err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				if d.IsDir() {
					if _, skip := ignore[d.Name()]; skip {
						return filepath.SkipDir
					}
					return nil
				}
				if len(extensions) > 0 {
					if _, ok := extensions[strings.ToLower(filepath.Ext(path))]; !ok {
						return nil
					}
				}
				info, err := d.Info()
				if err != nil {
					return err
				}
				if !info.Mode().IsRegular() {
					return nil
				}
				name := displayNameForIndexedPath(dir, path)
				if root.prefix != "" {
					name = filepath.ToSlash(filepath.Join(root.prefix, name))
				}
				weight := min(inputBufferBytes, max(1, info.Size()))
				if info.Size() > int64(opts.SizeMax) && !opts.IgnoreSizeMax(name) {
					weight = 1
				}
				select {
				case window <- struct{}{}:
				case <-ctx.Done():
					return ctx.Err()
				}
				if err := budget.Acquire(ctx, weight); err != nil {
					return err
				}
				task := inputTask{path: path, name: name, size: info.Size(), weight: weight, ready: make(chan index.PreparedDocument, 1)}
				select {
				case jobs <- task:
				case <-ctx.Done():
					return ctx.Err()
				}
				select {
				case ordered <- task:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			if err != nil {
				cancel(fmt.Errorf("walking %s: %w", dir, err))
				return
			}
		}
	}()

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var checker index.DocChecker
			for {
				select {
				case <-ctx.Done():
					return
				case task, ok := <-jobs:
					if !ok {
						return
					}
					stats.active.Add(1)
					doc := index.Document{Name: task.name}
					if task.size > int64(opts.SizeMax) && !opts.IgnoreSizeMax(task.name) {
						doc.SkipReason = index.SkipReasonTooLarge
					} else {
						readStart := time.Now()
						var err error
						doc.Content, err = readFile(task.path)
						stats.readNanos.Add(int64(time.Since(readStart)))
						if err != nil {
							stats.active.Add(-1)
							cancel(fmt.Errorf("reading %s: %w", task.path, err))
							return
						}
						stats.bytes.Add(int64(len(doc.Content)))
					}
					prepareStart := time.Now()
					task.ready <- prepare(doc, &checker)
					stats.prepareNanos.Add(int64(time.Since(prepareStart)))
					stats.files.Add(1)
					stats.active.Add(-1)
				}
			}
		}()
	}

	for {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case task, ok := <-ordered:
			if !ok {
				return context.Cause(ctx)
			}
			select {
			case <-ctx.Done():
				return context.Cause(ctx)
			case doc := <-task.ready:
				enqueueStart := time.Now()
				err := commit(doc)
				enqueueTime += time.Since(enqueueStart)
				budget.Release(task.weight)
				<-window
				if err != nil {
					return err
				}
			}
		}
	}
}
