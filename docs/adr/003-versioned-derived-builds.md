# ADR 003: Versioned Derived Build Model and Artifact Lineage

## Status
Accepted and Implemented

## Context
Code intelligence artifacts and retrieval indexes must be deterministic, versioned, and immutable to prevent race conditions and ensure reproducibility across repeated diagnosis runs.

## Decision
1. Model analysis artifacts hierarchically: `Repository` -> `RepositorySnapshot` -> `CodeIndexBuild` -> `RetrievalBuild`.
2. Pin version identifiers: `parser_version`, `analyzer_version`, `symbol_schema_version`, and `build_context_hash`.
3. Auto-chain build generation: Snapshot materialization automatically triggers `BUILD_CODE_INDEX`, which subsequently triggers `BUILD_RETRIEVAL`.
