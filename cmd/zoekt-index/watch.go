package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/sourcegraph/zoekt/cmd"
	"github.com/sourcegraph/zoekt/index"
)

func buildIgnoreDirMap(raw string) map[string]struct{} {
	ignoreDirMap := map[string]struct{}{}
	for _, d := range strings.Split(raw, ",") {
		d = strings.TrimSpace(d)
		if d != "" {
			ignoreDirMap[d] = struct{}{}
		}
	}
	return ignoreDirMap
}

func buildFileExtMap(raw string) map[string]struct{} {
	fileExts := map[string]struct{}{}
	for _, ext := range strings.Split(raw, "|") {
		ext = strings.TrimSpace(ext)
		if ext == "" {
			continue
		}

		if !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}

		fileExts[strings.ToLower(ext)] = struct{}{}
	}
	return fileExts
}

func displayNameForIndexedPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}

func runIndexConfig(config *IndexConfig, defaultIndexDir string) error {
	ignoreDirMap := buildIgnoreDirMap(config.IgnoreDirs)
	fileExts := buildFileExtMap(config.FileExtensions)

	indexOutputDir := defaultIndexDir
	if config.IndexDir != "" {
		indexOutputDir = config.IndexDir
	}

	if err := os.MkdirAll(indexOutputDir, 0o755); err != nil {
		return fmt.Errorf("creating index directory: %w", err)
	}

	for _, path := range config.Paths {
		opts := cmd.OptionsFromFlags()
		opts.IndexDir = indexOutputDir

		if config.Parallelism > 0 {
			opts.Parallelism = config.Parallelism
		}

		if config.RepoName != "" {
			if len(config.Paths) > 1 {
				baseDir := filepath.Base(path)
				opts.RepositoryDescription.Name = fmt.Sprintf("%s/%s", config.RepoName, baseDir)
			} else {
				opts.RepositoryDescription.Name = config.RepoName
			}
		}

		opts.RepositoryDescription.Source = path

		log.Printf("Indexing path: %s as repository: %s", path, opts.RepositoryDescription.Name)
		if err := indexArgWithFilters(path, *opts, ignoreDirMap, fileExts); err != nil {
			return fmt.Errorf("indexing %s: %w", path, err)
		}
	}

	return nil
}

func watchIndexConfig(configName, defaultIndexDir string, debounce time.Duration) error {
	if debounce <= 0 {
		return fmt.Errorf("watch_debounce must be greater than zero")
	}

	config, err := loadIndexConfig(configName)
	if err != nil {
		return err
	}
	if len(config.Paths) == 0 {
		return fmt.Errorf("configuration %q has no paths", configName)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	ignoreDirs := buildIgnoreDirMap(config.IgnoreDirs)
	fileExts := buildFileExtMap(config.FileExtensions)

	for _, root := range config.Paths {
		if err := addWatchTree(watcher, root, ignoreDirs); err != nil {
			return err
		}
	}

	log.Printf("watching %d path(s) from config %q", len(config.Paths), configName)

	if err := runIndexConfig(config, defaultIndexDir); err != nil {
		return err
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)

	changeCh := make(chan string, 1)
	doneCh := make(chan error, 1)

	queueChange := func(reason string) {
		select {
		case changeCh <- reason:
		default:
		}
	}

	var (
		timer         *time.Timer
		timerCh       <-chan time.Time
		pending       bool
		pendingReason string
		running       bool
	)

	startIndex := func(reason string) {
		running = true
		log.Printf("detected changes, reindexing (%s)", reason)
		go func() {
			doneCh <- runIndexConfig(config, defaultIndexDir)
		}()
	}

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}

			if shouldIgnoreWatchPath(event.Name, ignoreDirs) {
				continue
			}

			if event.Op&fsnotify.Create != 0 {
				info, err := os.Stat(event.Name)
				if err == nil && info.IsDir() {
					if err := addWatchTree(watcher, event.Name, ignoreDirs); err != nil {
						log.Printf("failed to add watcher for %s: %v", event.Name, err)
					}
					queueChange("directory create: " + event.Name)
					continue
				}
			}

			if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				_ = watcher.Remove(event.Name)
			}

			if watchEventRelevant(event, fileExts, ignoreDirs) {
				queueChange(event.String())
			}

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			if err != nil {
				log.Printf("watch error: %v", err)
			}

		case reason := <-changeCh:
			pending = true
			if pendingReason == "" {
				pendingReason = reason
			}
			if timer == nil {
				timer = time.NewTimer(debounce)
			} else {
				resetTimer(timer, debounce)
			}
			timerCh = timer.C

		case <-timerCh:
			timerCh = nil
			if pending && !running {
				reason := pendingReason
				pending = false
				pendingReason = ""
				startIndex(reason)
			}

		case err := <-doneCh:
			running = false
			if err != nil {
				log.Printf("reindex failed: %v", err)
			} else {
				log.Printf("reindex complete")
			}

			if pending {
				if timer == nil {
					timer = time.NewTimer(debounce)
				} else {
					resetTimer(timer, debounce)
				}
				timerCh = timer.C
			}

		case <-sigCh:
			log.Printf("stopping watch mode")
			return nil
		}
	}
}

