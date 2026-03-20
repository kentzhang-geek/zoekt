package main

import (
	"os"
	"path/filepath"
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
