# RepoLens Failure Semantics & Reliability Specification

## 1. Reliability Matrix

| Failure Mode | Trigger / Scenario | System Behavior & Mitigation | Guaranteed Outcome |
| :--- | :--- | :--- | :--- |
| **Duplicate job delivery** | Multiple workers observe the same pending database job. | Conditional claim with `worker_id`, `claim_token`, lease, and row locking admits one owner. | No concurrent execution under one lease. |
| **Worker process crash** | Worker terminates while running a task. | Lease expiry moves the `AnalysisJob` to `RETRY_WAIT`; the attempt is marked `ABANDONED`, while the Diagnosis remains `RUNNING`. | Automatic retry without a false diagnosis terminal state. |
| **LLM 429 / transient 5xx** | External provider rate limit or temporary failure. | Executor returns a classified retryable error; the job applies bounded exponential backoff. | No hot loop; retry state is not exposed as Diagnosis status. |
| **Malformed job payload** | Invalid resource ID or unsupported job type. | Handler returns a permanent categorized error; the job is terminal `FAILED` with error metadata. | Poison work is isolated in the database. |
| **Concurrent worker claim** | Two workers race for one pending job. | Atomic claim requires the current job state and generation; the loser receives `ErrOwnershipLost`/claim conflict. | No duplicate concurrent execution. |
| **Idempotency Key Conflict** | User resubmits `Idempotency-Key` with different payload. | SHA256 request hash comparison detects hash mismatch, immediately returning HTTP `409 Conflict`. | Prevents payload collision on identical idempotency keys. |
| **Graceful Shutdown (SIGTERM)** | Worker receives a termination signal. | The worker stops claiming new jobs, cancels the parent context, and waits for in-flight handlers and lease renewers. | Existing ownership is not silently transferred by a second executor. |
