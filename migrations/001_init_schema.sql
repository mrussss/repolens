-- RepoLens Schema v1.1 Final Freeze

CREATE TABLE IF NOT EXISTS users (
    id VARCHAR(36) PRIMARY KEY,
    email VARCHAR(255) NOT NULL UNIQUE,
    password_hash VARCHAR(255) NOT NULL,
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS repositories (
    id VARCHAR(36) PRIMARY KEY,
    user_id VARCHAR(36) NOT NULL,
    name VARCHAR(128) NOT NULL,
    git_url VARCHAR(512) NOT NULL,
    default_ref VARCHAR(128) NOT NULL DEFAULT 'main',
    status VARCHAR(32) NOT NULL DEFAULT 'ACTIVE',
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    INDEX idx_user_status (user_id, status),
    INDEX idx_user_created (user_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS repository_snapshots (
    id VARCHAR(36) PRIMARY KEY,
    repository_id VARCHAR(36) NOT NULL,
    commit_sha VARCHAR(64) NOT NULL,
    ref VARCHAR(128) NOT NULL,
    materialized_path VARCHAR(512) NOT NULL,
    content_hash VARCHAR(64) NOT NULL,
    status VARCHAR(32) NOT NULL DEFAULT 'CREATED',
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    ready_at DATETIME(3) NULL,
    INDEX idx_repo_commit (repository_id, commit_sha),
    INDEX idx_repo_status_created (repository_id, status, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS repository_indices (
    id VARCHAR(36) PRIMARY KEY,
    snapshot_id VARCHAR(36) NOT NULL,
    strategy VARCHAR(32) NOT NULL,
    index_version VARCHAR(32) NOT NULL DEFAULT 'v1',
    status VARCHAR(32) NOT NULL DEFAULT 'CREATED',
    chunk_count INT NOT NULL DEFAULT 0,
    document_count INT NOT NULL DEFAULT 0,
    embedding_model VARCHAR(64) NULL,
    embedding_version VARCHAR(32) NULL,
    error_code VARCHAR(64) NULL,
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    ready_at DATETIME(3) NULL,
    INDEX idx_snapshot_strategy (snapshot_id, strategy),
    INDEX idx_snapshot_status (snapshot_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS outbox_events (
    id VARCHAR(36) PRIMARY KEY,
    aggregate_type VARCHAR(64) NOT NULL,
    aggregate_id VARCHAR(36) NOT NULL,
    event_type VARCHAR(64) NOT NULL,
    payload TEXT NOT NULL,
    status VARCHAR(32) NOT NULL DEFAULT 'PENDING',
    retry_count INT NOT NULL DEFAULT 0,
    available_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    published_at DATETIME(3) NULL,
    INDEX idx_status_available (status, available_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS diagnosis_runs (
    id VARCHAR(36) PRIMARY KEY,
    user_id VARCHAR(36) NOT NULL,
    repository_id VARCHAR(36) NOT NULL,
    snapshot_id VARCHAR(36) NOT NULL,
    issue_title VARCHAR(255) NOT NULL,
    issue_description TEXT NULL,
    error_log MEDIUMTEXT NULL,
    status VARCHAR(32) NOT NULL DEFAULT 'QUEUED',
    cancel_requested BOOLEAN NOT NULL DEFAULT FALSE,
    idempotency_key VARCHAR(128) NOT NULL,
    idempotency_request_hash VARCHAR(64) NOT NULL,
    final_attempt_id VARCHAR(36) NULL,
    version INT NOT NULL DEFAULT 1,
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    UNIQUE INDEX uq_user_idemp (user_id, idempotency_key),
    INDEX idx_repo_created (repository_id, created_at),
    INDEX idx_status_created (status, created_at),
    INDEX idx_user_created (user_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS diagnosis_attempts (
    id VARCHAR(36) PRIMARY KEY,
    diagnosis_run_id VARCHAR(36) NOT NULL,
    attempt_no INT NOT NULL,
    worker_id VARCHAR(64) NOT NULL,
    status VARCHAR(32) NOT NULL DEFAULT 'RUNNING',
    started_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    heartbeat_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    deadline_at DATETIME(3) NOT NULL,
    finished_at DATETIME(3) NULL,
    error_code VARCHAR(64) NULL,
    error_message TEXT NULL,
    retryable BOOLEAN NOT NULL DEFAULT FALSE,
    model VARCHAR(64) NULL,
    prompt_tokens INT NOT NULL DEFAULT 0,
    completion_tokens INT NOT NULL DEFAULT 0,
    tool_calls INT NOT NULL DEFAULT 0,
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    INDEX idx_run_attempt (diagnosis_run_id, attempt_no),
    INDEX idx_status_heartbeat (status, heartbeat_at),
    INDEX idx_status_deadline (status, deadline_at),
    INDEX idx_worker (worker_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS reports (
    id VARCHAR(36) PRIMARY KEY,
    diagnosis_run_id VARCHAR(36) NOT NULL,
    attempt_id VARCHAR(36) NOT NULL,
    root_cause TEXT NOT NULL,
    findings_json TEXT NOT NULL,
    recommended_checks_json TEXT NULL,
    confidence DOUBLE NOT NULL DEFAULT 0.0,
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    INDEX idx_run (diagnosis_run_id),
    INDEX idx_attempt (attempt_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS citations (
    id VARCHAR(36) PRIMARY KEY,
    report_id VARCHAR(36) NULL,
    snapshot_id VARCHAR(36) NOT NULL,
    file_path VARCHAR(255) NOT NULL,
    start_line INT NOT NULL,
    end_line INT NOT NULL,
    excerpt TEXT NULL,
    reason VARCHAR(255) NULL,
    content_hash VARCHAR(64) NULL,
    validation_status VARCHAR(32) NOT NULL DEFAULT 'UNCHECKED',
    validation_error VARCHAR(255) NULL,
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    INDEX idx_report (report_id),
    INDEX idx_snapshot (snapshot_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS agent_steps (
    id VARCHAR(36) PRIMARY KEY,
    attempt_id VARCHAR(36) NOT NULL,
    seq INT NOT NULL,
    step_type VARCHAR(32) NOT NULL,
    tool_name VARCHAR(64) NULL,
    tool_args_summary TEXT NULL,
    tool_result_summary MEDIUMTEXT NULL,
    status VARCHAR(32) NOT NULL DEFAULT 'COMPLETED',
    latency_ms BIGINT NOT NULL DEFAULT 0,
    input_tokens INT NOT NULL DEFAULT 0,
    output_tokens INT NOT NULL DEFAULT 0,
    error_code VARCHAR(64) NULL,
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    INDEX idx_attempt_seq (attempt_id, seq)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
