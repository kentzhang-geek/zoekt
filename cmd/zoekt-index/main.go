// Copyright 2016 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Command zoekt-index indexes a directory of files.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"time"

	"go.uber.org/automaxprocs/maxprocs"

	"github.com/sourcegraph/zoekt/cmd"
)

type fileInfo struct {
	name string
	size int64
}

type fileAggregator struct {
	ignoreDirs map[string]struct{}
	sizeMax    int64
	sink       chan fileInfo
	fileExts   map[string]struct{} // Map of allowed file extensions
}

// IndexConfig represents a JSON configuration for indexing
type IndexConfig struct {
	// Paths to index
	Paths []string `json:"paths"`
	// Directories to ignore when indexing
	IgnoreDirs string `json:"ignore_dirs"`
	// CPU profile path, if profiling is desired
	CPUProfile string `json:"cpu_profile"`
	// Repository name (overrides directory name)
	RepoName string `json:"repo_name"`
	// File extensions to include (pipe-separated, without dots)
	FileExtensions string `json:"file_extensions"`
	// Custom index output directory (defaults to ~/.zoekt/indexdb)
	IndexDir string `json:"index_dir"`
	// Parallelism factor for indexing
	Parallelism int `json:"parallelism"`
}

func (a *fileAggregator) add(path string, info os.FileInfo, err error) error {
	if err != nil {
		return err
	}

	if info.IsDir() {
		base := filepath.Base(path)
		if _, ok := a.ignoreDirs[base]; ok {
			return filepath.SkipDir
		}
	}

	if info.Mode().IsRegular() {
		// Apply file extension filter if configured
		if len(a.fileExts) > 0 {
			ext := strings.ToLower(filepath.Ext(path))
			if _, ok := a.fileExts[ext]; !ok {
				return nil // Skip files with extensions not in the whitelist
			}
		}

		a.sink <- fileInfo{path, info.Size()}
	}
	return nil
}

// getZoektConfigDir returns the path to the .zoekt config directory
func getZoektConfigDir() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %v", err)
	}

	configDir := filepath.Join(homeDir, ".zoekt")

	// Create directory if it doesn't exist
	if _, err := os.Stat(configDir); os.IsNotExist(err) {
		if err := os.MkdirAll(configDir, 0755); err != nil {
			return "", fmt.Errorf("failed to create .zoekt directory: %v", err)
		}
	}

	return configDir, nil
}

// Add this function to create the indexdb directory
func ensureIndexDir() (string, error) {
	configDir, err := getZoektConfigDir()
	if err != nil {
		return "", err
	}

	// Create indexdb subdirectory
	indexDir := filepath.Join(configDir, "indexdb")
	if err := os.MkdirAll(indexDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create index directory: %v", err)
	}

	return indexDir, nil
}

// loadIndexConfig loads a named configuration file from .zoekt directory
func loadIndexConfig(name string) (*IndexConfig, error) {
	configDir, err := getZoektConfigDir()
	if err != nil {
		return nil, err
	}

	configPath := filepath.Join(configDir, name+".json")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("configuration file not found: %s", configPath)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %v", err)
	}

	var config IndexConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %v", err)
	}

	// Set defaults if not specified
	if config.IgnoreDirs == "" {
		config.IgnoreDirs = ".git,.hg,.svn"
	}

	return &config, nil
}

// listConfigs lists all available configuration files in the .zoekt directory
func listConfigs() error {
	configDir, err := getZoektConfigDir()
	if err != nil {
		return err
	}

	files, err := os.ReadDir(configDir)
	if err != nil {
		return fmt.Errorf("failed to read config directory: %v", err)
	}

	fmt.Println("Available configurations:")
	fmt.Println("------------------------")

	found := false
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
			continue
		}

		configName := strings.TrimSuffix(file.Name(), ".json")
		config, err := loadIndexConfig(configName)
		if err != nil {
			fmt.Printf("- %s (Error: %v)\n", configName, err)
			continue
		}

		// Display configuration details
		fmt.Printf("- %s:\n", configName)

		// Show repo name if specified
		if config.RepoName != "" {
			fmt.Printf("  Repo Name: %s\n", config.RepoName)
		}

		// show Parallelism
		if config.Parallelism > 0 {
			fmt.Printf("  Parallelism: %d\n", config.Parallelism)
		}

		fmt.Printf("  Paths: %d directories\n", len(config.Paths))
		if len(config.Paths) > 0 {
			for i, path := range config.Paths {
				if i < 3 { // Show up to 3 paths to avoid clutter
					fmt.Printf("    - %s\n", path)
				} else {
					fmt.Printf("    - ... and %d more\n", len(config.Paths)-3)
					break
				}
			}
		}

		fmt.Printf("  Ignore: %s\n", config.IgnoreDirs)

		// Show file extensions if specified
		if config.FileExtensions != "" {
			fmt.Printf("  File Extensions: %s\n", config.FileExtensions)
		}

		found = true
	}

	if !found {
		fmt.Println("No configuration files found.")
		fmt.Printf("Create JSON files in %s to get started.\n", configDir)
	}

	return nil
}

