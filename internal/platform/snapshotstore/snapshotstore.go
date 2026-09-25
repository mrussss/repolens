package snapshotstore

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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

type LineRange struct {
	StartLine int
	EndLine   int
}

type BoundedLineRangeResult struct {
	Content string
	Err     error
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
	return s.readFileRange(ctx, repoID, snapshotID, relativePath, startLine, endLine, maxBytes, false)
}

// ReadFileRangeBounded reads only through endLine (or until maxBytes is
// reached). TotalLines is zero when the scan stops before EOF because the
// complete line count is then intentionally unknown.
func (s *LocalSnapshotStore) ReadFileRangeBounded(ctx context.Context, repoID, snapshotID, relativePath string, startLine, endLine, maxBytes int) (FileRange, error) {
	return s.readFileRange(ctx, repoID, snapshotID, relativePath, startLine, endLine, maxBytes, true)
}

// ReadFileRangesBounded extracts several line ranges from one file in a
// single forward scan. Each range retains at most maxBytes of complete lines.
func (s *LocalSnapshotStore) ReadFileRangesBounded(ctx context.Context, repoID, snapshotID, relativePath string, ranges []LineRange, maxBytes int) ([]BoundedLineRangeResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if maxBytes <= 0 {
		return nil, fmt.Errorf("maxBytes must be positive")
	}
	for _, lineRange := range ranges {
		if lineRange.StartLine < 1 || lineRange.EndLine < lineRange.StartLine {
			return nil, fmt.Errorf("invalid source range %d-%d", lineRange.StartLine, lineRange.EndLine)
		}
	}
	fullPath, err := s.safePath(repoID, snapshotID, relativePath)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(fullPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}
	defer file.Close()
	return readBoundedRanges(ctx, bufio.NewReader(file), ranges, maxBytes)
}

type boundedRangeState struct {
	content strings.Builder
	bytes   int
	hasLine bool
	started bool
	done    bool
	err     error
}

func readBoundedRanges(ctx context.Context, reader *bufio.Reader, ranges []LineRange, maxBytes int) ([]BoundedLineRangeResult, error) {
	results := make([]BoundedLineRangeResult, len(ranges))
	if len(ranges) == 0 {
		return results, nil
	}
	states := make([]boundedRangeState, len(ranges))
	order := make([]int, len(ranges))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool { return ranges[order[i]].StartLine < ranges[order[j]].StartLine })

	active := make([]int, 0, len(ranges))
	next, completed, lineNo := 0, 0, 1
	var line bytes.Buffer
	lineTooLong := false
	finish := func(index int, err error) {
		state := &states[index]
		if state.done {
			return
		}
		state.done = true
		state.err = err
		completed++
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for next < len(order) && ranges[order[next]].StartLine <= lineNo {
			index := order[next]
			states[index].started = true
			active = append(active, index)
			next++
		}
		kept := active[:0]
		for _, index := range active {
			if !states[index].done && ranges[index].EndLine >= lineNo {
				kept = append(kept, index)
			}
		}
		active = kept

		fragment, readErr := reader.ReadSlice('\n')
		lineComplete := readErr == nil || readErr == io.EOF
		part := fragment
		if readErr == nil && len(part) > 0 {
			part = part[:len(part)-1]
		}
		if len(active) > 0 && !lineTooLong {
			if line.Len()+len(part) > maxBytes {
				lineTooLong = true
				line.Reset()
			} else {
				_, _ = line.Write(part)
			}
		}
		if lineComplete {
			for _, index := range active {
				state := &states[index]
				if lineTooLong {
					if state.bytes == 0 {
						finish(index, ErrLineTooLong)
					} else {
						finish(index, nil)
					}
					continue
				}
				separatorBytes := 0
				if state.hasLine {
					separatorBytes = 1
				}
				if state.bytes+separatorBytes+line.Len() > maxBytes {
					if state.bytes == 0 {
						finish(index, ErrLineTooLong)
					} else {
						finish(index, nil)
					}
					continue
				}
				if state.hasLine {
					state.content.WriteByte('\n')
				}
				state.content.Write(line.Bytes())
				state.bytes += separatorBytes + line.Len()
				state.hasLine = true
				if ranges[index].EndLine <= lineNo {
					finish(index, nil)
				}
			}
			line.Reset()
			lineTooLong = false
			if readErr == io.EOF {
				break
			}
			lineNo++
			if completed == len(ranges) {
				break
			}
		} else if readErr != bufio.ErrBufferFull {
			return nil, readErr
		}
	}

	for i := range states {
		if !states[i].started {
			states[i].err = fmt.Errorf("invalid line range: %d to %d (total lines %d)", ranges[i].StartLine, ranges[i].EndLine, lineNo)
		}
		results[i] = BoundedLineRangeResult{Content: states[i].content.String(), Err: states[i].err}
	}
	return results, nil
}

