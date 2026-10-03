-- Durable boundary for provider requests made by a Diagnosis Attempt.
-- Existing attempts have no known unresolved dispatch and remain replayable
-- according to their existing checkpoint state.
ALTER TABLE diagnosis_attempts
    ADD COLUMN provider_dispatch_unresolved BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN provider_dispatch_success_seen BOOLEAN NOT NULL DEFAULT FALSE;
