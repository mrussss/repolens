-- A new analysis pipeline may prepare the same immutable commit again. Keep
-- one snapshot per repository, commit, and AnalysisRevision while preserving
-- the legacy/direct snapshot identity as the empty revision ID.
UPDATE repository_snapshots SET analysis_revision_id = '' WHERE analysis_revision_id IS NULL;
ALTER TABLE repository_snapshots MODIFY COLUMN analysis_revision_id VARCHAR(36) NOT NULL DEFAULT '';
CREATE UNIQUE INDEX uq_snapshot_repo_commit_revision ON repository_snapshots (repository_id, commit_sha, analysis_revision_id);
DROP INDEX uq_snapshot_repo_commit ON repository_snapshots;
