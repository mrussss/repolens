package artifact

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"repolens/internal/retrieval/bm25"
)

// Manifest contains the metadata of a published retrieval build artifact.
type Manifest struct {
	RetrievalBuildID int64     `json:"retrieval_build_id"`
	Strategy         string    `json:"strategy"`
	DocumentCount    int       `json:"document_count"`
	ArtifactHash     string    `json:"artifact_hash"`
	CreatedAt        time.Time `json:"created_at"`
}

// Publisher handles atomic directory construction, verification, and promotion.
type Publisher struct {
	baseDir       string
	openIndexFile func(string, int, os.FileMode) (indexFile, error)
}

type indexFile interface {
	io.Writer
	Sync() error
	Close() error
}

// NewPublisher creates an artifact publisher with the given base index storage path.
func NewPublisher(baseDir string) *Publisher {
	return &Publisher{baseDir: baseDir}
}

// Publish atomically writes index files to a staging directory and promotes
// them to a path owned by this execution. Published paths are immutable: a
// later claim for the same build receives a distinct directory.
func (p *Publisher) Publish(buildID int64, executionGeneration int64, claimToken string, strategy string, idx *bm25.Index) (finalPath string, artifactHash string, err error) {
	if claimToken == "" {
		claimToken = "default-token"
	}
	if executionGeneration < 1 {
		return "", "", fmt.Errorf("execution generation must be positive")
	}

	claimDigest := sha256.Sum256([]byte(claimToken))
	claimKey := hex.EncodeToString(claimDigest[:])
	tmpRoot := filepath.Join(p.baseDir, ".tmp")
	if err := os.MkdirAll(tmpRoot, 0755); err != nil {
		return "", "", fmt.Errorf("failed creating artifact staging root: %w", err)
	}
	tmpDir, err := os.MkdirTemp(tmpRoot, fmt.Sprintf("%d-%d-", buildID, executionGeneration))
	if err != nil {
		return "", "", fmt.Errorf("failed creating staging artifact dir: %w", err)
	}
	finalDir := filepath.Join(p.baseDir, fmt.Sprintf("%d", buildID), fmt.Sprintf("gen-%d", executionGeneration), claimKey)

	defer func() {
		if err != nil {
			if cleanupErr := os.RemoveAll(tmpDir); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("failed cleaning temporary artifact directory: %w", cleanupErr))
			}
		}
	}()

	// 1. Write BM25 index
	indexPath := filepath.Join(tmpDir, "index.json")
	indexFile, err := p.createIndexFile(indexPath)
	if err != nil {
		return "", "", fmt.Errorf("failed creating index file: %w", err)
	}
	if err := idx.Save(indexFile); err != nil {
		if closeErr := indexFile.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("failed closing index file after save error: %w", closeErr))
		}
		return "", "", fmt.Errorf("failed saving index: %w", err)
	}
	if err := indexFile.Sync(); err != nil {
		closeErr := indexFile.Close()
		if closeErr != nil {
			err = errors.Join(err, fmt.Errorf("failed closing index file after sync error: %w", closeErr))
		}
		return "", "", fmt.Errorf("failed syncing index file: %w", err)
	}
	if err := indexFile.Close(); err != nil {
		return "", "", fmt.Errorf("failed closing index file: %w", err)
	}

	// 2. Compute SHA256 of index file
	h := sha256.New()
	readIndex, err := os.Open(indexPath)
	if err != nil {
		return "", "", err
	}
	if _, err := io.Copy(h, readIndex); err != nil {
		_ = readIndex.Close()
		return "", "", err
	}
	_ = readIndex.Close()
	hashStr := hex.EncodeToString(h.Sum(nil))

	// 3. Write manifest.json
	manifest := Manifest{
		RetrievalBuildID: buildID,
		Strategy:         strategy,
		DocumentCount:    idx.TotalDocs,
		ArtifactHash:     hashStr,
		CreatedAt:        time.Now().UTC(),
	}
	manifestPath := filepath.Join(tmpDir, "manifest.json")
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", "", fmt.Errorf("failed marshaling manifest: %w", err)
	}
	if err := os.WriteFile(manifestPath, manifestBytes, 0644); err != nil {
		return "", "", fmt.Errorf("failed writing manifest: %w", err)
	}

	// 4. Atomic rename to this execution's unique immutable directory. Never
	// remove a published path: a READY database row may point at it.
	if err := os.MkdirAll(filepath.Dir(finalDir), 0755); err != nil {
		return "", "", fmt.Errorf("failed creating artifact generation dir: %w", err)
	}
	if err := os.Rename(tmpDir, finalDir); err != nil {
		return "", "", fmt.Errorf("failed atomically renaming artifact dir to %s: %w", finalDir, err)
	}

	return finalDir, hashStr, nil
}

