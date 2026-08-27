package indexing

import (
	"path/filepath"
	"strings"
)

var (
	ignoredDirs = map[string]bool{
		".git":         true,
		"node_modules": true,
		"vendor":       true,
		"dist":         true,
		"build":        true,
		"bin":          true,
		"obj":          true,
		".idea":        true,
		".vscode":      true,
		"target":       true,
		"__pycache__":  true,
		".next":        true,
	}

	ignoredExtensions = map[string]bool{
		".exe":    true,
		".dll":    true,
		".so":     true,
		".dylib":  true,
		".bin":    true,
		".png":    true,
		".jpg":    true,
		".jpeg":   true,
		".gif":    true,
		".ico":    true,
		".svg":    true,
		".pdf":    true,
		".zip":    true,
		".tar":    true,
		".gz":     true,
		".7z":     true,
		".jar":    true,
		".war":    true,
		".pyc":    true,
		".class":  true,
		".db":     true,
		".sqlite": true,
		".woff":   true,
		".woff2":  true,
		".ttf":    true,
		".eot":    true,
		".mp4":    true,
		".mp3":    true,
	}

	secretFileNames = map[string]bool{
		".env":                 true,
		".env.local":           true,
		".env.production":      true,
		"id_rsa":               true,
		"id_dsa":               true,
		"id_ed25519":           true,
		"credentials.json":     true,
		"service-account.json": true,
	}
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
	return ignoredDirs[strings.ToLower(dirName)]
}

func (f *FileFilter) ShouldIgnoreFile(relPath string, sizeBytes int64) bool {
	base := filepath.Base(relPath)
	lowerBase := strings.ToLower(base)
	ext := strings.ToLower(filepath.Ext(relPath))

	if secretFileNames[lowerBase] || strings.HasPrefix(lowerBase, ".env.") {
		return true
	}
	if strings.HasSuffix(lowerBase, ".pem") || strings.HasSuffix(lowerBase, ".key") {
		return true
	}
	if ignoredExtensions[ext] {
		return true
	}
	if sizeBytes > f.maxFileSizeKB*1024 {
		return true
	}

	// Check path segments for ignored directories
	parts := strings.Split(filepath.ToSlash(relPath), "/")
	for _, part := range parts[:len(parts)-1] {
		if ignoredDirs[strings.ToLower(part)] {
			return true
		}
	}

	return false
}

// IsOversized reports a file that violates the configured hard limit. It is
// separate from ShouldIgnoreFile so production indexing can fail permanently
// instead of silently hiding an oversized source file.
func (f *FileFilter) IsOversized(sizeBytes int64) bool {
	return sizeBytes > f.maxFileSizeKB*1024
}

func DetectLanguage(relPath string) string {
	ext := strings.ToLower(filepath.Ext(relPath))
	switch ext {
	case ".go":
		return "go"
	case ".py":
		return "python"
	case ".js", ".mjs", ".cjs":
		return "javascript"
	case ".ts", ".tsx":
		return "typescript"
	case ".java":
		return "java"
	case ".rs":
		return "rust"
	case ".c", ".h":
		return "c"
	case ".cpp", ".hpp", ".cc":
		return "cpp"
	case ".md", ".markdown":
		return "markdown"
	case ".json":
		return "json"
	case ".yaml", ".yml":
		return "yaml"
	case ".sql":
		return "sql"
	case ".sh", ".bash":
		return "bash"
	default:
		return "text"
	}
}
