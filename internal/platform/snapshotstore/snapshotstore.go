package snapshotstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var ErrLineTooLong = errors.New("snapshot file line exceeds the configured range limit")

type FileRange struct {
	Path       string
	StartLine  int
	EndLine    int
	TotalLines int
	Content    string
	Truncated  bool
}

type SnapshotStore interface {
	GetSourcePath(repoID, snapshotID string) string
	EnsureDir(repoID, snapshotID string) (string, error)
	ReadFile(ctx context.Context, repoID, snapshotID, relativePath string, startLine, endLine int) (string, error)
	ReadFileRange(ctx context.Context, repoID, snapshotID, relativePath string, startLine, endLine, maxBytes int) (FileRange, error)
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
	result, err := s.ReadFileRange(ctx, repoID, snapshotID, relativePath, startLine, endLine, 0)
	if err != nil {
		return "", err
	}
	return result.Content, nil
}

func (s *LocalSnapshotStore) ReadFileRange(ctx context.Context, repoID, snapshotID, relativePath string, startLine, endLine, maxBytes int) (FileRange, error) {
	if err := ctx.Err(); err != nil {
		return FileRange{}, err
	}
	fullPath, err := s.safePath(repoID, snapshotID, relativePath)
	if err != nil {
		return FileRange{}, err
	}

	data, err := os.ReadFile(fullPath)
	if err != nil {
		return FileRange{}, fmt.Errorf("failed to read file: %w", err)
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
		return FileRange{}, fmt.Errorf("invalid line range: %d to %d (total lines %d)", startLine, endLine, totalLines)
	}

	selected := lines[startLine-1 : endLine]
	content := strings.Join(selected, "\n")
	result := FileRange{
		Path:       filepath.ToSlash(filepath.Clean(relativePath)),
		StartLine:  startLine,
		EndLine:    endLine,
		TotalLines: totalLines,
		Content:    content,
	}
	if maxBytes > 0 && len(content) > maxBytes {
		var kept []string
		used := 0
		for _, line := range selected {
			extra := len(line)
			if len(kept) > 0 {
				extra++
			}
			if used+extra > maxBytes {
				if len(kept) == 0 {
					return FileRange{}, ErrLineTooLong
				}
				break
			}
			kept = append(kept, line)
			used += extra
		}
		result.Content = strings.Join(kept, "\n")
		result.EndLine = startLine + len(kept) - 1
		result.Truncated = result.EndLine < endLine
	}
	if err := ctx.Err(); err != nil {
		return FileRange{}, err
	}
	return result, nil
}

func (s *LocalSnapshotStore) FileExists(repoID, snapshotID, relativePath string) bool {
	fullPath, err := s.safePath(repoID, snapshotID, relativePath)
	if err != nil {
		return false
	}
	info, err := os.Lstat(fullPath)
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
	return pathReal, nil
}
