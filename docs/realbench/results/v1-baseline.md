# RepoLens RealBench v1 Baseline

> `realbench-v1` 是第一版 pilot external benchmark，由 3 个真实 Go 项目历史 Bug 组成，用于验证完整外部评测链路，不代表大规模真实世界泛化结论。

## Run metadata

- Dataset: `realbench-v1`
- Manifest hash: `5b63f6e3ce1437c2d9e57dbb410530b54eca1f6a64590a29f4f639379768b9bf`
- RepoLens commit: `0a2cbd2d17c6ed9e45444a0d269900addeca8c1d`
- Run ID: `20260921T103622Z-b0f118ce`
- Command: `go run ./cmd/realbench run --case REAL-001`; `go run ./cmd/realbench run --all`
- Validation: `go run ./cmd/realbench validate` — PASS; single-case run — PASS
- Retrieval: `symbol_bm25_structural` — current Pure Go BM25 + Structural Retrieval
- Retrieval / index version: `v2.1.0` / `v2.1.0`; Agent / Prompt version: `v2.2.1` / `v2.2-evidence-1`
- E2E: `NOT_REQUESTED`
- Quality artifacts: per-case `analysis_quality.json` and run-level `analysis_quality_summary.csv`
- Environment: `go1.22.12`, `linux/amd64`, provider timeout `60s`, temperature `0.1`
- Working tree at baseline run: clean; the user-requested root development MD is excluded locally through `.git/info/exclude` and is not part of the repository.

## Summary

| Metric | Count | Rate |
|---|---:|---:|
| Total Cases | 3 | — |
| Completed Cases | 3 | — |
| Infra Errors | 0 | — |
| Product Failures | 0 | — |
| Evaluated Cases | 3 | — |
| E2E status | NOT_REQUESTED | — |
| File Hit@5 | 3/3 | 100.0% |
| File Hit@10 | 3/3 | 100.0% |
| MRR | — | 0.778 |

## Per-case results

| Case | Repository | Top-10 primary-file rank | Hit@5 | Hit@10 | Latency |
|---|---|---:|---:|---:|---:|
| REAL-001 | go-chi/chi | 1 | yes | yes | 12 ms |
| REAL-002 | spf13/cobra | 3 | yes | yes | 16 ms |
| REAL-003 | hashicorp/go-retryablehttp | 1 | yes | yes | 3 ms |

## Code Intelligence quality

The raw per-case quality files and CSV summary are stored in the run artifact directory. The current run reports the following completeness and uncertainty distribution:

| Case | Files parsed / total | Packages typechecked / total | Symbols | Semantic relations | Unresolved relations | Symlinks skipped | Related tests | Parse rate | Typecheck rate |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| REAL-001 | 66 / 67 | 11 / 12 | 357 | 1195 | 1778 | 0 | 12065 | 98.5% | 91.7% |
| REAL-002 | 35 / 36 | 0 / 2 | 613 | 2267 | 1524 | 0 | 74484 | 97.2% | 0.0% |
| REAL-003 | 4 / 4 | 0 / 1 | 77 | 176 | 372 | 0 | 1395 | 100.0% | 0.0% |

这些质量指标保留 type-check 不完整和 unresolved 分布，不将静态分析结果包装成完整 runtime call graph。

## Failure analysis

本次在 clean HEAD `e24ac06` 重跑没有 Retrieval failure、Infra Error 或 Product Failure，因此没有隐藏失败 case。REAL-002 的 primary file 排名为 3，仍命中 Hit@5，但相较另外两个 case 需要更多候选排序空间；这个观察仅记录为 benchmark 证据，不在本任务中修改检索算法。

本次没有请求 E2E（`--e2e` 未传），因此没有生成真实 Agent diagnosis、Citation validity 或 Root Cause Correct/Partial/Incorrect 分数；FakeProvider 不作为公开 E2E 成绩。若请求 E2E 但未配置 provider，状态会单独记录为 `NOT_RUN_PROVIDER_NOT_CONFIGURED`。

## Agent E2E

真实 Provider 的 Agent E2E 见 [v1-agent-e2e.md](v1-agent-e2e.md)。本文件的 headline 数字仍只代表无 Provider 依赖的 Retrieval baseline，不把小样本 Agent 试跑结果合并进 Retrieval 指标。
