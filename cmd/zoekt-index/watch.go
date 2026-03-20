package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/pprof"
	"sort"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/cmd"
	"github.com/sourcegraph/zoekt/index"
)

type namedConfig struct {
	name   string
	config *IndexConfig
}

type watchSpec struct {
	name       string
	roots      []string
	ignoreDirs map[string]struct{}
	fileExts   map[string]struct{}
}

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

func loadNamedConfigs(configNames []string) ([]namedConfig, error) {
	configDir, err := getZoektConfigDir()
	if err != nil {
		return nil, err
	}

	return loadNamedConfigsFromDir(configDir, configNames)
}

func loadNamedConfigsFromDir(configDir string, configNames []string) ([]namedConfig, error) {
	if len(configNames) == 0 {
		return loadAllNamedConfigsFromDir(configDir)
	}

	seen := map[string]struct{}{}
	configs := make([]namedConfig, 0, len(configNames))
	for _, name := range configNames {
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		config, err := loadIndexConfigFromDir(configDir, name)
		if err != nil {
			return nil, err
		}
		configs = append(configs, namedConfig{name: name, config: config})
	}

	return configs, nil
}

func loadAllNamedConfigs() ([]namedConfig, error) {
	configDir, err := getZoektConfigDir()
	if err != nil {
		return nil, err
	}

	return loadAllNamedConfigsFromDir(configDir)
}

func loadAllNamedConfigsFromDir(configDir string) ([]namedConfig, error) {
	names, err := listConfigNamesFromDir(configDir)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no configuration files found")
	}

	configs := make([]namedConfig, 0, len(names))
	for _, name := range names {
		config, err := loadIndexConfigFromDir(configDir, name)
		if err != nil {
			return nil, err
		}
		configs = append(configs, namedConfig{name: name, config: config})
	}

	return configs, nil
}

func runNamedConfigs(configs []namedConfig, defaultIndexDir string, withProfiles bool) error {
	for _, cfg := range configs {
		if withProfiles && cfg.config.CPUProfile != "" {
			f, err := os.Create(cfg.config.CPUProfile)
			if err != nil {
				return err
			}
			if err := pprof.StartCPUProfile(f); err != nil {
				_ = f.Close()
				return err
			}
			err = runIndexConfig(cfg.name, cfg.config, defaultIndexDir)
			pprof.StopCPUProfile()
			_ = f.Close()
			if err != nil {
				return err
			}
			continue
		}

		if err := runIndexConfig(cfg.name, cfg.config, defaultIndexDir); err != nil {
			return err
		}
	}

	return nil
}

func runIndexConfig(configName string, config *IndexConfig, defaultIndexDir string) error {
	ignoreDirMap := buildIgnoreDirMap(config.IgnoreDirs)
	fileExts := buildFileExtMap(config.FileExtensions)

	indexOutputDir := defaultIndexDir
	if config.IndexDir != "" {
		indexOutputDir = config.IndexDir
	}

	if err := os.MkdirAll(indexOutputDir, 0o755); err != nil {
		return fmt.Errorf("creating index directory: %w", err)
	}

	repoName := configRepoName(configName, config)
	opts := cmd.OptionsFromFlags()
	opts.IndexDir = indexOutputDir
	if config.Parallelism > 0 {
		opts.Parallelism = config.Parallelism
	}
	opts.RepositoryDescription.Name = repoName
	opts.RepositoryDescription.Source = "config:" + configName

	log.Printf("Indexing config %q as repository: %s", configName, repoName)
	if err := indexConfigPaths(config, *opts, ignoreDirMap, fileExts); err != nil {
		return err
	}
	if err := cleanupLegacyConfigShards(config, indexOutputDir, repoName); err != nil {
		return err
	}

	return nil
}

func watchConfigs(configNames []string, defaultIndexDir string, debounce time.Duration) error {
	if debounce <= 0 {
		return fmt.Errorf("watch_debounce must be greater than zero")
	}

	configs, err := loadNamedConfigs(configNames)
	if err != nil {
		return err
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	specs := make([]watchSpec, 0, len(configs))
	for _, cfg := range configs {
		spec, err := newWatchSpec(cfg)
		if err != nil {
			return err
		}
		specs = append(specs, spec)
	}

	if err := addWatchSpecs(watcher, specs); err != nil {
		return err
	}

	if len(configNames) == 0 {
		log.Printf("watching %d config(s)", len(configs))
	} else {
		log.Printf("watching %d named config(s): %s", len(configs), strings.Join(configNames, ", "))
	}

	if err := runNamedConfigs(configs, defaultIndexDir, false); err != nil {
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
			doneCh <- runNamedConfigs(configs, defaultIndexDir, false)
		}()
	}

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}

			if shouldIgnoreWatchPathForAllSpecs(event.Name, specs) {
				continue
			}

			if event.Op&fsnotify.Create != 0 {
				info, err := os.Stat(event.Name)
				if err == nil && info.IsDir() {
					if err := addWatchDirForSpecs(watcher, event.Name, specs); err != nil {
						log.Printf("failed to add watcher for %s: %v", event.Name, err)
					}
					queueChange("directory create: " + event.Name)
					continue
				}
			}

			if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				_ = watcher.Remove(event.Name)
			}

			if watchEventRelevantForSpecs(event, specs) {
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
	return addWatchTreeDedup(watcher, root, ignoreDirs, nil)
}

