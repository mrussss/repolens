// Package snapshotpolicy defines the shared file visibility rules for
// materialized snapshots, CodeIndex, agent reads, and evidence.
package snapshotpolicy

import (
	"path/filepath"
	"strings"
)

const VisibilityAgentReadable = "AGENT_READABLE"

type Decision struct {
	Allowed    bool
	Visibility string
	Reason     string
}

var ignoredDirectories = map[string]struct{}{
	".git": {}, "node_modules": {}, "vendor": {}, "dist": {}, "build": {},
	"bin": {}, "obj": {}, ".idea": {}, ".vscode": {}, "target": {},
	"__pycache__": {}, ".next": {}, "testdata": {},
}

var ignoredExtensions = map[string]struct{}{
	".exe": {}, ".dll": {}, ".so": {}, ".dylib": {}, ".bin": {},
	".png": {}, ".jpg": {}, ".jpeg": {}, ".gif": {}, ".ico": {},
	".svg": {}, ".pdf": {}, ".zip": {}, ".tar": {}, ".gz": {}, ".7z": {},
	".jar": {}, ".war": {}, ".pyc": {}, ".class": {}, ".db": {},
	".sqlite": {}, ".woff": {}, ".woff2": {}, ".ttf": {}, ".eot": {},
	".mp4": {}, ".mp3": {},
}

var sensitiveNames = map[string]struct{}{
	".env": {}, "id_rsa": {}, "id_dsa": {}, "id_ed25519": {},
	"credentials.json": {}, "service-account.json": {},
}

func CanMaterialize(path string, size int64) Decision   { return evaluate(path, size) }
func CanIndex(path string, size int64) Decision         { return evaluate(path, size) }
func CanReadByAgent(path string, size int64) Decision   { return evaluate(path, size) }
func CanIssueEvidence(path string, size int64) Decision { return evaluate(path, size) }

func ShouldSkipDirectory(name string) bool {
	name = strings.ToLower(name)
	if _, ok := ignoredDirectories[name]; ok {
		return true
	}
	return strings.HasPrefix(name, ".")
}

func evaluate(path string, size int64) Decision {
	if size < 0 || strings.Contains(path, "\\") {
		return Decision{Reason: "path or size is invalid"}
	}
	clean := filepath.ToSlash(filepath.Clean(path))
	if clean == "." || clean == "" || filepath.IsAbs(path) || clean == ".." || strings.HasPrefix(clean, "../") {
		return Decision{Reason: "path is not a snapshot relative path"}
	}
	parts := strings.Split(clean, "/")
	for _, part := range parts[:len(parts)-1] {
		if ShouldSkipDirectory(part) {
			return Decision{Reason: "path is under an excluded directory"}
		}
	}
	base := strings.ToLower(parts[len(parts)-1])
	if _, ok := sensitiveNames[base]; ok || strings.HasPrefix(base, ".env.") || strings.HasSuffix(base, ".pem") || strings.HasSuffix(base, ".key") {
		return Decision{Reason: "path identifies a sensitive file"}
	}
	if _, ok := ignoredExtensions[strings.ToLower(filepath.Ext(base))]; ok {
		return Decision{Reason: "file type is excluded by snapshot policy"}
	}
	return Decision{Allowed: true, Visibility: VisibilityAgentReadable}
}

func DetectLanguage(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
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
