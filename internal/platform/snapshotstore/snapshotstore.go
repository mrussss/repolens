package snapshotstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type SnapshotStore interface {
	GetSourcePath(repoID, snapshotID string) string
	EnsureDir(repoID, snapshotID string) (string, error)
	ReadFile(ctx context.Context, repoID, snapshotID, relativePath string, startLine, endLine int) (string, error)
	FileExists(repoID, snapshotID, relativePath string) bool
	WalkFiles(repoID, snapshotID string, fn func(relPath string, info os.FileInfo) error) error
}

type LocalSnapshotStore struct {
	basePath string
}

func NewLocalSnapshotStore(basePath string) *LocalSnapshotStore {
	return &LocalSnapshotStore{basePath: basePath}
}

func (s *LocalSnapshotStore) GetSourcePath(repoID, snapshotID string) string {
	return filepath.Join(s.basePath, repoID, snapshotID, "source")
}

func (s *LocalSnapshotStore) EnsureDir(repoID, snapshotID string) (string, error) {
	p := s.GetSourcePath(repoID, snapshotID)
	if err := os.MkdirAll(p, 0755); err != nil {
		return "", fmt.Errorf("failed to create snapshot dir: %w", err)
	}
	return p, nil
}

func (s *LocalSnapshotStore) ReadFile(ctx context.Context, repoID, snapshotID, relativePath string, startLine, endLine int) (string, error) {
	fullPath, err := s.safePath(repoID, snapshotID, relativePath)
	if err != nil {
		return "", err
	}

	data, err := os.ReadFile(fullPath)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %w", err)
	}

	lines := strings.Split(string(data), "\n")
	totalLines := len(lines)

	if startLine <= 0 {
		startLine = 1
	}
	if endLine <= 0 || endLine > totalLines {
		endLine = totalLines
	}
	if startLine > endLine {
		return "", fmt.Errorf("invalid line range: %d to %d (total lines %d)", startLine, endLine, totalLines)
	}

	selected := lines[startLine-1 : endLine]
	return strings.Join(selected, "\n"), nil
}

func (s *LocalSnapshotStore) FileExists(repoID, snapshotID, relativePath string) bool {
	fullPath, err := s.safePath(repoID, snapshotID, relativePath)
	if err != nil {
		return false
	}
	info, err := os.Stat(fullPath)
	if err != nil {
		return false
	}
	return !info.IsDir() && info.Mode()&os.ModeSymlink == 0
}

func (s *LocalSnapshotStore) WalkFiles(repoID, snapshotID string, fn func(relPath string, info os.FileInfo) error) error {
	sourceRoot := s.GetSourcePath(repoID, snapshotID)
	if _, err := os.Stat(sourceRoot); os.IsNotExist(err) {
		return fmt.Errorf("snapshot directory does not exist: %s", sourceRoot)
	}

	return filepath.Walk(sourceRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == sourceRoot {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(sourceRoot, path)
		if err != nil {
			return err
		}
		return fn(rel, info)
	})
}

func (s *LocalSnapshotStore) safePath(repoID, snapshotID, relativePath string) (string, error) {
	sourceRoot := s.GetSourcePath(repoID, snapshotID)
	cleaned := filepath.Clean(relativePath)
	if cleaned == "." || filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) || cleaned == ".." {
		return "", fmt.Errorf("path traversal denied: %s", relativePath)
	}
	fullPath := filepath.Join(sourceRoot, cleaned)
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
	return pathReal, nil
}