func newWatchSpec(cfg namedConfig) (watchSpec, error) {
	roots := make([]string, 0, len(cfg.config.Paths))
	for _, root := range cfg.config.Paths {
		abs, err := filepath.Abs(filepath.Clean(root))
		if err != nil {
			return watchSpec{}, err
		}
		roots = append(roots, abs)
	}
	if len(roots) == 0 {
		return watchSpec{}, fmt.Errorf("configuration %q has no paths", cfg.name)
	}

	return watchSpec{
		name:       cfg.name,
		roots:      roots,
		ignoreDirs: buildIgnoreDirMap(cfg.config.IgnoreDirs),
		fileExts:   buildFileExtMap(cfg.config.FileExtensions),
	}, nil
}

func addWatchSpecs(watcher *fsnotify.Watcher, specs []watchSpec) error {
	added := map[string]struct{}{}
	for _, spec := range specs {
		for _, root := range spec.roots {
			if err := addWatchTreeDedup(watcher, root, spec.ignoreDirs, added); err != nil {
				return err
			}
		}
	}
	return nil
}

func addWatchDirForSpecs(watcher *fsnotify.Watcher, dir string, specs []watchSpec) error {
	added := map[string]struct{}{}
	for _, spec := range specs {
		if !pathWithinRoots(dir, spec.roots) {
			continue
		}
		if err := addWatchTreeDedup(watcher, dir, spec.ignoreDirs, added); err != nil {
			return err
		}
	}
	return nil
}

func addWatchTreeDedup(watcher *fsnotify.Watcher, root string, ignoreDirs map[string]struct{}, added map[string]struct{}) error {
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
		if added != nil {
			if _, ok := added[path]; ok {
				return nil
			}
			added[path] = struct{}{}
		}
		if err := watcher.Add(path); err != nil {
			return fmt.Errorf("adding watch for %s: %w", path, err)
		}
		return nil
	})
}

func pathWithinRoots(path string, roots []string) bool {
	for _, root := range roots {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			continue
		}
		if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			return true
		}
	}
	return false
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

func shouldIgnoreWatchPathForAllSpecs(path string, specs []watchSpec) bool {
	if !pathWithinAnySpec(path, specs) {
		return true
	}
	for _, spec := range specs {
		if pathWithinRoots(path, spec.roots) && !shouldIgnoreWatchPath(path, spec.ignoreDirs) {
			return false
		}
	}
	return true
}

