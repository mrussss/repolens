CREATE TABLE IF NOT EXISTS analysis_revisions (
    id VARCHAR(36) PRIMARY KEY,
    repository_id VARCHAR(36) NOT NULL,
    source_ref VARCHAR(255) NOT NULL,
    commit_sha VARCHAR(40) NOT NULL,
    pipeline_version VARCHAR(64) NOT NULL,
    pipeline_fingerprint VARCHAR(64) NOT NULL,
    snapshot_id VARCHAR(64) NULL,
    code_index_build_id BIGINT NULL,
    retrieval_build_id BIGINT NULL,
    status VARCHAR(32) NOT NULL,
    stage VARCHAR(64) NOT NULL,
    error_code VARCHAR(64) NULL,
    error_message TEXT NULL,
    execution_generation INT NOT NULL DEFAULT 1,
    version INT NOT NULL DEFAULT 1,
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    ready_at DATETIME(3) NULL,
    UNIQUE KEY uq_revision_identity (repository_id, commit_sha, pipeline_fingerprint),
    KEY idx_revision_repository (repository_id, created_at),
    KEY idx_revision_status (status, stage)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

ALTER TABLE repository_snapshots ADD COLUMN analysis_revision_id VARCHAR(36) NULL, ADD KEY idx_snapshot_revision (analysis_revision_id);
ALTER TABLE code_index_builds ADD COLUMN analysis_revision_id VARCHAR(36) NULL, ADD KEY idx_code_index_revision (analysis_revision_id);
ALTER TABLE code_index_builds ADD COLUMN quality_warnings_json TEXT NULL;
ALTER TABLE retrieval_builds ADD COLUMN analysis_revision_id VARCHAR(36) NULL, ADD KEY idx_retrieval_revision (analysis_revision_id);
ALTER TABLE diagnosis_runs ADD COLUMN analysis_revision_id VARCHAR(36) NULL, ADD COLUMN pipeline_fingerprint VARCHAR(64) NOT NULL DEFAULT '', ADD KEY idx_diagnosis_revision (analysis_revision_id);
ALTER TABLE diagnosis_attempts ADD COLUMN raw_output MEDIUMTEXT NULL, ADD COLUMN parsed_report_json MEDIUMTEXT NULL, ADD COLUMN structured_output_valid BOOLEAN NOT NULL DEFAULT FALSE, ADD COLUMN provider_completed_at DATETIME(3) NULL, ADD COLUMN cached_prompt_tokens INT NOT NULL DEFAULT 0, ADD COLUMN reasoning_tokens INT NOT NULL DEFAULT 0, ADD COLUMN agent_rounds INT NOT NULL DEFAULT 0, ADD COLUMN search_calls INT NOT NULL DEFAULT 0, ADD COLUMN provider_calls INT NOT NULL DEFAULT 0, ADD COLUMN finalization_reason VARCHAR(64) NULL;
ALTER TABLE reports ADD COLUMN report_status VARCHAR(32) NOT NULL DEFAULT 'INVALID', ADD COLUMN conclusion_kind VARCHAR(32) NOT NULL DEFAULT '', ADD COLUMN summary TEXT NULL, ADD COLUMN raw_output MEDIUMTEXT NULL, ADD COLUMN parse_error TEXT NULL, ADD COLUMN limitations_json TEXT NULL, ADD COLUMN model_claimed_confidence DOUBLE NULL, ADD COLUMN finding_count INT NOT NULL DEFAULT 0, ADD COLUMN supported_finding_count INT NOT NULL DEFAULT 0, ADD COLUMN unsupported_finding_count INT NOT NULL DEFAULT 0, ADD COLUMN valid_citation_count INT NOT NULL DEFAULT 0, ADD COLUMN invalid_citation_count INT NOT NULL DEFAULT 0, ADD COLUMN citation_coverage DOUBLE NULL, ADD COLUMN finalization_reason VARCHAR(64) NULL;
