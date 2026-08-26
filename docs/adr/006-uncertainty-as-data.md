# ADR 006: Modeling Uncertainty as First-Class Data and Quality Metrics

## Status
Accepted and Implemented

## Context
Static analysis cannot achieve 100% semantic resolution when external dependencies are uncompiled. Hiding unresolved relations or faking certainty corrupts downstream reasoning.

## Decision
1. Classify every `SymbolRelation` by `resolution_kind`: `SEMANTIC` (exact typecheck), `SYNTACTIC` (AST identifier match), `HEURISTIC` (naming/package conventions), or `UNRESOLVED` (external target).
2. Calculate and persist `AnalysisQuality` breakdowns on every `CodeIndexBuild` (% parsed, % typechecked, relation distribution).
3. Expose quality statistics in the API and UI to let users understand system confidence transparently.
