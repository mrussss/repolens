# RepoLens Failure Semantics & Reliability Specification

## 1. Reliability Matrix

| Failure Mode | Trigger / Scenario | System Behavior & Mitigation | Guaranteed Outcome |
| :--- | :--- | :--- | :--- |
| **Duplicate job delivery** | Multiple workers observe the same pending database job. | Conditional claim with `worker_id`, `claim_token`, lease, and row locking admits one owner. | No concurrent execution under one lease. |
| **Worker process crash** | Worker terminates while running a task. | Lease recovery marks the active attempt `ABANDONED` and applies job retry policy. Provider calls whose outcome is unknown after dispatch are terminalized as `PROVIDER_OUTCOME_UNKNOWN` when observed and are not automatically replayed. | Generic job recovery does not convert a confirmed unknown Provider outcome into an automatic retry. |
| **Provider pre-dispatch failure** | A request is known not to have been dispatched, such as connection refusal during dial. | Classified as retryable under the job retry policy. | Safe automatic retry before external execution. |
| **LLM 429 / transient 5xx** | Provider returns an explicit retryable HTTP response. | Provider retries within the frozen `DiagnosisExecutionSpec` budget; after exhaustion, the job retry policy may apply. | Only explicit retryable responses use Provider-level retries. |
| **Provider outcome unknown** | A request may have been dispatched but no complete response is observed. | Classified as permanent `PROVIDER_OUTCOME_UNKNOWN`; automatic replay is disabled. An explicit Diagnosis retry remains available. | Avoids silently repeating potentially billable Provider work. |
| **Malformed job payload** | Invalid resource ID or unsupported job type. | Handler returns a permanent categorized error; the job is terminal `FAILED` with error metadata. | Poison work is isolated in the database. |
| **Concurrent worker claim** | Two workers race for one pending job. | Atomic claim requires the current job state and generation; the loser receives `ErrOwnershipLost`/claim conflict. | No duplicate concurrent execution. |
| **Idempotency Key Conflict** | User resubmits `Idempotency-Key` with different payload. | SHA256 request hash comparison detects hash mismatch, immediately returning HTTP `409 Conflict`. | Prevents payload collision on identical idempotency keys. |
| **Graceful Shutdown (SIGTERM)** | Worker receives a termination signal. | The worker stops claiming new jobs, cancels the parent context, and waits for in-flight handlers and lease renewers. | Existing ownership is not silently transferred by a second executor. |
