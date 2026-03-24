package zoekt

import (
	"encoding/json"
	"path"
	"path/filepath"
	"strings"
)

const (
	metadataFileSystemRoot     = "zoekt.file_system_root"
	metadataFileSystemRootJSON = "zoekt.file_system_roots"
)

// SetFileSystemRoot stores the absolute filesystem root for a repository that
// was indexed directly from local files.
func SetFileSystemRoot(repo *Repository, root string) error {
	return SetFileSystemRoots(repo, map[string]string{"": root})
}

// SetFileSystemRoots stores a mapping from indexed path prefixes to absolute
// filesystem roots for repositories built from multiple local directories.
func SetFileSystemRoots(repo *Repository, roots map[string]string) error {
	if repo == nil {
		return nil
	}

	if repo.Metadata == nil {
		repo.Metadata = map[string]string{}
	}

	delete(repo.Metadata, metadataFileSystemRoot)
	delete(repo.Metadata, metadataFileSystemRootJSON)

	if len(roots) == 0 {
		return nil
	}

	normalized := make(map[string]string, len(roots))
	for prefix, root := range roots {
		root = filepath.Clean(root)
		if !filepath.IsAbs(root) {
			continue
		}

		prefix = path.Clean(filepath.ToSlash(prefix))
		if prefix == "." {
			prefix = ""
		}
		prefix = strings.Trim(prefix, "/")
		normalized[prefix] = root
	}

	if len(normalized) == 0 {
		return nil
	}

	if root, ok := normalized[""]; ok && len(normalized) == 1 {
		repo.Metadata[metadataFileSystemRoot] = root
		return nil
	}

	payload, err := json.Marshal(normalized)
	if err != nil {
		return err
	}
	repo.Metadata[metadataFileSystemRootJSON] = string(payload)
	return nil
}

// ResolveFileSystemPath returns the absolute filesystem path for fileName when
// the repository was indexed from local files and the necessary metadata is
// available. It returns an empty string when the path cannot be resolved.
func ResolveFileSystemPath(repo *Repository, fileName string) string {
	if repo == nil {
		return ""
	}

	fileName = normalizeIndexedPath(fileName)
	if fileName == "" {
		return ""
	}

	if root := normalizeFileSystemRoot(repo.Metadata[metadataFileSystemRoot]); root != "" {
		return joinFileSystemPath(root, fileName)
	}

	if payload := repo.Metadata[metadataFileSystemRootJSON]; payload != "" {
		var roots map[string]string
		if err := json.Unmarshal([]byte(payload), &roots); err == nil {
			return resolveFromFileSystemRoots(fileName, roots)
		}
	}

	// Best-effort fallback for single-directory local indexes that only stored
	// their source path.
	if repo.URL == "" && repo.FileURLTemplate == "" {
		if root := normalizeFileSystemRoot(repo.Source); root != "" {
			return joinFileSystemPath(root, fileName)
		}
	}

	return ""
}

func resolveFromFileSystemRoots(fileName string, roots map[string]string) string {
	bestPrefix := ""
	bestRoot := ""

	for prefix, root := range roots {
		root = normalizeFileSystemRoot(root)
		if root == "" {
			continue
		}

		prefix = normalizeIndexedPath(prefix)
		if prefix == "" {
			if bestRoot == "" {
				bestRoot = root
			}
			continue
		}

		if fileName != prefix && !strings.HasPrefix(fileName, prefix+"/") {
			continue
		}

		if len(prefix) > len(bestPrefix) {
			bestPrefix = prefix
			bestRoot = root
		}
	}

	if bestRoot == "" {
		return ""
	}

	rel := fileName
	if bestPrefix != "" {
		rel = strings.TrimPrefix(strings.TrimPrefix(fileName, bestPrefix), "/")
	}

	return joinFileSystemPath(bestRoot, rel)
}

func joinFileSystemPath(root, rel string) string {
	root = normalizeFileSystemRoot(root)
	if root == "" {
		return ""
	}
	rel = normalizeIndexedPath(rel)
	if rel == "" {
		return root
	}
	return filepath.Clean(filepath.Join(root, filepath.FromSlash(rel)))
}

func normalizeIndexedPath(name string) string {
	name = path.Clean(filepath.ToSlash(name))
	if name == "." {
		return ""
	}
	return strings.TrimPrefix(name, "/")
}

func normalizeFileSystemRoot(root string) string {
	root = filepath.Clean(root)
	if !filepath.IsAbs(root) {
		return ""
	}
	return root
}
