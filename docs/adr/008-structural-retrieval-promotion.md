# ADR 008: Structural Retrieval Promotion Rule and Benchmark Evaluation

## Status
Accepted; historical promotion superseded by the current production state

## Current production state (2026-10-04 follow-up)

The original promotion conclusion below is a historical benchmark record.
The current frozen evaluation does not pass all promotion gates. Production
builds use `BM25` and actually execute Pure BM25; structural reranking requires
an explicit `BM25_STRUCTURAL` build. Strategy, retrieval version and configuration
are part of build identity and the artifact manifest strategy must match.
See [current retrieval contract](../retrieval-eval.md). Promotion rules remain
unchanged; future adoption requires a passing evaluation and an explicit
production strategy/version change.

## Context
Adding structural signals to BM25 must be proven superior on a frozen held-out test suite before being promoted to the production query path.

## Decision
1. Benchmark 4 retrieval strategies (A: V1 baseline, B: Symbol Lexical, C: Pure BM25, D: BM25 + Structural).
2. Enforce 4 frozen promotion criteria on the held-out test set:
   - Hit@5 count: D >= C
   - Mean MRR: D >= C - 0.01
   - Evidence Recall: D strictly improves on >= 2 test cases, with 0 severe regressions (>= 0.10 drop)
   - P95 Latency: D <= 1.5 * C
3. Strategy D satisfied all promotion gates and is promoted to production.
