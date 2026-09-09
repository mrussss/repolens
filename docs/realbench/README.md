# RealBench v1

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
