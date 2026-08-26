# ADR 002: Evolution from Regex Windows to Structural Code Intelligence

## Status
Accepted and Implemented

## Context
RepoLens v1 split code files into fixed 40-line text sliding windows. This created broken syntax spans, severed function declarations from their call sites, and degraded LLM reasoning.

## Decision
1. Migrate from fixed line chunking to AST-driven Code Intelligence (`internal/codeintel/`).
2. Parse repository source files into authoritative `CodeFile`, `Symbol`, and `SymbolRelation` records.
3. Keep the V1 regex/window baseline strictly under `internal/retrieval/baseline/` for evaluation and comparison only.
