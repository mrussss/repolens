package indexing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"repolens/internal/mq"
	"repolens/internal/platform/logger"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/repoindex"
	"repolens/internal/snapshot"
)

type IndexPayload struct {
	RepositoryID string                      `json:"repository_id"`
	SnapshotID   string                      `json:"snapshot_id"`
	IndexID      string                      `json:"index_id"`
	GitURL       string                      `json:"git_url"`
	Ref          string                      `json:"ref"`
	Strategy     repoindex.RetrievalStrategy `json:"strategy"`
}

type ChunkIndexWriter interface {
	IndexChunks(ctx context.Context, snapshotID string, chunks []CodeChunk) error
}

type IndexWorker struct {
	broker        mq.Broker
	snapshotStore snapshot.Store
	indexStore    repoindex.Store
	storeFS       snapshotstore.SnapshotStore
	cloner        *SafeGitCloner
	filter        *FileFilter
	chunker       *CodeChunker
	indexWriter   ChunkIndexWriter
	prefetch      int
	wg            sync.WaitGroup
}

func NewIndexWorker(
	broker mq.Broker,
	snapshotStore snapshot.Store,
	indexStore repoindex.Store,
	storeFS snapshotstore.SnapshotStore,
	cloner *SafeGitCloner,
	filter *FileFilter,
	chunker *CodeChunker,
	indexWriter ChunkIndexWriter,
	prefetch int,
) *IndexWorker {
	if prefetch <= 0 {
		prefetch = 2
	}
	return &IndexWorker{
		broker:        broker,
		snapshotStore: snapshotStore,
		indexStore:    indexStore,
		storeFS:       storeFS,
		cloner:        cloner,
		filter:        filter,
		chunker:       chunker,
		indexWriter:   indexWriter,
		prefetch:      prefetch,
	}
}

func (w *IndexWorker) Start(ctx context.Context) error {
	msgCh, err := w.broker.Consume(ctx, mq.QueueIndexTask, w.prefetch)
	if err != nil {
		return fmt.Errorf("failed to consume index queue: %w", err)
	}

	logger.L(ctx).Info("index worker started")

	for {
		select {
		case <-ctx.Done():
			w.wg.Wait()
			return nil
		case msg, ok := <-msgCh:
			if !ok {
				w.wg.Wait()
				return nil
			}
			w.wg.Add(1)
			go func(m mq.Message) {
				defer w.wg.Done()
				w.handleMessage(ctx, m)
			}(msg)
		}
	}
}

func (w *IndexWorker) handleMessage(parentCtx context.Context, msg mq.Message) {
	var payload IndexPayload
	if err := json.Unmarshal([]byte(msg.Payload), &payload); err != nil {
		logger.L(parentCtx).Error("malformed index message, routing to DLQ", "error", err)
		_ = w.broker.PublishDLQ(parentCtx, mq.QueueIndexTask, msg, "malformed_json: "+err.Error())
		if msg.AckFunc != nil {
			_ = msg.AckFunc()
		}
		return
	}

	ctx := context.WithValue(parentCtx, logger.SnapshotIDKey, payload.SnapshotID)
	logger.L(ctx).Info("processing repository indexing", "repo_id", payload.RepositoryID, "snapshot_id", payload.SnapshotID)

	targetDir := w.storeFS.GetSourcePath(payload.RepositoryID, payload.SnapshotID)

	// Step 1: Materialize Snapshot if needed
	var commitSHA string
	if _, err := os.Stat(targetDir); os.IsNotExist(err) {
		sha, err := w.cloner.CloneTo(ctx, payload.GitURL, payload.Ref, targetDir)
		if err != nil {
			logger.L(ctx).Error("failed to clone repository for snapshot", "error", err)
			_ = w.snapshotStore.UpdateStatus(ctx, payload.SnapshotID, snapshot.StatusMaterializing, snapshot.StatusMaterializeFailed, nil)
			_ = w.indexStore.UpdateStatus(ctx, payload.IndexID, repoindex.StatusIndexQueued, repoindex.StatusIndexFailed, nil, 0, 0, "CLONE_FAILED: "+err.Error())
			if msg.AckFunc != nil {
				_ = msg.AckFunc()
			}
			return
		}
		commitSHA = sha
	} else {
		commitSHA = "cached"
	}

	now := time.Now()
	_ = w.snapshotStore.UpdateStatus(ctx, payload.SnapshotID, snapshot.StatusMaterializing, snapshot.StatusReady, &now)

	// Step 2: Chunk files
	_ = w.indexStore.UpdateStatus(ctx, payload.IndexID, repoindex.StatusIndexQueued, repoindex.StatusIndexing, nil, 0, 0, "")

	var allChunks []CodeChunk
	docCount := 0

	err := w.storeFS.WalkFiles(payload.RepositoryID, payload.SnapshotID, func(relPath string, info os.FileInfo) error {
		if info.IsDir() {
			if w.filter.ShouldIgnoreDir(info.Name()) {
				return filepath.SkipDir
			}
			return nil
		}

		if w.filter.ShouldIgnoreFile(relPath, info.Size()) {
			return nil
		}

		content, err := w.storeFS.ReadFile(ctx, payload.RepositoryID, payload.SnapshotID, relPath, 1, -1)
		if err != nil {
			return nil // skip unreadable
		}

		chunks := w.chunker.ChunkFile(payload.SnapshotID, relPath, content)
		allChunks = append(allChunks, chunks...)
		docCount++
		return nil
	})

	if err != nil {
		logger.L(ctx).Error("failed to walk snapshot files for chunking", "error", err)
		_ = w.indexStore.UpdateStatus(ctx, payload.IndexID, repoindex.StatusIndexing, repoindex.StatusIndexFailed, nil, 0, 0, "CHUNKING_FAILED: "+err.Error())
		if msg.AckFunc != nil {
			_ = msg.AckFunc()
		}
		return
	}

	// Compute overall snapshot content hash
	h := sha256.New()
	for _, ch := range allChunks {
		h.Write([]byte(ch.ContentHash))
	}
	_ = hex.EncodeToString(h.Sum(nil))

	// Step 3: Save chunks in index store
	if w.indexWriter != nil {
		if err := w.indexWriter.IndexChunks(ctx, payload.SnapshotID, allChunks); err != nil {
			logger.L(ctx).Error("failed to write chunks to index store", "error", err)
			_ = w.indexStore.UpdateStatus(ctx, payload.IndexID, repoindex.StatusIndexing, repoindex.StatusIndexFailed, nil, 0, 0, "INDEX_WRITE_FAILED: "+err.Error())
			if msg.AckFunc != nil {
				_ = msg.AckFunc()
			}
			return
		}
	}

	readyAt := time.Now()
	_ = w.indexStore.UpdateStatus(ctx, payload.IndexID, repoindex.StatusIndexing, repoindex.StatusReady, &readyAt, len(allChunks), docCount, "")

	logger.L(ctx).Info("indexing completed successfully",
		"snapshot_id", payload.SnapshotID,
		"commit", commitSHA,
		"chunks", len(allChunks),
		"documents", docCount,
	)

	if msg.AckFunc != nil {
		_ = msg.AckFunc()
	}
}