func printUsage() {
	fmt.Fprintf(flag.CommandLine.Output(), "USAGE:\n")
	fmt.Fprintf(flag.CommandLine.Output(), "  %s [options] PATHS...\n", filepath.Base(os.Args[0]))
	fmt.Fprintf(flag.CommandLine.Output(), "  %s update <config-name>\n", filepath.Base(os.Args[0]))
	fmt.Fprintf(flag.CommandLine.Output(), "  %s watch <config-name>\n", filepath.Base(os.Args[0]))
	fmt.Fprintf(flag.CommandLine.Output(), "  %s list\n\n", filepath.Base(os.Args[0]))
	fmt.Fprintln(flag.CommandLine.Output(), "Options:")
	flag.PrintDefaults()
	fmt.Fprintln(flag.CommandLine.Output(), "\nBy default, index files are stored in ~/.zoekt/indexdb")
}

func main() {
	cpuProfile := flag.String("cpu_profile", "", "write cpu profile to file")
	ignoreDirs := flag.String("ignore_dirs", ".git,.hg,.svn", "comma separated list of directories to ignore.")
	metaFile := flag.String("meta", "", "path to .meta JSON file with repository description")
	// Add output index directory flag with default to our custom location
	indexDir := flag.String("index_dir", "", "directory to write index files (defaults to ~/.zoekt/indexdb)")
	watchDebounce := flag.Duration("watch_debounce", 3*time.Second, "delay before reindexing after filesystem changes in watch mode")
	flag.Parse()

	// Create and ensure index directory exists
	defaultIndexDir, err := ensureIndexDir()
	if err != nil {
		log.Fatalf("Failed to create index directory: %v", err)
	}

	// Use custom dir if provided, otherwise use default
	outputIndexDir := defaultIndexDir
	if *indexDir != "" {
		outputIndexDir = *indexDir
	}

	// Check for the "list" subcommand
	if flag.NArg() >= 1 && flag.Arg(0) == "list" {
		if err := listConfigs(); err != nil {
			log.Fatalf("Failed to list configurations: %v", err)
		}
		return
	}

	// Check for the "update" subcommand
	if flag.NArg() >= 2 && flag.Arg(0) == "update" {
		configName := flag.Arg(1)
		config, err := loadIndexConfig(configName)
		if err != nil {
			log.Fatalf("Failed to load configuration: %v", err)
		}

		// Set up CPU profiling if requested in config
		if config.CPUProfile != "" {
			f, err := os.Create(config.CPUProfile)
			if err != nil {
				log.Fatal(err)
			}
			if err := pprof.StartCPUProfile(f); err != nil {
				log.Fatal(err)
			}
			defer pprof.StopCPUProfile()
		}

		if err := runIndexConfig(config, outputIndexDir); err != nil {
			log.Fatalf("Failed to update configuration: %v", err)
		}

		return
	}

	if flag.NArg() >= 2 && flag.Arg(0) == "watch" {
		configName := flag.Arg(1)
		if err := watchIndexConfig(configName, outputIndexDir, *watchDebounce); err != nil {
			log.Fatalf("watch failed: %v", err)
		}

		return
	}

	// Original command-line based path
	if flag.NArg() == 0 {
		printUsage()
		os.Exit(1)
	}

	// Tune GOMAXPROCS to match Linux container CPU quota.
	_, _ = maxprocs.Set()

	opts := cmd.OptionsFromFlags()
	// Set the index output directory
	opts.IndexDir = outputIndexDir

	if *cpuProfile != "" {
		f, err := os.Create(*cpuProfile)
		if err != nil {
			log.Fatal(err)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			log.Fatal(err)
		}
		defer pprof.StopCPUProfile()
	}

	ignoreDirMap := buildIgnoreDirMap(*ignoreDirs)

	if *metaFile != "" {
		// Read and parse the .meta JSON file into opts.RepositoryDescription
		data, err := os.ReadFile(*metaFile)
		if err != nil {
			log.Fatalf("failed to read .meta file %s: %v", *metaFile, err)
		}
		if err := json.Unmarshal(data, &opts.RepositoryDescription); err != nil {
			log.Fatalf("failed to decode .meta file %s: %v", *metaFile, err)
		}
	}

	for _, arg := range flag.Args() {
		opts.RepositoryDescription.Source = arg
		if err := indexArgWithFilters(arg, *opts, ignoreDirMap, nil); err != nil {
			log.Fatal(err)
		}
	}
}
