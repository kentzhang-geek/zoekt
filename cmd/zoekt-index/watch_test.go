package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/fsnotify/fsnotify"
)

func TestDisplayNameForIndexedPath(t *testing.T) {
	root := filepath.Join("C:", "repo")
	path := filepath.Join(root, "src", "main.go")

	got := displayNameForIndexedPath(root, path)
	want := "src/main.go"
	if got != want {
		t.Fatalf("displayNameForIndexedPath() = %q, want %q", got, want)
	}
}

func TestShouldIgnoreWatchPath(t *testing.T) {
	ignore := buildIgnoreDirMap(".git,node_modules")

	cases := []struct {
		path string
		want bool
	}{
		{path: filepath.Join("repo", ".git", "config"), want: true},
		{path: filepath.Join("repo", "node_modules", "pkg", "index.js"), want: true},
		{path: filepath.Join("repo", "src", "main.go"), want: false},
	}

	for _, tc := range cases {
		if got := shouldIgnoreWatchPath(tc.path, ignore); got != tc.want {
			t.Fatalf("shouldIgnoreWatchPath(%q) = %t, want %t", tc.path, got, tc.want)
		}
	}
}

func TestWatchEventRelevant(t *testing.T) {
	tempDir := t.TempDir()
	subDir := filepath.Join(tempDir, "src")
	if err := os.Mkdir(subDir, 0o755); err != nil {
		t.Fatal(err)
	}

	fileExts := buildFileExtMap("go|txt")
	ignore := buildIgnoreDirMap(".git")

	cases := []struct {
		name  string
		event fsnotify.Event
		want  bool
	}{
		{
			name:  "allowed extension",
			event: fsnotify.Event{Name: filepath.Join(tempDir, "main.go"), Op: fsnotify.Write},
			want:  true,
		},
		{
			name:  "filtered extension",
			event: fsnotify.Event{Name: filepath.Join(tempDir, "main.bin"), Op: fsnotify.Write},
			want:  false,
		},
		{
			name:  "directory create",
			event: fsnotify.Event{Name: subDir, Op: fsnotify.Create},
			want:  true,
		},
		{
			name:  "ignored directory",
			event: fsnotify.Event{Name: filepath.Join(tempDir, ".git", "index"), Op: fsnotify.Write},
			want:  false,
		},
		{
			name:  "chmod ignored",
			event: fsnotify.Event{Name: filepath.Join(tempDir, "main.go"), Op: fsnotify.Chmod},
			want:  false,
		},
	}

	for _, tc := range cases {
		if got := watchEventRelevant(tc.event, fileExts, ignore); got != tc.want {
			t.Fatalf("%s: watchEventRelevant() = %t, want %t", tc.name, got, tc.want)
		}
	}
}

func TestConfigRepoName(t *testing.T) {
	cases := []struct {
		name       string
		configName string
		config     IndexConfig
		want       string
	}{
		{
			name:       "explicit repo name wins",
			configName: "ue",
			config: IndexConfig{
				RepoName: "UE",
				Paths:    []string{"one", "two"},
			},
			want: "UE",
		},
		{
			name:       "single path defaults to basename",
			configName: "ignored",
			config: IndexConfig{
				Paths: []string{filepath.Join("D:", "code", "Engine")},
			},
			want: "Engine",
		},
		{
			name:       "multi path defaults to config name",
			configName: "ue",
			config: IndexConfig{
				Paths: []string{"one", "two"},
			},
			want: "ue",
		},
	}

	for _, tc := range cases {
		if got := configRepoName(tc.configName, &tc.config); got != tc.want {
			t.Fatalf("%s: configRepoName() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestLegacyConfigRepoNames(t *testing.T) {
	config := &IndexConfig{
		RepoName: "UE",
		Paths: []string{
			filepath.Join("D:", "code", "Engine"),
			filepath.Join("D:", "code", "Templates"),
			filepath.Join("D:", "other", "Engine"),
		},
	}

	got := legacyConfigRepoNames(config)
	want := []string{"UE/Engine", "UE/Templates"}
	if !slices.Equal(got, want) {
		t.Fatalf("legacyConfigRepoNames() = %#v, want %#v", got, want)
	}
}

func TestConfigPathPrefixes(t *testing.T) {
	tempDir := t.TempDir()
	paths := []string{
		filepath.Join(tempDir, "Engine"),
		filepath.Join(tempDir, "Samples", "Engine"),
		filepath.Join(tempDir, "Templates"),
	}

	got, err := configPathPrefixes(paths)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"Engine", "Samples/Engine", "Templates"}
	if !slices.Equal(got, want) {
		t.Fatalf("configPathPrefixes() = %#v, want %#v", got, want)
	}
}

func TestListConfigNamesFromDir(t *testing.T) {
	tempDir := t.TempDir()
	for _, name := range []string{"b.json", "a.json", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(tempDir, name), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := listConfigNamesFromDir(tempDir)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"a", "b"}
	if !slices.Equal(got, want) {
		t.Fatalf("listConfigNamesFromDir() = %#v, want %#v", got, want)
	}
}

func TestNukeIndexDir(t *testing.T) {
	tempDir := t.TempDir()
	files := []string{
		"repo_v17.00000.zoekt",
		"repo_v17.00000.zoekt.meta",
		"orphan.tmp",
		"keep.txt",
	}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(tempDir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := nukeIndexDir(tempDir); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(tempDir, "keep.txt")); err != nil {
		t.Fatalf("keep.txt should remain: %v", err)
	}

	for _, name := range []string{"repo_v17.00000.zoekt", "repo_v17.00000.zoekt.meta", "orphan.tmp"} {
		if _, err := os.Stat(filepath.Join(tempDir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s should be removed, got err=%v", name, err)
		}
	}
}

func TestLoadNamedConfigsFromDir(t *testing.T) {
	tempDir := t.TempDir()
	files := map[string]string{
		"ue.json":     `{"repo_name":"UE","paths":["one"]}`,
		"pbrtv3.json": `{"repo_name":"PBRTv3","paths":["two"]}`,
		"pbrtv4.json": `{"repo_name":"PBRTv4","paths":["three"]}`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(tempDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("all configs when empty", func(t *testing.T) {
		got, err := loadNamedConfigsFromDir(tempDir, nil)
		if err != nil {
			t.Fatal(err)
		}

		var names []string
		for _, cfg := range got {
			names = append(names, cfg.name)
		}

		want := []string{"pbrtv3", "pbrtv4", "ue"}
		if !slices.Equal(names, want) {
			t.Fatalf("loadNamedConfigsFromDir(nil) names = %#v, want %#v", names, want)
		}
	})

	t.Run("selected configs preserve input order and dedupe", func(t *testing.T) {
		got, err := loadNamedConfigsFromDir(tempDir, []string{"ue", "pbrtv4", "ue"})
		if err != nil {
			t.Fatal(err)
		}

		var names []string
		for _, cfg := range got {
			names = append(names, cfg.name)
		}

		want := []string{"ue", "pbrtv4"}
		if !slices.Equal(names, want) {
			t.Fatalf("loadNamedConfigsFromDir(selected) names = %#v, want %#v", names, want)
		}
	})
}