func (s *LocalSnapshotStore) readFileRange(ctx context.Context, repoID, snapshotID, relativePath string, startLine, endLine, maxBytes int, stopAtEnd bool) (FileRange, error) {
	if err := ctx.Err(); err != nil {
		return FileRange{}, err
	}
	fullPath, err := s.safePath(repoID, snapshotID, relativePath)
	if err != nil {
		return FileRange{}, err
	}

	file, err := os.Open(fullPath)
	if err != nil {
		return FileRange{}, fmt.Errorf("failed to read file: %w", err)
	}
	defer file.Close()

	if startLine <= 0 {
		startLine = 1
	}
	if endLine > 0 && startLine > endLine {
		return FileRange{}, fmt.Errorf("invalid line range: %d to %d", startLine, endLine)
	}

	content, actualEndLine, totalLines, truncated, totalKnown, err := readBoundedLines(ctx, bufio.NewReader(file), startLine, endLine, maxBytes, stopAtEnd)
	if err != nil {
		return FileRange{}, err
	}
	if totalKnown && startLine > totalLines {
		return FileRange{}, fmt.Errorf("invalid line range: %d to %d (total lines %d)", startLine, endLine, totalLines)
	}
	result := FileRange{
		Path:       filepath.ToSlash(filepath.Clean(relativePath)),
		StartLine:  startLine,
		EndLine:    actualEndLine,
		TotalLines: totalLines,
		Content:    content,
		Truncated:  truncated,
	}
	if !totalKnown {
		result.TotalLines = 0
	}
	if err := ctx.Err(); err != nil {
		return FileRange{}, err
	}
	return result, nil
}

// readBoundedLines counts the whole file for FileRange.TotalLines but only
// retains selected lines up to maxBytes. A non-positive limit preserves the
// historical unbounded API for callers that explicitly require exact content.
func readBoundedLines(ctx context.Context, reader *bufio.Reader, startLine, requestedEnd, maxBytes int, stopAtEnd bool) (string, int, int, bool, bool, error) {
	var content strings.Builder
	lineNo, totalLines := 1, 1
	selectedLines := 0
	var currentLine []byte
	currentLineTooLong := false
	truncated := false
	actualEndLine := startLine - 1

	for {
		if err := ctx.Err(); err != nil {
			return "", 0, 0, false, false, err
		}
		fragment, readErr := reader.ReadSlice('\n')
		lineComplete := readErr == nil || readErr == io.EOF
		part := fragment
		if readErr == nil && len(part) > 0 {
			part = part[:len(part)-1]
		}
		selected := lineNo >= startLine && (requestedEnd <= 0 || lineNo <= requestedEnd) && !truncated
		if selected && !currentLineTooLong {
			if maxBytes > 0 && len(currentLine)+len(part) > maxBytes {
				currentLineTooLong = true
				currentLine = nil
			} else {
				currentLine = append(currentLine, part...)
			}
		}

		if lineComplete {
			if selected {
				separatorBytes := 0
				if selectedLines > 0 {
					separatorBytes = 1
				}
				if currentLineTooLong || (maxBytes > 0 && content.Len()+separatorBytes+len(currentLine) > maxBytes) {
					if selectedLines == 0 {
						return "", 0, 0, false, false, ErrLineTooLong
					}
					truncated = true
				} else {
					if selectedLines > 0 {
						content.WriteByte('\n')
					}
					content.Write(currentLine)
					selectedLines++
					actualEndLine = lineNo
				}
			}
			currentLine = nil
			currentLineTooLong = false
			if readErr == io.EOF {
				break
			}
			lineNo++
			totalLines++
			if stopAtEnd && ((requestedEnd > 0 && lineNo > requestedEnd) || truncated) {
				return content.String(), actualEndLine, totalLines, truncated, false, nil
			}
		} else if readErr != bufio.ErrBufferFull {
			return "", 0, 0, false, false, readErr
		}
	}

	return content.String(), actualEndLine, totalLines, truncated, true, nil
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
