# ADR 012: Separation of Business State and Job Execution with Manual Requeue

## Status
Accepted and Implemented

## Context
Diagnosis runs have business states (`QUEUED`, `RUNNING`, `SUCCEEDED`, `FAILED`, `CANCELLED`) while underlying execution jobs cycle through transient retries (`RETRY_WAIT`). Mixing transient execution attempts with domain business states causes confusing UI flickering.

## Decision
1. Separate `DiagnosisRun` status from `AnalysisJob` status.
2. Allow `GET /api/v1/diagnoses/:id` to expose `execution_status` and `attempt_count` alongside business `status`.
3. Support manual requeue for jobs terminating with `RETRYABLE_EXHAUSTED` (HTTP 202), while preventing requeue of `PERMANENT` failures (HTTP 409).
