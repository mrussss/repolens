# ADR 001: Repository Code Retrieval Strategy

## Status

Superseded by the v2.1 final freeze.

## Context

RepoLens v2.1 is a local single-user diagnosis tool. Retrieval must be reproducible over a pinned `Snapshot → CodeIndexBuild → RetrievalBuild` chain, preserve exact identifiers and paths, and avoid a mandatory external search cluster.

The v1.1 benchmark and its Elasticsearch/vector/RRF experiments remain historical evidence only. They are not part of the v2.1 production runtime.

## Decision

1. Use pure-Go BM25 (`internal/retrieval/bm25`) as the production retrieval baseline.
2. Use versioned Structural Retrieval over CodeIndex symbols and relations as an additive strategy.
3. Persist RetrievalBuild identity and artifact hash. Publish artifacts atomically and verify the manifest, bytes, pinned build IDs, and READY lineage before loading.
4. Promote Structural Retrieval only when ADR 008's frozen held-out benchmark rule passes; otherwise retain BM25.
5. Keep MySQL as the transactional source of truth for business state and jobs. Do not require Elasticsearch, dense-vector services, RRF, or an outbox relay.

## Consequences

- Production startup needs only the v2.1 MySQL, API, and worker services.
- BM25 remains deterministic and has no embedding API dependency.
- Structural retrieval can be evaluated fairly against the same immutable snapshot and code-index build.
- Historical v1.1 benchmark documents must not be interpreted as current deployment instructions.
