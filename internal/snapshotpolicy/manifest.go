package snapshotpolicy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	ManifestFileName = ".repolens-snapshot-manifest.json"
	ManifestVersion  = 1
)

var ErrManifestNotFound = errors.New("snapshot file manifest not found")

type FileEntry struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	Language   string `json:"language"`
	Visibility string `json:"visibility"`
}

type Manifest struct {
	Version     int         `json:"version"`
	SnapshotID  string      `json:"snapshot_id"`
	CommitSHA   string      `json:"commit_sha"`
	ContentHash string      `json:"content_hash"`
	Files       []FileEntry `json:"files"`
}

func ManifestPath(sourceRoot string) string {
	return filepath.Join(filepath.Dir(sourceRoot), ManifestFileName)
}

func NewManifest(snapshotID, commitSHA, contentHash string, files []FileEntry) Manifest {
	entries := append([]FileEntry(nil), files...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return Manifest{Version: ManifestVersion, SnapshotID: snapshotID, CommitSHA: commitSHA, ContentHash: contentHash, Files: entries}
}

func (m Manifest) AllowedPaths() []string {
	paths := make([]string, 0, len(m.Files))
	for _, file := range m.Files {
		if CanIndex(file.Path, file.Size).Allowed && file.Visibility == VisibilityAgentReadable {
			paths = append(paths, filepath.ToSlash(file.Path))
		}
	}
	return paths
}

func (m Manifest) Contains(path string) bool {
	decision := CanReadByAgent(path, 0)
	if !decision.Allowed {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(path))
	i := sort.Search(len(m.Files), func(i int) bool { return m.Files[i].Path >= clean })
	return i < len(m.Files) && m.Files[i].Path == clean && m.Files[i].Visibility == VisibilityAgentReadable
}

func WriteManifest(sourceRoot string, manifest Manifest) error {
	if manifest.Version != ManifestVersion {
		return fmt.Errorf("unsupported snapshot manifest version %d", manifest.Version)
	}
	if manifest.SnapshotID == "" || manifest.CommitSHA == "" || !validHash(manifest.ContentHash) {
		return fmt.Errorf("snapshot manifest identity is incomplete")
	}
	manifest = NewManifest(manifest.SnapshotID, manifest.CommitSHA, manifest.ContentHash, manifest.Files)
	for index, file := range manifest.Files {
		if (index > 0 && file.Path == manifest.Files[index-1].Path) || !canonicalEntry(file) {
			return fmt.Errorf("invalid or excluded snapshot manifest entry %q", file.Path)
		}
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	path := ManifestPath(sourceRoot)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func LoadManifest(sourceRoot string) (Manifest, error) {
	data, err := os.ReadFile(ManifestPath(sourceRoot))
	if errors.Is(err, os.ErrNotExist) {
		return Manifest{}, ErrManifestNotFound
	}
	if err != nil {
		return Manifest{}, err
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode snapshot file manifest: %w", err)
	}
	if manifest.Version != ManifestVersion {
		return Manifest{}, fmt.Errorf("unsupported snapshot manifest version %d", manifest.Version)
	}
	if manifest.SnapshotID == "" || manifest.CommitSHA == "" || !validHash(manifest.ContentHash) {
		return Manifest{}, fmt.Errorf("snapshot manifest identity is incomplete")
	}
	sorted := NewManifest(manifest.SnapshotID, manifest.CommitSHA, manifest.ContentHash, manifest.Files)
	if len(sorted.Files) != len(manifest.Files) {
		return Manifest{}, fmt.Errorf("invalid snapshot file manifest")
	}
	for index, file := range manifest.Files {
		if file.Path != sorted.Files[index].Path || (index > 0 && file.Path == manifest.Files[index-1].Path) || !canonicalEntry(file) {
			return Manifest{}, fmt.Errorf("invalid snapshot manifest entry %q", file.Path)
		}
	}
	return manifest, nil
}

func canonicalEntry(file FileEntry) bool {
	return file.Path != "" && filepath.ToSlash(filepath.Clean(file.Path)) == file.Path &&
		file.Size >= 0 && CanMaterialize(file.Path, file.Size).Allowed &&
		file.Visibility == VisibilityAgentReadable && validHash(file.SHA256) && file.Language != ""
}

func validHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func CanReadFromSnapshot(sourceRoot, path string) Decision {
	decision := CanReadByAgent(path, 0)
	if !decision.Allowed {
		return decision
	}
	manifest, err := LoadManifest(sourceRoot)
	if errors.Is(err, ErrManifestNotFound) {
		if RequiresManifest(sourceRoot) {
			return Decision{Reason: "immutable snapshot manifest is missing"}
		}
		return decision // Legacy snapshots have no manifest; use the shared policy.
	}
	if err != nil {
		return Decision{Reason: "snapshot manifest is unavailable or invalid"}
	}
	if !manifest.Contains(path) {
		return Decision{Reason: "file is not present in the snapshot manifest"}
	}
	return decision
}

func RequiresManifest(sourceRoot string) bool {
	parts := strings.Split(filepath.Clean(sourceRoot), string(filepath.Separator))
	for index, part := range parts {
		if part == "executions" && index+1 < len(parts) && strings.HasPrefix(parts[index+1], "gen-") {
			return true
		}
	}
	return false
}

func CanIssueEvidenceFromSnapshot(sourceRoot, path string) Decision {
	decision := CanIssueEvidence(path, 0)
	if !decision.Allowed {
		return decision
	}
	manifestDecision := CanReadFromSnapshot(sourceRoot, path)
	if !manifestDecision.Allowed {
		return manifestDecision
	}
	return decision
}

func FileEntryFor(path string, content []byte) FileEntry {
	hash := sha256.Sum256(content)
	return FileEntry{
		Path: filepath.ToSlash(filepath.Clean(path)), Size: int64(len(content)),
		SHA256: hex.EncodeToString(hash[:]), Language: DetectLanguage(path), Visibility: VisibilityAgentReadable,
	}
}
