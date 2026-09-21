CREATE TABLE attempt_evidence_items (
    id VARCHAR(64) PRIMARY KEY,
    attempt_id VARCHAR(36) NOT NULL,
    diagnosis_run_id VARCHAR(36) NOT NULL,
    snapshot_id VARCHAR(64) NOT NULL,
    code_index_build_id BIGINT NOT NULL DEFAULT 0,
    source_kind VARCHAR(32) NOT NULL,
    source_step_seq INT NULL,
    retrieval_chunk_id VARCHAR(128) NULL,
    file_path VARCHAR(512) NOT NULL,
    start_line INT NOT NULL,
    end_line INT NOT NULL,
    total_lines INT NOT NULL DEFAULT 0,
    raw_content_hash VARCHAR(64) NOT NULL,
    display_excerpt MEDIUMTEXT NOT NULL,
    redaction_applied BOOLEAN NOT NULL DEFAULT FALSE,
    truncated BOOLEAN NOT NULL DEFAULT FALSE,
    created_at DATETIME(3) NOT NULL,
    INDEX idx_attempt_evidence (attempt_id, created_at),
    INDEX idx_attempt_evidence_run (diagnosis_run_id),
    INDEX idx_snapshot_evidence (snapshot_id, file_path),
    UNIQUE KEY uq_attempt_evidence_identity (attempt_id, snapshot_id, file_path, start_line, end_line, raw_content_hash),
    CHECK (start_line > 0),
    CHECK (end_line >= start_line)
);

ALTER TABLE citations ADD COLUMN evidence_id VARCHAR(64) NULL;

ALTER TABLE diagnosis_attempts ADD COLUMN parsed_report_draft_json MEDIUMTEXT NULL;
ALTER TABLE diagnosis_attempts ADD COLUMN checkpoint_prompt_version VARCHAR(64) NULL;
ALTER TABLE diagnosis_attempts ADD COLUMN checkpoint_agent_version VARCHAR(64) NULL;