func pathWithinAnySpec(path string, specs []watchSpec) bool {
	for _, spec := range specs {
		if pathWithinRoots(path, spec.roots) {
			return true
		}
	}
	return false
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

func watchEventRelevantForSpecs(event fsnotify.Event, specs []watchSpec) bool {
	for _, spec := range specs {
		if !pathWithinRoots(event.Name, spec.roots) {
			continue
		}
		if watchEventRelevant(event, spec.fileExts, spec.ignoreDirs) {
			return true
		}
	}
	return false
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

func configRepoName(configName string, config *IndexConfig) string {
	if config.RepoName != "" {
		return config.RepoName
	}
	if len(config.Paths) == 1 {
		return filepath.Base(filepath.Clean(config.Paths[0]))
	}
	return configName
}

func legacyConfigRepoNames(config *IndexConfig) []string {
	if len(config.Paths) <= 1 {
		return nil
	}

	seen := map[string]struct{}{}
	var names []string
	for _, path := range config.Paths {
		baseDir := filepath.Base(filepath.Clean(path))
		name := baseDir
		if config.RepoName != "" {
			name = fmt.Sprintf("%s/%s", config.RepoName, baseDir)
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func cleanupLegacyConfigShards(config *IndexConfig, indexDir, currentRepoName string) error {
	for _, legacyName := range legacyConfigRepoNames(config) {
		if legacyName == currentRepoName {
			continue
		}

		opts := index.Options{
			IndexDir: indexDir,
			RepositoryDescription: zoekt.Repository{
				Name: legacyName,
			},
		}

		for _, shard := range opts.FindAllShards() {
			paths, err := index.IndexFilePaths(shard)
			if err != nil {
				return fmt.Errorf("finding artifact paths for legacy shard %s: %w", shard, err)
			}
			for _, path := range paths {
				log.Printf("removing legacy config shard file: %s", path)
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					return fmt.Errorf("removing legacy shard file %s: %w", path, err)
				}
			}
		}
	}
	return nil
}

func indexConfigPaths(config *IndexConfig, opts index.Options, ignore map[string]struct{}, fileExts map[string]struct{}) error {
	builder, err := index.NewBuilder(opts)
	if err != nil {
		return err
	}
	defer builder.Finish() // nolint:errcheck

	prefixes, err := configPathPrefixes(config.Paths)
	if err != nil {
		return err
	}

	for i, path := range config.Paths {
		log.Printf("adding path to repository %s: %s", opts.RepositoryDescription.Name, path)
		if err := addPathToBuilder(builder, path, prefixes[i], opts, ignore, fileExts); err != nil {
			return fmt.Errorf("indexing %s: %w", path, err)
		}
	}

	return builder.Finish()
}

func configPathPrefixes(paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	if len(paths) == 1 {
		return []string{""}, nil
	}

	type pathParts struct {
		original string
		parts    []string
	}

	pp := make([]pathParts, 0, len(paths))
	for _, path := range paths {
		abs, err := filepath.Abs(filepath.Clean(path))
		if err != nil {
			return nil, err
		}
		pp = append(pp, pathParts{
			original: path,
			parts:    splitPathParts(abs),
		})
	}

	prefixes := make([]string, len(paths))
	depths := make([]int, len(pp))
	for i := range depths {
		depths[i] = 1
	}

	for {
		changed := false
		seen := map[string]int{}
		for i, item := range pp {
			if depths[i] > len(item.parts) {
				prefixes[i] = fmt.Sprintf("path-%d", i+1)
				continue
			}

			start := len(item.parts) - depths[i]
			candidate := filepath.ToSlash(filepath.Join(item.parts[start:]...))
			prefixes[i] = candidate

			if first, exists := seen[candidate]; exists {
				if depths[i] < len(item.parts) {
					depths[i]++
					changed = true
				} else if depths[first] < len(pp[first].parts) {
					depths[first]++
					changed = true
				}
			} else {
				seen[candidate] = i
			}
		}

		if !changed {
			break
		}
	}

	return prefixes, nil
}

func splitPathParts(path string) []string {
	vol := filepath.VolumeName(path)
	trimmed := path[len(vol):]
	trimmed = strings.TrimPrefix(trimmed, string(filepath.Separator))
	if trimmed == "" {
		if vol == "" {
			return nil
		}
		return []string{strings.TrimSuffix(vol, ":")}
	}

	parts := strings.Split(trimmed, string(filepath.Separator))
	if vol != "" {
		parts[0] = strings.TrimSuffix(vol, ":") + "/" + parts[0]
	}
	return parts
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
	defer builder.Finish() // nolint:errcheck

	if err := addPathToBuilder(builder, dir, "", opts, ignore, fileExts); err != nil {
		return err
	}

	return builder.Finish()
}

func addPathToBuilder(builder *index.Builder, arg, pathPrefix string, opts index.Options, ignore map[string]struct{}, fileExts map[string]struct{}) error {
	dir, err := filepath.Abs(filepath.Clean(arg))
	if err != nil {
		return err
	}

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
		if pathPrefix != "" {
			displayName = filepath.ToSlash(filepath.Join(pathPrefix, displayName))
		}
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

	return nil
}

func indexArgWithFilters(arg string, opts index.Options, ignore map[string]struct{}, fileExts map[string]struct{}) error {
	if opts.RepositoryDescription.Name == "" {
		opts.RepositoryDescription.Name = filepath.Base(filepath.Clean(arg))
	}
	return indexArg(arg, opts, ignore, fileExts)
}

func nukeIndexDir(indexDir string) error {
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		return err
	}

	entries, err := os.ReadDir(indexDir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		name := entry.Name()
		if !strings.Contains(name, ".zoekt") && !strings.HasSuffix(name, ".tmp") {
			continue
		}

		path := filepath.Join(indexDir, name)
		log.Printf("removing index artifact: %s", path)
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}

	return nil
}
