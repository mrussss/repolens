# Provider execution semantics

## Provider execution guarantee

Provider retry behavior depends on whether dispatch and its outcome are known.
The frozen `DiagnosisExecutionSpec` supplies the Provider retry budget and
identity for the run.

### Before dispatch

Failures known to occur before dispatch, such as a connection refusal during
dial, may be retried automatically by the job policy.

### Definite provider response

An explicit HTTP 429 or 5xx response is a known retryable provider response.
The Provider may retry it within the run's frozen provider retry budget; if
that budget is exhausted, the job retry policy may schedule another attempt.

### Outcome unknown after dispatch

When a request may have been dispatched but no complete response is observed,
RepoLens records `PROVIDER_OUTCOME_UNKNOWN` as a permanent job failure. It does
not automatically replay the request, since the Provider may already have
performed billable work. The user can explicitly retry the Diagnosis.

Job claim fencing still applies: a worker that has lost its claim cannot
finalize the outcome. The worker reconciles ambiguous finalization against the
durable job state while preserving the no-replay policy.

### Limits

An operator's explicit retry can issue a new Provider request. The execution
contract therefore prevents automatic replay of an unknown outcome; it does
not promise at-most-once effects across explicit retries.
