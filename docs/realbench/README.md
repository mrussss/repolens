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

runner 会固定 checkout 每个 case 的 buggy SHA，执行现有 CodeIndex 和当前生产 Pure Go BM25（构建策略 `BM25`；Structural 尚未通过 promotion），并将结果写入 `artifacts/realbench/<run-id>/`。源码缓存位于 `.cache/realbench/`，两者都不提交。

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

真实 Agent E2E 的 pilot 记录见 [`../history/benchmarks/realbench/v1-agent-e2e.md`](../history/benchmarks/realbench/v1-agent-e2e.md)。其中的 API Key 只通过运行环境传入，不写入命令示例、Git 或 benchmark artifact；Retrieval 正式 baseline 仍见 [`../history/benchmarks/realbench/v1-baseline.md`](../history/benchmarks/realbench/v1-baseline.md)。

## RealBench v2

v2 使用独立的 `testdata/realbench/v2/` frozen dataset，包含 10 个来自 8 个公开 Go 仓库的真实历史 Bug；每个仓库最多 2 个 case。v1 的 3 个 case、输入、Ground Truth 和 manifest hash 完全保留。

```bash
go run ./cmd/realbench validate --dataset v2
go run ./cmd/realbench run --dataset v2 --all
```

不传 `--dataset` 仍运行 v1，保持既有命令兼容。v2 离线 baseline 和真实 Provider E2E 证据分别见 [`../history/benchmarks/realbench/v2-baseline.md`](../history/benchmarks/realbench/v2-baseline.md) 与 [`../history/benchmarks/realbench/v2-agent-e2e.md`](../history/benchmarks/realbench/v2-agent-e2e.md)。

`realbench-v1` 是第一版 pilot external benchmark，由 3 个真实 Go 项目历史 Bug 组成，用于验证完整外部评测链路，不代表大规模真实世界泛化结论；v2 同样是小规模外部历史 Bug 证据，不是 production accuracy 声明。

## Production-consistent paired retrieval

```bash
go run ./cmd/realbench validate --dataset v2
go run ./cmd/realbench compare-retrieval --dataset v2 --all
go run ./cmd/realbench compare-retrieval --dataset v2 --case REAL-004
```

支持已有 `--data`、`--cache`、`--artifacts`；无需 Provider，不运行 LLM 或 Agent。
既有 `validate`、`run` 命令与其 Top10/full-input query 契约保留。
索引文档现在统一复用生产 Symbol builder，包含 pinned Snapshot 的源码正文。

每 case 使用 exact buggy commit、独立 Snapshot、一次 CodeIndex 分析；两个
RetrievalBuild 共享 CodeIndexBuild、版本和文档，分别固定 BM25 与 BM25_STRUCTURAL
及各自 ConfigHash、manifest strategy。相同 index bytes 的 ArtifactHash 相同。
两边均执行 ProductionRetriever.Search，覆盖 build pinning、artifact 校验、strategy dispatch
及真实 CodeIntel store / related-test 查询。Query 复用生产 BuildQuery，TopK 固定 8。

指标只使用 PrimaryFiles：Recall@8 对路径 normalize 后去重，按 case 平均；
RR 使用首个命中的 Symbol 结果排名，MRR 按 case 平均。SupportingFiles 仅展示。
better/equal/worse 先比较 Recall@8，再比较 RR。ground truth 在预测结果落盘后读取。

每次 run 写入：

- `retrieval_compare.json`：逐 case identity、构建、query、原始 Top8、耗时、ground truth、指标及差值。
- `retrieval_compare_metrics.json`：完成/失败数、聚合指标及 paired case 列表。
- `retrieval_compare_report.md`：汇总与可读 Top8 对照。
- `cases/REAL-NNN/retrieval_compare_prediction.json`：评分前的无标签预测。

聚合只使用完整 pairs，明确报告分母；失败 case 保留 status / error_class / error，
全失败不生成虚假的零分聚合。失败分类为 EXTERNAL_INFRA 或 REPOLENS_PRODUCT；
有未完成 case 时 CLI 返回非零 exit status，结果仍写入 artifacts。

Latency 为一次 ProductionRetriever.Search 的 wall clock 毫秒，保留浮点精度，
包含冷 artifact 加载，不含分析/建索引；按 case 交替先搜索策略。p50/p95 使用排序后
线性插值。它反映本次机器和缓存条件，只用于观察，无 promotion gate。
本轮不调整 Structural 算法，也不改变 BM25 production default。
旧 `cmd/eval` 只作为 legacy/synthetic regression，不能替代该生产语义对照。
