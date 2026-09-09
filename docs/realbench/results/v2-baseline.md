# RepoLens RealBench v2 Retrieval Baseline

> `realbench-v2` is a small external historical-bug benchmark. It is evidence for the complete evaluation chain, not a claim of production accuracy or broad real-world generalization.

## Frozen run metadata

- Dataset: `realbench-v2`
- Manifest hash: `a7fa7d404d37b9a5b94c6f448d8bf3c4ad26ee4fad7b2f2ce4b3b442936af1dc`
- Cases: 10
- Public repositories: 8
- RepoLens code-under-test commit: `80f73597787393fe9f358380da5eb9c9979a4a1c`
- Run ID: `20260909T113202Z-0b9cfe0c`
- Command: `go run ./cmd/realbench run --dataset v2 --all`
- Retrieval: `symbol_bm25_structural` — Pure Go BM25 + Structural Retrieval
- Retrieval / index version: `v2.1.0` / `v2.1.0`
- E2E: `NOT_REQUESTED`
- Environment: `go1.22.12`, `linux/amd64`
- Working tree at run start: clean

## Retrieval metrics

| Metric | Count | Rate |
|---|---:|---:|
| Total cases | 10 | — |
| Completed cases | 10 | — |
| Infra errors | 0 | — |
| Product failures | 0 | — |
| Evaluated cases | 10 | — |
| File Hit@5 | 7/10 | 70.0% |
| File Hit@10 | 9/10 | 90.0% |
| MRR | — | 0.724 |

## Per-case results

| Case | Repository | Primary-file rank | Hit@5 | Hit@10 | Retrieval latency |
|---|---|---:|:---:|:---:|---:|
| REAL-004 | gin-gonic/gin | 1 | yes | yes | 45 ms |
| REAL-005 | sirupsen/logrus | 1 | yes | yes | 17 ms |
| REAL-006 | urfave/cli | 7 | no | yes | 27 ms |
| REAL-007 | urfave/cli | 10 | no | yes | 30 ms |
| REAL-008 | spf13/pflag | 1 | yes | yes | 26 ms |
| REAL-009 | spf13/pflag | 1 | yes | yes | 32 ms |
| REAL-010 | fsnotify/fsnotify | 1 | yes | yes | 10 ms |
| REAL-011 | grpc/grpc-go | — | no | no | 749 ms |
| REAL-012 | redis/go-redis | 1 | yes | yes | 276 ms |
| REAL-013 | gofiber/fiber | 1 | yes | yes | 314 ms |

`—` means that no primary relevant file appeared in the top 10. Ranks are computed from the frozen Ground Truth primary-file set after Retrieval; Ground Truth is not passed to the production query, index, or Agent path.

## AnalysisQuality summary

| Case | Files parsed / total | Packages typechecked / total | Symbols | Semantic relations | Unresolved relations |
|---|---:|---:|---:|---:|---:|
| REAL-004 | 93 / 98 | 1 / 7 | 1,435 | 3,532 | 5,003 |
| REAL-005 | 48 / 53 | 0 / 6 | 531 | 601 | 846 |
| REAL-006 | 66 / 66 | 3 / 4 | 1,183 | 2,390 | 2,507 |
| REAL-007 | 66 / 66 | 3 / 4 | 1,183 | 2,390 | 2,507 |
| REAL-008 | 72 / 74 | 0 / 1 | 1,004 | 2,348 | 1,544 |
| REAL-009 | 72 / 74 | 0 / 1 | 1,004 | 2,348 | 1,544 |
| REAL-010 | 24 / 37 | 2 / 4 | 170 | 609 | 674 |
| REAL-011 | 932 / 943 | 73 / 262 | 12,300 | 42,667 | 28,832 |
| REAL-012 | 335 / 338 | 5 / 20 | 5,763 | 12,273 | 10,084 |
| REAL-013 | 263 / 265 | 2 / 45 | 4,406 | 18,815 | 27,826 |

The raw `analysis_quality.json` files and `analysis_quality_summary.csv` are generated under the run artifact directory. Type-checking is deliberately best-effort and offline; unresolved external dependencies and nested modules remain visible rather than being presented as complete semantic analysis.

## Known limitations

- Ten cases are a pilot-scale sample from eight public repositories; repeated cases are capped at two per repository, so the result does not estimate broad generalization.
- The benchmark freezes real historical buggy and fix SHAs, but does not execute each upstream project's full test suite as part of Retrieval scoring.
- The current strategy is Pure Go BM25 plus Structural Retrieval. No Vector, Embedding, RRF, or external search service is used.
- `REAL-006`, `REAL-007`, and `REAL-011` show the current top-10 misses or lower ranks; cases and inputs are frozen and were not changed to improve the score.
- This is an offline Retrieval baseline. Provider E2E is recorded separately in [`v2-agent-e2e.md`](v2-agent-e2e.md).
