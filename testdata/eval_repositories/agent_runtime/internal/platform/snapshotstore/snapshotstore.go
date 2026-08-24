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
	sourceRoot := s.GetSourcePath(repoID, snapshotID)
	cleanedRel := filepath.Clean(relativePath)
	if strings.HasPrefix(cleanedRel, "..") || filepath.IsAbs(cleanedRel) {
		return "", fmt.Errorf("path traversal denied: %s", relativePath)
	}

	fullPath := filepath.Join(sourceRoot, cleanedRel)
	// Verify it does not escape sourceRoot via symlink
	realPath, err := filepath.EvalSymlinks(fullPath)
	if err != nil {
		return "", fmt.Errorf("file not found: %s", relativePath)
	}
	realRoot, err := filepath.EvalSymlinks(sourceRoot)
	if err != nil {
		realRoot = sourceRoot
	}
	if !strings.HasPrefix(realPath, realRoot) {
		return "", fmt.Errorf("symlink escape denied: %s", relativePath)
	}

	data, err := os.ReadFile(realPath)
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
	sourceRoot := s.GetSourcePath(repoID, snapshotID)
	fullPath := filepath.Join(sourceRoot, filepath.Clean(relativePath))
	info, err := os.Stat(fullPath)
	if err != nil {
		return false
	}
	return !info.IsDir()
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
		rel, err := filepath.Rel(sourceRoot, path)
		if err != nil {
			return err
		}
		return fn(rel, info)
	})
}
