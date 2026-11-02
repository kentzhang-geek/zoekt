// Research-only Windows indexing probe. All index writes require a fresh output directory.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/index"
	"github.com/sourcegraph/zoekt/query"
	"golang.org/x/sys/windows"
)

type config struct {
	Paths       []string
	IgnoreDirs  string `json:"ignore_dirs"`
	Extensions  string `json:"file_extensions"`
	Parallelism int
}
type entry struct {
	Path, Name string
	Size       int64
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
func emit(v any) { must(json.NewEncoder(os.Stdout).Encode(v)) }
func cpu() float64 {
	var c, e, k, u windows.Filetime
	must(windows.GetProcessTimes(windows.CurrentProcess(), &c, &e, &k, &u))
	return float64((uint64(k.HighDateTime)<<32|uint64(k.LowDateTime))+(uint64(u.HighDateTime)<<32|uint64(u.LowDateTime))) / 1e7
}
func measure(name string, fn func() error) {
	runtime.GC()
	t, c := time.Now(), cpu()
	pprof.Do(context.Background(), pprof.Labels("stage", name), func(context.Context) { must(fn()) })
	wall, used := time.Since(t).Seconds(), cpu()-c
	emit(map[string]any{"stage": name, "wall_s": wall, "cpu_s": used, "average_cores": used / wall})
}
func main() {
	mode := flag.String("mode", "scan", "scan, stages, build, verify, or metadata")
	cfgPath := flag.String("config", "", "JSON configuration for file filters and roots")
	out := flag.String("out", "", "fresh index output directory")
	workers := flag.Int("workers", 20, "worker count")
	shardMiB := flag.Int("shard-mib", 16, "shard/task source size")
	baselineMiB := flag.Int("baseline-shard-mib", 100, "existing builder shard size")
	profile := flag.String("profile", "", "CPU profile")
	flag.Parse()
	if *workers < 1 || *shardMiB < 1 || *baselineMiB < 1 {
		log.Fatal("worker and shard limits must be positive")
	}
	emit(map[string]any{"go": runtime.Version(), "cpus": runtime.NumCPU(), "gomaxprocs": runtime.GOMAXPROCS(0), "mode": *mode})
	if *mode == "verify" {
		for _, root := range flag.Args() {
			var rows []string
			seen := map[string]bool{}
			must(filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() || !strings.HasSuffix(p, ".zoekt") {
					return nil
				}
				f, err := os.Open(p)
				if err != nil {
					return err
				}
				defer f.Close()
				inf, err := index.NewIndexFile(f)
				if err != nil {
					return err
				}
				s, err := index.NewSearcher(inf)
				if err != nil {
					inf.Close()
					return err
				}
				defer s.Close()
				r, err := s.Search(context.Background(), &query.Const{Value: true}, &zoekt.SearchOptions{Whole: true, ShardMaxMatchCount: 10000000, TotalMaxMatchCount: 10000000})
				if err != nil {
					return err
				}
				if r.Stats.FilesSkipped != 0 || r.Stats.ShardsSkipped != 0 || r.Stats.Crashes != 0 {
					return fmt.Errorf("incomplete verification: %s", p)
				}
				for _, fm := range r.Files {
					if seen[fm.FileName] {
						return fmt.Errorf("duplicate file: %s", fm.FileName)
					}
					seen[fm.FileName] = true
					sum := sha256.Sum256(fm.Content)
					rows = append(rows, fmt.Sprintf("%s\x00%s\x00%x", fm.FileName, fm.Language, sum))
				}
				return nil
			}))
			sort.Strings(rows)
			h := sha256.New()
			for _, row := range rows {
				fmt.Fprintf(h, "%d:%s", len(row), row)
			}
			emit(map[string]any{"root": root, "files": len(rows), "name_language_content_sha256": fmt.Sprintf("%x", h.Sum(nil))})
		}
		return
	}
	if *mode == "metadata" {
		for _, p := range flag.Args() {
			rs, m, err := index.ReadMetadataPath(p)
			must(err)
			for _, r := range rs {
				emit(map[string]any{"path": p, "name": r.Name, "has_symbols": r.HasSymbols, "index_time": m.IndexTime, "version": m.ZoektVersion, "options_hash": r.IndexOptions})
			}
		}
		return
	}
	cfg := config{Paths: flag.Args(), IgnoreDirs: ".git,.hg,.svn,node_modules"}
	if *cfgPath != "" {
		b, err := os.ReadFile(*cfgPath)
		must(err)
		must(json.Unmarshal(b, &cfg))
		if len(flag.Args()) > 0 {
			cfg.Paths = flag.Args()
		}
	}
	if len(cfg.Paths) == 0 {
		log.Fatal("no roots")
	}
	ext, ignore := map[string]bool{}, map[string]bool{}
	for _, x := range strings.Split(cfg.Extensions, "|") {
		if x != "" {
			ext["."+strings.ToLower(x)] = true
		}
	}
	for _, x := range strings.Split(cfg.IgnoreDirs, ",") {
		ignore[x] = true
	}
	emit(map[string]any{"config_parallelism": cfg.Parallelism, "roots": cfg.Paths})
	var files []entry
	var entries, dirs, large int
	var bytes int64
	scan := func(walkdir bool) error {
		files = nil
		entries = 0
		dirs = 0
		large = 0
		bytes = 0
		for _, root := range cfg.Paths {
			add := func(p string, info fs.FileInfo) error {
				if !info.Mode().IsRegular() {
					return nil
				}
				rel, err := filepath.Rel(root, p)
				if err != nil {
					return err
				}
				files = append(files, entry{p, filepath.ToSlash(filepath.Join(filepath.Base(root), rel)), info.Size()})
				bytes += info.Size()
				if info.Size() > 2<<20 {
					large++
				}
				return nil
			}
			var err error
			if walkdir {
				err = filepath.WalkDir(root, func(p string, d fs.DirEntry, e error) error {
					if e != nil {
						return e
					}
					entries++
					if d.IsDir() {
						dirs++
						if ignore[d.Name()] {
							return filepath.SkipDir
						}
						return nil
					}
					if len(ext) > 0 && !ext[strings.ToLower(filepath.Ext(p))] {
						return nil
					}
					info, e := d.Info()
					if e != nil {
						return e
					}
					return add(p, info)
				})
			} else {
				err = filepath.Walk(root, func(p string, info fs.FileInfo, e error) error {
					if e != nil {
						return e
					}
					entries++
					if info.IsDir() {
						dirs++
						if ignore[info.Name()] {
							return filepath.SkipDir
						}
						return nil
					}
					if len(ext) > 0 && !ext[strings.ToLower(filepath.Ext(p))] {
						return nil
					}
					return add(p, info)
				})
			}
			if err != nil {
				return err
			}
		}
		emit(map[string]any{"entries": entries, "dirs": dirs, "files": len(files), "bytes": bytes, "over_2mib": large})
		return nil
	}
	if *mode == "scan" {
		measure("walk", func() error { return scan(false) })
		measure("walkdir", func() error { return scan(true) })
		return
	}
	measure("walkdir", func() error { return scan(true) })
	if *profile != "" {
		f, err := os.Create(*profile)
		must(err)
		must(pprof.StartCPUProfile(f))
		defer func() { pprof.StopCPUProfile(); must(f.Close()) }()
	}
	if *mode == "stages" {
		for _, n := range []int{1, 4, *workers} {
			measure(fmt.Sprintf("read_check_%d", n), func() error {
				jobs := make(chan entry)
				errs := make(chan error, n)
				var wg sync.WaitGroup
				for i := 0; i < n; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						var check index.DocChecker
						for f := range jobs {
							if f.Size > 2<<20 {
								continue
							}
							b, err := os.ReadFile(f.Path)
							if err != nil {
								errs <- err
								return
							}
							check.Check(b, 20000, false)
						}
					}()
				}
				// Buffered errors are checked between dispatches to prevent a failed worker from stalling the probe.
				for _, f := range files {
					select {
					case jobs <- f:
					case err := <-errs:
						close(jobs)
						wg.Wait()
						return err
					}
				}
				close(jobs)
				wg.Wait()
				select {
				case err := <-errs:
					return err
				default:
					return nil
				}
			})
		}
		return
	}
	if *mode != "build" {
		log.Fatal("invalid mode")
	}
	if *out == "" {
		log.Fatal("-out is required")
	}
	if _, err := os.Stat(*out); !os.IsNotExist(err) {
		log.Fatal("output directory must not exist")
	}
	must(os.MkdirAll(*out, 0700))
	build := func(batch []entry, dest string, parallel, limit int) error {
		opts := index.Options{IndexDir: dest, Parallelism: parallel, ShardMax: limit, DisableCTags: true, RepositoryDescription: zoekt.Repository{Name: "probe"}}
		opts.SetDefaults()
		b, err := index.NewBuilder(opts)
		if err != nil {
			return err
		}
		for _, f := range batch {
			doc := index.Document{Name: f.Name}
			if f.Size > int64(opts.SizeMax) {
				doc.SkipReason = index.SkipReasonTooLarge
			} else {
				doc.Content, err = os.ReadFile(f.Path)
				if err != nil {
					return err
				}
			}
			if err = b.Add(doc); err != nil {
				return err
			}
		}
		return b.Finish()
	}
	// Existing builder: serialized read/check followed by parallel shard construction.
	measure("existing", func() error { return build(files, filepath.Join(*out, "existing"), *workers, *baselineMiB<<20) })
	var batches [][]entry
	start := 0
	var size int64
	for i, f := range files {
		size += int64(len(f.Name))
		if f.Size <= 2<<20 {
			size += f.Size
		}
		if size > int64(*shardMiB)<<20 {
			batches = append(batches, files[start:i+1])
			start = i + 1
			size = 0
		}
	}
	if start < len(files) {
		batches = append(batches, files[start:])
	}
	emit(map[string]any{"tasks": len(batches), "workers": *workers, "shard_mib": *shardMiB})
	// Independent builders prove the complete read/check/build pipeline can be parallelized.
	// Separate task directories avoid unsafe shared publication; this is not production integration.
	measure("task_parallel", func() error {
		jobs := make(chan int)
		errs := make(chan error, *workers)
		var wg sync.WaitGroup
		for w := 0; w < *workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range jobs {
					if err := build(batches[i], filepath.Join(*out, "tasks", fmt.Sprintf("task-%04d", i)), 1, *shardMiB<<20); err != nil {
						errs <- err
						return
					}
				}
			}()
		}
		for i := range batches {
			select {
			case jobs <- i:
			case err := <-errs:
				close(jobs)
				wg.Wait()
				return err
			}
		}
		close(jobs)
		wg.Wait()
		select {
		case err := <-errs:
			return err
		default:
			return nil
		}
	})
}
