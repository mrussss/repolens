# Provider execution semantics

## Provider execution guarantee

RepoLens uses at-least-once Provider execution. A Diagnosis request can make an
external call whose final outcome RepoLens cannot observe.

### Live execution

While the Worker process is alive and still owns the job claim, a confirmed
`PROVIDER_OUTCOME_UNKNOWN` result is classified as permanent for automatic job
execution. The Worker preserves that failure policy through cancel-poll errors
and failure-finalization recovery, so the job does not enter the automatic
retry queue. An explicit diagnosis retry remains available to the user.

Ownership loss and claim fencing still take precedence: a worker that no longer
owns the claim cannot finalize it. Once the claim is lost, normal lease recovery
rules apply.

### Process crash recovery

If the process crashes after dispatching a Provider request, RepoLens may not
know whether the Provider completed the work. The job can later be recovered
and executed again. An external Provider call MAY repeat, including a billable
call or another external side effect.

This is the at-least-once crash recovery contract. RepoLens does not guarantee
at-most-once external execution after a hard process crash.

### Future upgrade path

If stronger behavior is required, a future execution model could add Provider
idempotency keys, a durable Provider execution ledger, and external outcome
reconciliation. Together, where supported by the Provider, those mechanisms
could provide effectively-once external effects. They are not part of the
current contract.
