-- Persist diagnosis attempt generations and typed provider checkpoints.
-- MySQL DDL auto-commits, so the production migrator applies these operations
-- with idempotent preflight/recovery semantics. Keep the runner's definitions
-- in internal/platform/mysql/migrate.go aligned with this final schema.
ALTER TABLE diagnosis_attempts
    ADD COLUMN execution_generation INT NOT NULL DEFAULT 1 AFTER diagnosis_run_id,
    ADD COLUMN checkpoint_kind VARCHAR(32) NOT NULL DEFAULT 'NONE' AFTER parsed_report_draft_json,
    ADD COLUMN checkpoint_error_code VARCHAR(64) NULL AFTER checkpoint_kind,
    ADD COLUMN checkpoint_error_message VARCHAR(255) NULL AFTER checkpoint_error_code;

-- Existing provider output cannot be distinguished reliably as final or partial.
UPDATE diagnosis_attempts
SET checkpoint_kind = 'LEGACY_UNTYPED'
WHERE raw_output <> '' OR finish_reason <> '';

ALTER TABLE diagnosis_attempts
    ADD UNIQUE KEY uq_attempt_run_generation_no (diagnosis_run_id, execution_generation, attempt_no);
