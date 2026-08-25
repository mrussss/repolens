# RepoLens Failure Semantics & Reliability Specification

## 1. Reliability Matrix

| Failure Mode | Trigger / Scenario | System Behavior & Mitigation | Guaranteed Outcome |
| :--- | :--- | :--- | :--- |
| **Duplicate Message Delivery** | RabbitMQ network blip, unacknowledged message redelivery. | Consumer queries `DiagnosisRun` status; if terminal (`SUCCEEDED`/`FAILED`), it safely ACKs immediately without re-executing agent tools. | Exact-once business execution. |
| **Worker Process Crash** | Worker terminated (`SIGKILL`/OOM) while running task. | Heartbeat stops updating. `RecoverySweeper` detects expired heartbeat (>30s), marks Attempt #1 as `ABANDONED`, transitions Run to `RETRY_WAIT`, and creates retry outbox event. | Zero zombie tasks, automatic retry on new worker node. |
| **LLM 429 Rate Limit** | External LLM API rate limit or transient 5xx error. | Executor catches error, transitions Attempt to `FAILED_RETRYABLE`, sets Run to `RETRY_WAIT` with backoff delay, and ACKs message. | Eliminates consumer hot-looping; retry delayed exponentially. |
| **Poison / Corrupted Message** | Malformed JSON payload or unknown event schema. | Consumer unmarshal error handler catches exception, publishes to Dead-Letter Queue (`QueueDiagnosisDLQ`), and ACKs bad message. | Queue head-of-line blocking avoided; poison message isolated for inspection. |
| **Concurrent Worker Claim** | Two workers consume duplicate/concurrent task events. | Atomic conditional update (`WHERE id = ? AND version = ? AND status IN ('QUEUED', 'RETRY_WAIT')`). First worker succeeds; second worker receives `ErrClaimConflict`. | Zero duplicate concurrent task executions. |
| **Idempotency Key Conflict** | User resubmits `Idempotency-Key` with different payload. | SHA256 request hash comparison detects hash mismatch, immediately returning HTTP `409 Conflict`. | Prevents payload collision on identical idempotency keys. |
| **Graceful Shutdown (SIGTERM)** | Worker receiving `SIGTERM` during rolling update. | `Coordinator` stops accepting new AMQP deliveries, allows in-flight attempts up to 30s deadline to finish, flushes logs, closes DB pool, and exits. | In-flight tasks complete cleanly without partial writes. |
