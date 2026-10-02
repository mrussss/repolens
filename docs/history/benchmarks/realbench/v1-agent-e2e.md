# RepoLens RealBench v1 Agent E2E Pilot

> `realbench-v1` 是第一版 pilot external benchmark，由 3 个真实 Go 项目历史 Bug 组成，用于验证完整外部评测链路，不代表大规模真实世界泛化结论。

本记录是一次真实 OpenAI-compatible Provider 的小样本 Agent E2E 验证，不是生产准确率声明，也不改变 frozen dataset。API Key 未写入文档、artifact、trace 或日志。

## Provider preflight

- Provider：AIHubMix OpenAI-compatible endpoint
- Model：`coding-glm-5.3-flash`
- Auth：Bearer
- `is_configured=true`
- `is_demo=false`
- Settings connection test：PASS，HTTP 200，约 4519 ms
- Agent timeout used for exploratory runs：180 seconds
- Temperature：`0.1`

Preflight 的普通 completion 通过了 Settings connection test；真实 Agent 路径还额外验证了工具调用、结构化报告解析和 Citation validation。运行时只从本地 secret 文件读取 Key，并在进程环境中注入 RealBench 所需配置，不记录 Key 值。

## Exploratory runs

### Single-case confirmation

Run ID：`20260909T093344Z-41157c70`

- Code commit：`b02e9757ad7ac21ab7e25e6a3916f00ffa2b3b52`
- Command：`go run ./cmd/realbench run --case REAL-001 --e2e`
- Case：`REAL-001`
- Retrieval：Hit@5 / Hit@10 命中，MRR `1.000`
- Agent：`E2E_COMPLETED`
- Citation：4 total, 4 valid, 0 invalid
- Infra errors：0
- Product failures：0

### Three-case exploratory run

Run ID：`20260909T093635Z-d3d14211`

- Code commit：`dc2309ea8fc35086b6c06707590832084e1141a1`
- Command：`go run ./cmd/realbench run --all --e2e`
- Total / completed：3 / 3
- Retrieval Hit@5：3/3
- Retrieval Hit@10：3/3
- Retrieval MRR：`0.778`
- Infra errors：0
- Product failures：1
- E2E status：`E2E_FAILURE`

| Case | Agent E2E | Citation result | Failure classification | Detail |
|---|---|---|---|---|
| REAL-001 | completed | 4 / 4 valid | — | Structured report and citations validated |
| REAL-002 | failed | not produced | `REPOLENS_PRODUCT` | Agent reached bounded 12-tool-call limit |
| REAL-003 | completed in this exploratory run | 0 / 0 | — | Earlier response used prose fallback; not treated as final citation evidence |

### Follow-up validation

The current parser accepts both the agent contract's `path` and persisted `file_path`, and prioritizes fenced JSON after prose containing braces. A later `REAL-003` retry at current code commit `7b49389a08b20a7b4d164b187f05036333867487` reached an external Provider timeout and was recorded as:

- Run ID：`20260909T095101Z-b632f0e2`
- E2E：`E2E_FAILURE`
- Infra errors：1
- Product failures：0
- Failure：Provider request exceeded the 180-second timeout

These failures remain visible as evidence. They are not converted into an accuracy score or omitted from the record. The current offline Retrieval baseline remains the formal v1 baseline and is recorded separately in [v1-baseline.md](v1-baseline.md).

## Scope boundary

This is a 3-case pilot over three real historical Go bugs. It demonstrates that the chain can execute Retrieval, real Provider calls, bounded Agent behavior, structured output handling, and Citation validation under both product and external failure conditions. It does not establish broad real-world generalization, production accuracy, or a stable root-cause success rate.
