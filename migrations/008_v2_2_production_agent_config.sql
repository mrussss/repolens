ALTER TABLE diagnosis_runs
    ADD COLUMN reasoning_effort VARCHAR(32) NOT NULL DEFAULT '';

ALTER TABLE diagnosis_runs
    ALTER COLUMN max_output_tokens SET DEFAULT 4096;
