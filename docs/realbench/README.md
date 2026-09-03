# RealBench v1

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

`--e2e` 仅在配置真实 OpenAI-compatible provider 后才会进入 Agent 路径；没有 provider 配置时，结果会明确标记为 `NOT_RUN_PROVIDER_NOT_CONFIGURED`，不会使用 FakeProvider 冒充真实 E2E。

配置 provider 后可显式运行 E2E：

```bash
export REPOLENS_REALBENCH_BASE_URL=https://api.example.com/v1
export REPOLENS_REALBENCH_MODEL=your-model
export REPOLENS_REALBENCH_API_KEY=your-key
export REPOLENS_REALBENCH_AUTH_MODE=bearer  # 或 none
go run ./cmd/realbench run --all --e2e
```
