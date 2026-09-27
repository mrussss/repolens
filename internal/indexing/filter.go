package indexing

import (
	"repolens/internal/snapshotpolicy"
)

type FileFilter struct {
	maxFileSizeKB int64
}

func NewFileFilter(maxFileSizeKB int64) *FileFilter {
	if maxFileSizeKB <= 0 {
		maxFileSizeKB = 512
	}
	return &FileFilter{maxFileSizeKB: maxFileSizeKB}
}

func (f *FileFilter) ShouldIgnoreDir(dirName string) bool {
	return snapshotpolicy.ShouldSkipDirectory(dirName)
}

func (f *FileFilter) ShouldIgnoreFile(relPath string, sizeBytes int64) bool {
	return f.IsExplicitlyIgnoredFile(relPath) || f.IsOversized(sizeBytes)
}

// IsExplicitlyIgnoredFile reports files excluded by path/name/type policy,
// independently of their size. Snapshot materialization uses this before the
// hard source-file size limit so large images, secrets, and files under ignored
// directories cannot fail a build for content that will never be indexed.
func (f *FileFilter) IsExplicitlyIgnoredFile(relPath string) bool {
	return !snapshotpolicy.CanMaterialize(relPath, 0).Allowed
}

// IsOversized reports a file that violates the configured hard limit. It is
// separate from ShouldIgnoreFile so production indexing can fail permanently
// instead of silently hiding an oversized source file.
func (f *FileFilter) IsOversized(sizeBytes int64) bool {
	return sizeBytes > f.maxFileSizeKB*1024
}

func DetectLanguage(relPath string) string {
	return snapshotpolicy.DetectLanguage(relPath)
}
