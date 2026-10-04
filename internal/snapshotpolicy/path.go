package snapshotpolicy

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SafePath rejects symlinks and non-regular files within an immutable source root.
func SafePath(sourceRoot, relativePath string) (string, error) {
	if !CanReadByAgent(relativePath, 0).Allowed {
		return "", fmt.Errorf("snapshot file access denied: %s", relativePath)
	}
	cleaned := filepath.Clean(relativePath)
	if cleaned == "." || filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) || cleaned == ".." {
		return "", fmt.Errorf("path traversal denied: %s", relativePath)
	}
	rootInfo, err := os.Lstat(sourceRoot)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("non-canonical snapshot root denied")
	}
	fullPath := filepath.Join(sourceRoot, cleaned)
	info, err := os.Lstat(fullPath)
	if err != nil {
		return "", fmt.Errorf("file not found: %s", relativePath)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("symlink access denied: %s", relativePath)
	}
	// Reject symlinked parent directories as well as a symlink at the final
	// path. EvalSymlinks below prevents escape, but an in-root symlink is still
	// not a canonical snapshot path and must not receive an Evidence ID.
	relativeFromRoot, err := filepath.Rel(sourceRoot, fullPath)
	if err != nil {
		return "", fmt.Errorf("path resolution denied: %s", relativePath)
	}
	current := sourceRoot
	for _, component := range strings.Split(relativeFromRoot, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		componentInfo, componentErr := os.Lstat(current)
		if componentErr != nil {
			return "", fmt.Errorf("file not found: %s", relativePath)
		}
		if componentInfo.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("symlink access denied: %s", relativePath)
		}
	}
	rootReal, err := filepath.EvalSymlinks(sourceRoot)
	if err != nil {
		return "", fmt.Errorf("snapshot root unavailable")
	}
	pathReal, err := filepath.EvalSymlinks(fullPath)
	if err != nil {
		return "", fmt.Errorf("file not found: %s", relativePath)
	}
	rel, err := filepath.Rel(rootReal, pathReal)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("symlink escape denied: %s", relativePath)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("non-regular snapshot file denied: %s", relativePath)
	}
	return pathReal, nil
}
