package gitindex

import "testing"

func TestAgentGitLogicalPaths(t *testing.T) {
	for _, tt := range []struct {
		sub, path, want string
	}{
		{"", "Source/Runtime/file.cpp", "Source/Runtime/file.cpp"},
		{"vendor/nested", "Source/file.cpp", "vendor/nested/Source/file.cpp"},
	} {
		k := fileKey{SubRepoPath: tt.sub, Path: tt.path}
		if got := k.FullPath(); got != tt.want {
			t.Errorf("FullPath(%q, %q)=%q want=%q", tt.sub, tt.path, got, tt.want)
		}
	}
}
