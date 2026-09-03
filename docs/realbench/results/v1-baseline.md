# RepoLens RealBench v1 Baseline

> `realbench-v1` 是第一版 pilot external benchmark，由 3 个真实 Go 项目历史 Bug 组成，用于验证完整外部评测链路，不代表大规模真实世界泛化结论。

## Run metadata

- Dataset: `realbench-v1`
- Manifest hash: `5b63f6e3ce1437c2d9e57dbb410530b54eca1f6a64590a29f4f639379768b9bf`
- RepoLens commit: `d457c75f8d28316646adfcd78cfbdf18e5b1b1c3`
- Run ID: `20260903T104922Z-3e07e8cd`
- Command: `go run ./cmd/realbench run --all`
- Retrieval: `symbol_bm25_structural` — current Pure Go BM25 + Structural Retrieval
- Retrieval / index version: `v2.1.0` / `v2.1.0`
- E2E: `NOT_REQUESTED`

## Summary

| Metric | Count | Rate |
|---|---:|---:|
| Total Cases | 3 | — |
| Completed Cases | 3 | — |
| Infra Errors | 0 | — |
| Product Failures | 0 | — |
| Evaluated Cases | 3 | — |
| File Hit@5 | 3/3 | 100.0% |
| File Hit@10 | 3/3 | 100.0% |
| MRR | — | 0.778 |

## Per-case results

| Case | Repository | Top-10 primary-file rank | Hit@5 | Hit@10 | Latency |
|---|---|---:|---:|---:|---:|
| REAL-001 | go-chi/chi | 1 | yes | yes | 12 ms |
| REAL-002 | spf13/cobra | 3 | yes | yes | 16 ms |
| REAL-003 | hashicorp/go-retryablehttp | 1 | yes | yes | 3 ms |

## Failure analysis

本次没有 Retrieval failure、Infra Error 或 Product Failure，因此没有隐藏失败 case。REAL-002 的 primary file 排名为 3，仍命中 Hit@5，但相较另外两个 case 需要更多候选排序空间；这个观察仅记录为后续 benchmark 证据，不在本任务中修改检索算法。

本次没有请求 E2E（`--e2e` 未传），因此没有生成真实 Agent diagnosis、Citation validity 或 Root Cause Correct/Partial/Incorrect 分数；FakeProvider 不作为公开 E2E 成绩。若请求 E2E 但未配置 provider，状态会单独记录为 `NOT_RUN_PROVIDER_NOT_CONFIGURED`。