func (p *Publisher) createIndexFile(path string) (indexFile, error) {
	if p.openIndexFile != nil {
		return p.openIndexFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
}

// LoadIndex loads a published BM25 index from its final artifact directory.
func LoadIndex(artifactDir string) (*bm25.Index, error) {
	return LoadIndexVerified(artifactDir, 0, "")
}

// LoadIndexVerified validates both the manifest identity and the checksum of
// the index bytes before exposing an artifact to the retriever.
func LoadIndexVerified(artifactDir string, expectedBuildID int64, expectedHash string) (*bm25.Index, error) {
	indexPath := filepath.Join(artifactDir, "index.json")
	manifestPath := filepath.Join(artifactDir, "manifest.json")
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("failed opening artifact manifest at %s: %w", manifestPath, err)
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, fmt.Errorf("invalid artifact manifest: %w", err)
	}
	if expectedBuildID > 0 && manifest.RetrievalBuildID != expectedBuildID {
		return nil, fmt.Errorf("artifact build id mismatch: manifest=%d expected=%d", manifest.RetrievalBuildID, expectedBuildID)
	}
	f, err := os.Open(indexPath)
	if err != nil {
		return nil, fmt.Errorf("failed opening index artifact at %s: %w", indexPath, err)
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("failed reading index artifact: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	h := sha256.Sum256(data)
	actualHash := hex.EncodeToString(h[:])
	if manifest.ArtifactHash == "" || actualHash != manifest.ArtifactHash || (expectedHash != "" && actualHash != expectedHash) {
		return nil, fmt.Errorf("retrieval artifact hash mismatch")
	}
	return bm25.Load(bytes.NewReader(data))
}

// CleanupUnreferenced removes only old immutable execution directories that
// are absent from the database's artifact_path values. It is intended for a
// periodic maintenance process, never for a job publisher.
func (p *Publisher) CleanupUnreferenced(referenced map[string]struct{}, olderThan time.Time) (int, error) {
	root, err := filepath.Abs(p.baseDir)
	if err != nil {
		return 0, err
	}
	refs := make(map[string]struct{}, len(referenced))
	for path := range referenced {
		abs, err := filepath.Abs(path)
		if err != nil {
			return 0, err
		}
		refs[filepath.Clean(abs)] = struct{}{}
	}
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, build := range entries {
		if build.Name() == ".tmp" {
			tmpRoot := filepath.Join(root, build.Name())
			tmpEntries, readErr := os.ReadDir(tmpRoot)
			if readErr != nil {
				return removed, readErr
			}
			for _, tmp := range tmpEntries {
				if !tmp.IsDir() || tmp.Type()&os.ModeSymlink != 0 {
					continue
				}
				path := filepath.Join(tmpRoot, tmp.Name())
				info, statErr := tmp.Info()
				if statErr != nil {
					return removed, statErr
				}
				if info.ModTime().Before(olderThan) {
					if err := os.RemoveAll(path); err != nil {
						return removed, err
					}
					removed++
				}
			}
			continue
		}
		if _, err := strconv.ParseInt(build.Name(), 10, 64); err != nil || !build.IsDir() || build.Type()&os.ModeSymlink != 0 {
			continue
		}
		generationEntries, readErr := os.ReadDir(filepath.Join(root, build.Name()))
		if readErr != nil {
			return removed, readErr
		}
		for _, generation := range generationEntries {
			if !strings.HasPrefix(generation.Name(), "gen-") || !generation.IsDir() || generation.Type()&os.ModeSymlink != 0 {
				continue
			}
			if n, err := strconv.ParseInt(strings.TrimPrefix(generation.Name(), "gen-"), 10, 64); err != nil || n < 1 {
				continue
			}
			claimEntries, readErr := os.ReadDir(filepath.Join(root, build.Name(), generation.Name()))
			if readErr != nil {
				return removed, readErr
			}
			for _, claim := range claimEntries {
				if len(claim.Name()) != 64 || !isHexString(claim.Name()) || !claim.IsDir() || claim.Type()&os.ModeSymlink != 0 {
					continue
				}
				path := filepath.Join(root, build.Name(), generation.Name(), claim.Name())
				if _, ok := refs[filepath.Clean(path)]; ok {
					continue
				}
				info, statErr := claim.Info()
				if statErr != nil {
					return removed, statErr
				}
				if info.ModTime().Before(olderThan) {
					if err := os.RemoveAll(path); err != nil {
						return removed, err
					}
					removed++
				}
			}
		}
	}
	return removed, nil
}

func isHexString(value string) bool {
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}
