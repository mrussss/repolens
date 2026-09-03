# RepoLens v2.1 检索策略与离线评测

## 1. 当前生产检索路径

RepoLens v2.1 当前生产使用：

```text
Pure Go BM25 + Structural Retrieval
```

- **Pure Go BM25**：进程内运行，使用代码感知 tokenizer 对文件、符号和路径进行确定性 lexical ranking；不依赖外部搜索集群或 embedding 服务。
- **Structural Retrieval**：基于固定 CodeIndexBuild 中的 symbols、references、callers 和 related tests 对候选结果进行结构化扩展，并提供可解释的命中原因。
- **版本与 lineage**：RetrievalBuild 固定 `Snapshot → CodeIndexBuild → RetrievalBuild` 链路，artifact 发布后通过 hash 和 READY 状态校验。

BM25 是稳定、可复现的生产基线；Structural Retrieval 按冻结的 held-out benchmark 规则评估，只有满足 promotion gate 才能改变生产策略。

## 2. 历史方案与实验方向

以下方案不属于当前 v2.1 生产链路：

- **Dense Vector Search / Embedding**：曾用于探索语义召回，属于 v1.x 历史方案或未来实验方向。
- **Hybrid RRF Fusion**：曾用于融合稀疏与稠密结果，属于历史实验记录或 future work；当前代码不要求 Vector DB、Embedding Provider 或 RRF。
- **Elasticsearch**：v2.1 不部署、不依赖 Elasticsearch；历史 benchmark 中出现的相关结果不能当作当前部署说明。

后续如果重新评估 Vector 或 RRF，必须新增独立实验版本和 held-out 对照，不得改变本页对当前生产方案的描述。

---

## 3. 评测元数据与可复现性

每次 EvalRun 完整记录版本元数据：
- `dataset_version`
- `git_commit`
- `snapshot_sha`
- `retrieval_strategy`
- `retrieval_version`
- `model`

核心指标包括：
- **File Hit@5 / Hit@10**：前 K 个检索候选是否包含真实故障文件；
- **MRR (Mean Reciprocal Rank)**：真实相关文件的平均倒数排名；
- **Citation Validity Rate**：报告中引用的代码路径、行号与内容在源码中的真实合法率；
- **Root Cause Success Rate**：基于规则与关键词覆盖评估根因准确率；
- **P50 / P95 Latency & Token Usage**：诊断延迟与成本。

评测必须在同一 immutable Snapshot 和固定 CodeIndexBuild 上比较 BM25 与 Structural Retrieval，避免源码、索引版本或 lineage 漂移影响结论。
