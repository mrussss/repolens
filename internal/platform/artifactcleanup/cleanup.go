package artifactcleanup

import (
	"context"
	"fmt"
	"time"

	codeintelmodel "repolens/internal/codeintel/model"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/retrieval/artifact"
	"repolens/internal/snapshot"

	"gorm.io/gorm"
)

type Result struct {
	SnapshotPaths  int
	RetrievalPaths int
}

// Cleanup removes immutable execution outputs older than ttl when no snapshot
// or retrieval build row references them. Call it from a periodic maintenance
// process, outside job execution.
func Cleanup(ctx context.Context, db *gorm.DB, snapshotFS *snapshotstore.LocalSnapshotStore, retrievalPublisher *artifact.Publisher, ttl time.Duration, now time.Time) (Result, error) {
	if db == nil || snapshotFS == nil || retrievalPublisher == nil {
		return Result{}, fmt.Errorf("artifact cleanup dependencies are incomplete")
	}
	if ttl <= 0 {
		return Result{}, fmt.Errorf("artifact cleanup TTL must be positive")
	}
	var snapshotPaths []string
	if err := db.WithContext(ctx).Model(&snapshot.RepositorySnapshot{}).
		Where("materialized_path <> ''").Pluck("materialized_path", &snapshotPaths).Error; err != nil {
		return Result{}, err
	}
	var retrievalPaths []string
	if err := db.WithContext(ctx).Model(&codeintelmodel.RetrievalBuild{}).
		Where("artifact_path <> ''").Pluck("artifact_path", &retrievalPaths).Error; err != nil {
		return Result{}, err
	}
	result := Result{}
	var err error
	result.SnapshotPaths, err = snapshotFS.CleanupUnreferencedExecutionPaths(pathSet(snapshotPaths), now.Add(-ttl))
	if err != nil {
		return result, err
	}
	result.RetrievalPaths, err = retrievalPublisher.CleanupUnreferenced(pathSet(retrievalPaths), now.Add(-ttl))
	if err != nil {
		return result, err
	}
	return result, nil
}

func pathSet(paths []string) map[string]struct{} {
	refs := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if path != "" {
			refs[path] = struct{}{}
		}
	}
	return refs
}
