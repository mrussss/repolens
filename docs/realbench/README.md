# RealBench

> `realbench-v1` 是第一版 pilot external benchmark，由 3 个真实 Go 项目历史 Bug 组成，用于验证完整外部评测链路，不代表大规模真实世界泛化结论。

RealBench v1 的输入和 Ground Truth 分离存放在 `testdata/realbench/v1/`。校验完全离线：

```bash
go run ./cmd/realbench validate
```

运行生产检索链路：

```bash
go run ./cmd/realbench run --case REAL-001
go run ./cmd/realbench run --all
```

runner 会固定 checkout 每个 case 的 buggy SHA，执行现有 CodeIndex、Pure Go BM25 + Structural Retrieval，并将结果写入 `artifacts/realbench/<run-id>/`。源码缓存位于 `.cache/realbench/`，两者都不提交。

每个完成的 case 还会生成 `analysis_quality.json`，记录文件解析、包 type-check、符号、关系、相关测试和 warning 分布；run 根目录的 `analysis_quality_summary.csv` 汇总这些 Code Intelligence 质量指标。`realbench-v1` 仍是 3 个真实 Go 项目的 pilot，不代表大规模真实世界泛化结论。

`--e2e` 仅在配置真实 OpenAI-compatible provider 后才会进入 Agent 路径；没有 provider 配置时，结果会明确标记为 `NOT_RUN_PROVIDER_NOT_CONFIGURED`，不会使用 FakeProvider 冒充真实 E2E。

配置 provider 后可显式运行 E2E：

```bash
export REPOLENS_REALBENCH_BASE_URL=https://api.example.com/v1
export REPOLENS_REALBENCH_MODEL=your-model
export REPOLENS_REALBENCH_API_KEY=your-key
export REPOLENS_REALBENCH_AUTH_MODE=bearer  # 或 none
go run ./cmd/realbench run --all --e2e
```

真实 Agent E2E 的 pilot 记录见 [`results/v1-agent-e2e.md`](results/v1-agent-e2e.md)。其中的 API Key 只通过运行环境传入，不写入命令示例、Git 或 benchmark artifact；Retrieval 正式 baseline 仍见 [`results/v1-baseline.md`](results/v1-baseline.md)。

## RealBench v2

v2 使用独立的 `testdata/realbench/v2/` frozen dataset，包含 10 个来自 8 个公开 Go 仓库的真实历史 Bug；每个仓库最多 2 个 case。v1 的 3 个 case、输入、Ground Truth 和 manifest hash 完全保留。

```bash
go run ./cmd/realbench validate --dataset v2
go run ./cmd/realbench run --dataset v2 --all
```

不传 `--dataset` 仍运行 v1，保持既有命令兼容。v2 离线 baseline 和真实 Provider E2E 证据分别见 [`results/v2-baseline.md`](results/v2-baseline.md) 与 [`results/v2-agent-e2e.md`](results/v2-agent-e2e.md)。

`realbench-v1` 是第一版 pilot external benchmark，由 3 个真实 Go 项目历史 Bug 组成，用于验证完整外部评测链路，不代表大规模真实世界泛化结论；v2 同样是小规模外部历史 Bug 证据，不是 production accuracy 声明。
