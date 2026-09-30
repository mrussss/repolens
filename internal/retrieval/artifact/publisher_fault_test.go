package artifact

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"repolens/internal/retrieval/bm25"
)

func TestPublisherPropagatesIndexSyncAndCloseFailuresWithoutPublishing(t *testing.T) {
	tests := []struct {
		name     string
		syncErr  error
		closeErr error
	}{
		{name: "sync failure", syncErr: errors.New("injected sync failure")},
		{name: "close failure", closeErr: errors.New("injected close failure")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			var opened *faultingIndexFile
			publisher := &Publisher{
				baseDir: root,
				openIndexFile: func(path string, flags int, perm os.FileMode) (indexFile, error) {
					file, err := os.OpenFile(path, flags, perm)
					if err != nil {
						return nil, err
					}
					opened = &faultingIndexFile{File: file, syncErr: tt.syncErr, closeErr: tt.closeErr}
					return opened, nil
				},
			}
			idx := bm25.NewIndex(1.2, 0.75)
			idx.AddDocument(bm25.Document{FilePath: "main.go", Content: "func Handle() {}"})
			idx.Build()

			path, hash, err := publisher.Publish(42, 1, "fault-injected-claim", "BM25", idx)
			wantErr := tt.syncErr
			if wantErr == nil {
				wantErr = tt.closeErr
			}
			if !errors.Is(err, wantErr) {
				t.Fatalf("Publish error = %v, want propagated %v", err, wantErr)
			}
			if path != "" || hash != "" {
				t.Fatalf("failed publication returned path/hash %q/%q", path, hash)
			}
			if opened == nil || opened.closeCalls != 1 {
				t.Fatalf("index file close calls = %v, want exactly 1", opened)
			}

			tmpEntries, err := os.ReadDir(filepath.Join(root, ".tmp"))
			if err != nil {
				t.Fatalf("read temporary artifact root: %v", err)
			}
			if len(tmpEntries) != 0 {
				t.Fatalf("temporary artifacts remain after failed publication: %v", tmpEntries)
			}
			claimDigest := sha256.Sum256([]byte("fault-injected-claim"))
			finalDir := filepath.Join(root, "42", "gen-1", hex.EncodeToString(claimDigest[:]))
			if _, err := os.Stat(finalDir); !os.IsNotExist(err) {
				t.Fatalf("failed publication exposed a final artifact at %s: %v", finalDir, err)
			}
		})
	}
}

type faultingIndexFile struct {
	*os.File
	syncErr    error
	closeErr   error
	closeCalls int
}

func (f *faultingIndexFile) Sync() error {
	if f.syncErr != nil {
		return f.syncErr
	}
	return f.File.Sync()
}

func (f *faultingIndexFile) Close() error {
	f.closeCalls++
	if err := f.File.Close(); err != nil {
		return err
	}
	return f.closeErr
}
