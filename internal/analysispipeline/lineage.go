package analysispipeline

// ResolvedLineage identifies one validated READY analysis revision and its
// derived artifacts. Legacy callers may leave RevisionID empty.
type ResolvedLineage struct {
	RepositoryID string
	RevisionID   string
	CommitSHA    string

	SnapshotID       string
	CodeIndexBuildID int64
	RetrievalBuildID int64

	PipelineVersion     string
	PipelineFingerprint string
}
