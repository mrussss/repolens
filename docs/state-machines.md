# RepoLens State Machines & Transition Specification

## 1. DiagnosisRun Lifecycle

```mermaid
stateDiagram-v2
    [*] --> QUEUED: API POST /diagnoses (run + job transaction)
    QUEUED --> RUNNING: Worker starts an attempt
    RUNNING --> SUCCEEDED: Agent finishes, citations verified & report saved
    RUNNING --> FAILED: Retries exhausted or terminal non-retryable error
    RUNNING --> CANCELLED: User requested cancellation via /diagnoses/:id/cancel
    QUEUED --> CANCELLED: User requested cancellation before worker claim
```

### Transition Invariants
- `QUEUED`: No worker holds execution rights; retry scheduling belongs to `AnalysisJob`.
- `RUNNING`: The current `DiagnosisAttempt` owns execution progress.
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
