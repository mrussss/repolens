# ADR 013: Build Lineage Invariant and Cross-Build Integrity

## Status
Accepted and Implemented

## Context
A diagnosis request or agent query must not mix snapshots, code intelligence tables, and retrieval builds across disparate repository commits or parent lineages.

## Decision
1. Enforce strict lineage invariant checks on all diagnosis creations and queries:
   - `snapshot.repository_id == diagnosis.repository_id`
   - `code_index_build.snapshot_id == diagnosis.snapshot_id`
   - `retrieval_build.code_index_build_id == code_index_build.id`
2. Reject any mismatched combination with HTTP 409 `BUILD_LINEAGE_MISMATCH`.
