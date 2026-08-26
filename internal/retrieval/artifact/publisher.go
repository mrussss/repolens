package artifact

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	baseDir string
}

// NewPublisher creates an artifact publisher with the given base index storage path.
func NewPublisher(baseDir string) *Publisher {
	return &Publisher{baseDir: baseDir}
}

// Publish atomically writes index files to a staging directory, computes checksums, and renames to the final path.
func (p *Publisher) Publish(buildID int64, claimToken string, strategy string, idx *bm25.Index) (finalPath string, artifactHash string, err error) {
	if claimToken == "" {
		claimToken = "default-token"
	}

	tmpDir := filepath.Join(p.baseDir, ".tmp", fmt.Sprintf("%d-%s", buildID, claimToken))
	finalDir := filepath.Join(p.baseDir, fmt.Sprintf("%d", buildID))

	// Clean up any stale tmpDir
	_ = os.RemoveAll(tmpDir)
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return "", "", fmt.Errorf("failed creating staging artifact dir: %w", err)
	}

	defer func() {
		// Clean up tmpDir on error
		if err != nil {
			_ = os.RemoveAll(tmpDir)
		}
	}()

	// 1. Write BM25 index
	indexPath := filepath.Join(tmpDir, "index.json")
	indexFile, err := os.OpenFile(indexPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return "", "", fmt.Errorf("failed creating index file: %w", err)
	}
	if err := idx.Save(indexFile); err != nil {
		_ = indexFile.Close()
		return "", "", fmt.Errorf("failed saving index: %w", err)
	}
	_ = indexFile.Sync()
	_ = indexFile.Close()

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

	// 4. Atomic Rename to final directory
	_ = os.RemoveAll(finalDir)
	_ = os.MkdirAll(filepath.Dir(finalDir), 0755)
	if err := os.Rename(tmpDir, finalDir); err != nil {
		return "", "", fmt.Errorf("failed atomically renaming artifact dir to %s: %w", finalDir, err)
	}

	return finalDir, hashStr, nil
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
