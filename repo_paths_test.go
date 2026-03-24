package zoekt

import (
	"path/filepath"
	"testing"
)

func TestResolveFileSystemPathSingleRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Engine")
	repo := &Repository{}
	if err := SetFileSystemRoot(repo, root); err != nil {
		t.Fatal(err)
	}

	got := ResolveFileSystemPath(repo, "Source/main.go")
	want := filepath.Join(root, "Source", "main.go")
	if got != want {
		t.Fatalf("ResolveFileSystemPath() = %q, want %q", got, want)
	}
}

func TestResolveFileSystemPathMultipleRoots(t *testing.T) {
	tempDir := t.TempDir()
	repo := &Repository{}
	if err := SetFileSystemRoots(repo, map[string]string{
		"Engine":         filepath.Join(tempDir, "Engine"),
		"Samples/Engine": filepath.Join(tempDir, "Samples", "Engine"),
	}); err != nil {
		t.Fatal(err)
	}

	got := ResolveFileSystemPath(repo, "Samples/Engine/Source/main.go")
	want := filepath.Join(tempDir, "Samples", "Engine", "Source", "main.go")
	if got != want {
		t.Fatalf("ResolveFileSystemPath() = %q, want %q", got, want)
	}
}

func TestResolveFileSystemPathSourceFallback(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Workspace")
	repo := &Repository{Source: root}

	got := ResolveFileSystemPath(repo, "src/app.go")
	want := filepath.Join(root, "src", "app.go")
	if got != want {
		t.Fatalf("ResolveFileSystemPath() = %q, want %q", got, want)
	}
}
