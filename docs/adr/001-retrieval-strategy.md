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
   - Strong retrieval across technical symbols, identifier camelCase splits, and error log fragments.
3. **Dense Vector Search (`VECTOR`)**:
   - Semantic representation via `EmbeddingProvider` (128-dim TF-IDF / 1536-dim OpenAI `text-embedding-3-small`) with cosine similarity.
   - Captures high-level semantic intent but can miss exact line-level symbol references.
4. **Hybrid Reciprocal Rank Fusion (`HYBRID_RRF`)**:
   - Rank-based reciprocal rank score fusion combining BM25 ranking and Vector kNN ranking:
     $$RRF\_Score(d) = \sum_{m \in \{BM25, Vector\}} \frac{1}{k + rank_m(d)}, \quad k = 60$$
   - Combines the lexical precision of BM25 with dense semantic generalization.

## Benchmark Results (32 Standard Fault Cases)

| Strategy | File Hit@5 | File Hit@10 | MRR | Latency P50 (ms) | Complexity / Resource Cost |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **Lexical** | 87.5% | 93.8% | 0.812 | < 5ms | Low (In-Memory) |
| **BM25** | 96.9% | 100.0% | 0.941 | < 10ms | Moderate (In-Memory / ES 8) |
| **Dense Vector** | 84.4% | 90.6% | 0.785 | ~ 25ms | Moderate (Embedding + Vector Search) |
| **Hybrid RRF** | **96.9%** | **100.0%** | **0.958** | ~ 28ms | Moderate (Two-phase merge) |

## Failure Analysis & Trade-offs
- **Lexical failure cases**: Missed cases where the issue description used conceptual synonyms (e.g., "duplicate charge on rapid double clicks") without mentioning the exact function name (`IdempotencyKey`).
- **Dense Vector failure cases**: Returned semantically related modules (e.g., general payment processing) but ranked the exact error site lower than BM25 when precise function signatures appeared in the error log.
- **Hybrid RRF advantages**: Prevents score scale incompatibility between BM25 raw BM25 scores and cosine similarity, ranking ground-truth files at Top-1 for 95%+ of cases.

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
