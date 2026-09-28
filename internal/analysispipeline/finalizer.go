package analysispipeline

import (
	"context"
	"time"

	codeintelmodel "repolens/internal/codeintel/model"
)

// JobOwnership contains exactly the claim identity accepted by the existing
// stage transactions. Generation handling stays in those transactions.
type JobOwnership struct {
	JobID      int64
	WorkerID   string
	ClaimToken string
}

type SnapshotStageResult struct {
	Ownership        JobOwnership
	RevisionID       string
	SnapshotID       string
	MaterializedPath string
	ModulePath       string
	CommitSHA        string
	ContentHash      string
	FileCount        int
	TotalBytes       int64
	ReadyAt          time.Time
}

type CodeIndexStageResult struct {
	Ownership        JobOwnership
	RevisionID       string
	CodeIndexBuildID int64
	AnalysisResult   *codeintelmodel.AnalysisResult
}

type RetrievalStageResult struct {
	Ownership        JobOwnership
	RevisionID       string
	RetrievalBuildID int64
	ArtifactPath     string
	ArtifactHash     string
	DocCount         int
}

type snapshotRevisionFinalizer interface {
	FinalizeSnapshotSuccessWithRevision(ctx context.Context, jobID int64, workerID, claimToken, snapshotID, materializedPath, revisionID, modulePath, commitSHA, contentHash string, fileCount int, totalBytes int64, readyAt time.Time) error
}

type codeIntelRevisionFinalizer interface {
	FinalizeCodeIndexSuccessWithRevision(ctx context.Context, jobID int64, workerID, claimToken string, buildID int64, revisionID string, result *codeintelmodel.AnalysisResult) error
	FinalizeRetrievalSuccessWithRevision(ctx context.Context, jobID int64, workerID, claimToken string, buildID int64, revisionID, artifactPath, artifactHash string, docCount int) error
}

// Finalizer owns the product stage submission boundary. The stores retain
// their existing claim-fenced cross-resource SQL transactions.
type Finalizer struct {
	snapshot  snapshotRevisionFinalizer
	codeintel codeIntelRevisionFinalizer
}

func NewFinalizer(snapshot snapshotRevisionFinalizer, codeintel codeIntelRevisionFinalizer) *Finalizer {
	return &Finalizer{snapshot: snapshot, codeintel: codeintel}
}

func (f *Finalizer) FinalizeSnapshot(ctx context.Context, result SnapshotStageResult) error {
	return f.snapshot.FinalizeSnapshotSuccessWithRevision(ctx,
		result.Ownership.JobID, result.Ownership.WorkerID, result.Ownership.ClaimToken,
		result.SnapshotID, result.MaterializedPath, result.RevisionID, result.ModulePath,
		result.CommitSHA, result.ContentHash, result.FileCount, result.TotalBytes, result.ReadyAt,
	)
}

func (f *Finalizer) FinalizeCodeIndex(ctx context.Context, result CodeIndexStageResult) error {
	return f.codeintel.FinalizeCodeIndexSuccessWithRevision(ctx,
		result.Ownership.JobID, result.Ownership.WorkerID, result.Ownership.ClaimToken,
		result.CodeIndexBuildID, result.RevisionID, result.AnalysisResult,
	)
}

func (f *Finalizer) FinalizeRetrieval(ctx context.Context, result RetrievalStageResult) error {
	return f.codeintel.FinalizeRetrievalSuccessWithRevision(ctx,
		result.Ownership.JobID, result.Ownership.WorkerID, result.Ownership.ClaimToken,
		result.RetrievalBuildID, result.RevisionID, result.ArtifactPath, result.ArtifactHash, result.DocCount,
	)
}
