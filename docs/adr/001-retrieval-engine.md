# ADR 001: Retrieval Engine Architecture & Promotion Decision

## Status
Accepted and Promoted to Production

## Context
RepoLens v1 relied on an external Elasticsearch 8 service and dense vector indexing with RRF fusion. In a single-tenant local-first developer tool, this incurred significant operational overhead (requiring heavy JVM containers, complex lifecycle management, and external cluster syncing) without providing syntax-aware code navigation.

In RepoLens v2.1, we designed and benchmarked four candidate retrieval tracks:
- **Track A (Baseline)**: V1 regex text window search (Eval only).
- **Track B**: Symbol Lexical Search.
- **Track C**: Self-written pure Go BM25 index with AST tokenization.
- **Track D**: Symbol BM25 + Structural Code Intelligence expansion (exact symbol match, callers, references, related tests).

## Decision
1. **Adopt Pure Go BM25 Engine with Code-Aware Tokenizer (`internal/retrieval/bm25`)**:
   - Zero external infrastructure dependency (runs in-process with minimal memory footprint).
   - Code-aware subword tokenization (splits camelCase, snake_case, punctuation while preserving exact identifiers).
   - Sub-millisecond P95 retrieval latency.

2. **Structural Code Intelligence Boosting (`internal/retrieval/structural`)**:
   - Integrates authoritative symbol tables and relations from `code_index_builds`.
   - Explains search rankings with deterministic structural reasons (`EXACT_SYMBOL_MATCH`, `RELATED_TEST_DISCOVERY`, `TEST_CONTEXT_MATCH`).

3. **Atomic Artifact Publishing (`internal/retrieval/artifact`)**:
   - Index builds are staged in `.tmp/<build_id>-<token>/` and verified with SHA256 checksums before atomic rename to `<build_id>/`.
   - Guaranteed immutable snapshot pinning: diagnoses only search pinned `READY` retrieval builds.

4. **Deprecate and Remove Legacy Elasticsearch**:
   - Elasticsearch container, Docker service, and legacy search adapters are permanently removed.
   - V1 regex/window baseline is isolated to `internal/retrieval/baseline/` solely for benchmark reproducibility.

## Evaluation & Promotion Results
Evaluated on the paired held-out benchmark suite:
- **Symbol Hit@1**: 100.0% (Tracks C & D)
- **Symbol Hit@5**: 100.0% (Tracks C & D)
- **Mean MRR**: 1.000 (D >= C - 0.01 satisfied)
- **P95 Latency**: < 1.0ms across test cases (D <= 1.5 * C satisfied)
- **Degradation Cases**: 0 cases degraded.

**Winning Strategy**: `SYMBOL_BM25_STRUCTURAL` (Promoted).
