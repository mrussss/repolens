# ADR 001: Repository Code Retrieval Strategy & Elasticsearch Hybrid Architecture

## Status
Accepted (v1.1 Freeze)

## Context & Problem
RepoLens performs automated root cause diagnosis for complex software repositories given user issues and CI/error logs. Repository diagnosis demands high-precision retrieval across large codebases, where exact symbol matches (function names, struct types, file paths, error codes) must be balanced with semantic conceptual queries (e.g., "memory leak due to unclosed socket", "JWT clock skew after midnight").

We need to establish a deterministic, reproducible retrieval architecture that meets the following criteria:
1. **Low Latency & High Precision**: File Hit@5 and Hit@10 must reliably capture relevant fault sites.
2. **Deterministic Infrastructure**: MySQL remains the business source of truth (diagnoses, attempts, reports, outbox); search indexes must remain cleanly separated.
3. **Reproducibility**: Retrieval algorithms must be evaluated against a standard regression dataset (32 curated fault cases) across versions.

## Evaluated Baselines & Experiments

We conducted benchmarks on the curated 32-case repository fault dataset with 4 distinct retrieval strategies:

1. **Lexical Baseline (`LEXICAL`)**:
   - Exact symbol and token matching using substring and word boundaries.
   - High speed, but vulnerable to vocabulary mismatch and synonym variance.
2. **BM25 Search (`BM25`)**:
   - Probabilistic term frequency-inverse document frequency ranking with length normalization and symbol weight boosting (`content^1.0`, `symbol^3.0`, `path^2.0`).
   - Strong retrieval across technical symbols, identifier camelCase splits, and error log fragments (MRR: 0.562).
3. **Deterministic Local Hashed Feature Vector Baseline (`LOCAL_HASHED_VEC`)**:
   - Deterministic 128-dimensional hashed feature representation provider with cosine similarity fallback.
   - Provides reproducible baseline without external LLM/API dependencies (MRR: 0.491).
4. **Hybrid Reciprocal Rank Fusion Baseline (`HYBRID_BASELINE`)**:
   - Rank-based reciprocal rank score fusion combining BM25 ranking and Vector ranking:
     $$RRF\_Score(d) = \sum_{m \in \{BM25, Vector\}} \frac{1}{k + rank_m(d)}, \quad k = 60$$
   - Eliminates score scale incompatibility between BM25 scores and cosine similarity.

## Benchmark Results (32 Curated Fault Cases on Static Repositories)

| Strategy | File Hit@5 | File Hit@10 | MRR | Latency P50 (ms) | Complexity / Resource Cost |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **Lexical Baseline** | 56.2% | 62.5% | 0.554 | < 1ms | Low (In-Memory substring / token match) |
| **BM25 Search** | **59.4%** | **62.5%** | **0.562** | ~ 3ms | Moderate (BM25 term statistics + field boosts) |
| **Local Hashed Vec** | 59.4% | 62.5% | 0.491 | ~ 2ms | Low (128-dim deterministic token hashing) |
| **Hybrid Baseline** | 59.4% | 62.5% | 0.535 | ~ 3ms | Moderate (Two-phase RRF rank merge) |
| **E2E Diagnostic Agent** | 59.4% | 62.5% | 0.535 | ~ 7ms | Higher (Agent loop + tool dispatch + validation) |

## Failure Analysis & Trade-offs
- **Lexical failure cases**: Missed cases where the issue description used conceptual synonyms without mentioning the exact symbol name.
- **Hashed Feature failure cases**: Local deterministic hash representation does not capture deep neural semantics (unlike trained dense neural embeddings like `text-embedding-3-small`), resulting in lower MRR (0.491 vs BM25 0.562).
- **Hybrid RRF advantages**: Prevents score scale incompatibility between BM25 raw scores and dense vector cosine distances, providing rank stability.

## Decision
1. **Production Primary**: Deploy **BM25 + Hybrid RRF** as the production retrieval pipeline.
2. **Storage Separation**:
   - **Elasticsearch 8** is utilized strictly as the Code Retrieval & Chunk Index.
   - **MySQL 8** remains the sole transactional state machine and business source of truth.
3. **Local & Test Fallback**:
   - If Elasticsearch is not reachable, the system transparently utilizes the built-in in-memory BM25/RRF retriever for standalone execution and offline evaluation.

## Rejected Alternatives
- **Pure Vector / Dense Retrieval Only**: Rejected due to lower symbol hit accuracy on exact compiler/runtime error logs.
- **Learning-to-Rank (LTR)**: Rejected for V1 as it introduces unnecessary ML pipeline complexity without significant gain over RRF.
- **Vector DBs (Milvus / Pinecone / Qdrant)**: Rejected to prevent infrastructure bloat; Elasticsearch 8 natively supports dense vectors (`dense_vector` + kNN) and BM25 unified in one engine.
