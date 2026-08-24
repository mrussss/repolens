# RepoLens State Machines & Transition Specification

## 1. DiagnosisRun Lifecycle

```mermaid
stateDiagram-v2
    [*] --> QUEUED: API POST /diagnoses (Atomic Transaction with OutboxEvent)
    QUEUED --> RUNNING: Worker ClaimRun (Optimistic version bump)
    RUNNING --> SUCCEEDED: Agent finishes, citations verified & report saved
    RUNNING --> RETRY_WAIT: Stale heartbeat recovery or retryable application error (429)
    RUNNING --> FAILED: Retries exhausted or terminal non-retryable error
    RETRY_WAIT --> RUNNING: Outbox Relay dispatches retry event & Worker claims
    RUNNING --> CANCELLED: User requested cancellation via /diagnoses/:id/cancel
    QUEUED --> CANCELLED: User requested cancellation before worker claim
```

### Transition Invariants
- `QUEUED`: `DiagnosisAttempt` count must be `0`. No worker holds execution rights.
- `RUNNING`: Exactly one active `DiagnosisAttempt` with status `RUNNING`.
- `RETRY_WAIT`: Associated with a pending `DIAGNOSIS_RETRY_REQUESTED` OutboxEvent where `available_at = NOW() + backoff`.
- `SUCCEEDED` / `FAILED` / `CANCELLED`: Terminal states. No subsequent transitions allowed.

---

## 2. DiagnosisAttempt Lifecycle

```mermaid
stateDiagram-v2
    [*] --> RUNNING: Created upon ClaimRun (AttemptNo = N)
    RUNNING --> SUCCEEDED: Diagnosis executed, citations valid
    RUNNING --> FAILED_RETRYABLE: LLM rate limit (429) or transient network error
    RUNNING --> FAILED_TERMINAL: Fatal execution failure or syntax error
    RUNNING --> ABANDONED: Worker crash detected by Recovery Sweeper
```

### Transition Invariants
- `Heartbeat`: Workers must refresh `heartbeat_at` periodically (default: every 5s).
- `ABANDONED`: An attempt marked as `ABANDONED` cannot be resumed by any worker. A new attempt (Attempt #N+1) is spawned instead.