func addWatchTree(watcher *fsnotify.Watcher, root string, ignoreDirs map[string]struct{}) error {
	root, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return err
	}

	return filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !info.IsDir() {
			return nil
		}
		if shouldIgnoreWatchPath(path, ignoreDirs) {
			return filepath.SkipDir
		}
		if err := watcher.Add(path); err != nil {
			return fmt.Errorf("adding watch for %s: %w", path, err)
		}
		return nil
	})
}

func shouldIgnoreWatchPath(path string, ignoreDirs map[string]struct{}) bool {
	if len(ignoreDirs) == 0 {
		return false
	}

	clean := filepath.Clean(path)
	for {
		base := filepath.Base(clean)
		if _, ok := ignoreDirs[base]; ok {
			return true
		}

		next := filepath.Dir(clean)
		if next == clean {
			return false
		}
		clean = next
	}
}

func watchEventRelevant(event fsnotify.Event, fileExts map[string]struct{}, ignoreDirs map[string]struct{}) bool {
	if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) == 0 {
		return false
	}
	if shouldIgnoreWatchPath(event.Name, ignoreDirs) {
		return false
	}

	info, err := os.Stat(event.Name)
	if err == nil && info.IsDir() {
		return true
	}

	if len(fileExts) == 0 {
		return true
	}

	_, ok := fileExts[strings.ToLower(filepath.Ext(event.Name))]
	return ok
}

func resetTimer(timer *time.Timer, delay time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(delay)
}

func indexArg(arg string, opts index.Options, ignore map[string]struct{}, fileExts map[string]struct{}) error {
	dir, err := filepath.Abs(filepath.Clean(arg))
	if err != nil {
		return err
	}

	opts.RepositoryDescription.Name = filepath.Base(dir)
	builder, err := index.NewBuilder(opts)
	if err != nil {
		return err
	}
	// we don't need to check error, since we either already have an error, or
	// we returning the first call to builder.Finish.
	defer builder.Finish() // nolint:errcheck

	comm := make(chan fileInfo, 100)
	agg := fileAggregator{
		ignoreDirs: ignore,
		sink:       comm,
		sizeMax:    int64(opts.SizeMax),
		fileExts:   fileExts,
	}

	go func() {
		if err := filepath.Walk(dir, agg.add); err != nil {
			log.Fatal(err)
		}
		close(comm)
	}()

	for f := range comm {
		displayName := displayNameForIndexedPath(dir, f.name)
		if f.size > int64(opts.SizeMax) && !opts.IgnoreSizeMax(displayName) {
			if err := builder.Add(index.Document{
				Name:       displayName,
				SkipReason: index.SkipReasonTooLarge,
			}); err != nil {
				return err
			}
			continue
		}
		content, err := os.ReadFile(f.name)
		if err != nil {
			return err
		}

		if err := builder.AddFile(displayName, content); err != nil {
			return err
		}
	}

	return builder.Finish()
}

func indexArgWithFilters(arg string, opts index.Options, ignore map[string]struct{}, fileExts map[string]struct{}) error {
	dir, err := filepath.Abs(filepath.Clean(arg))
	if err != nil {
		return err
	}

	if opts.RepositoryDescription.Name == "" {
		opts.RepositoryDescription.Name = filepath.Base(dir)
	}

	builder, err := index.NewBuilder(opts)
	if err != nil {
		return err
	}
	// we don't need to check error, since we either already have an error, or
	// we returning the first call to builder.Finish.
	defer builder.Finish() // nolint:errcheck

	comm := make(chan fileInfo, 100)
	agg := fileAggregator{
		ignoreDirs: ignore,
		sink:       comm,
		sizeMax:    int64(opts.SizeMax),
		fileExts:   fileExts,
	}

	go func() {
		if err := filepath.Walk(dir, agg.add); err != nil {
			log.Fatal(err)
		}
		close(comm)
	}()

	for f := range comm {
		displayName := displayNameForIndexedPath(dir, f.name)
		if f.size > int64(opts.SizeMax) && !opts.IgnoreSizeMax(displayName) {
			if err := builder.Add(index.Document{
				Name:       displayName,
				SkipReason: index.SkipReasonTooLarge,
			}); err != nil {
				return err
			}
			continue
		}
		content, err := os.ReadFile(f.name)
		if err != nil {
			return err
		}

		if err := builder.AddFile(displayName, content); err != nil {
			return err
		}
	}

	return builder.Finish()
}
