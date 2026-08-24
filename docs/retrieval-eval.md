# RepoLens 检索策略对比与离线评测体系

## 1. 实验驱动的检索演进

系统拒绝盲目引入外部重型中间件，通过基线数据集进行四阶段对比实验：

1. **Stage A: Lexical Baseline**
   - 符号匹配、大小写敏感过滤、精准错误码匹配。
2. **Stage B: Code-aware Chunking + BM25**
   - CamelCase/snake_case 分词、语言感知函数/类型提取、Okapi BM25 排序。
3. **Stage C: Dense Vector Search**
   - 语义向量空间计算，弥补无精确 symbol 的模糊意图召回（如“哪里处理连接关闭后的旧异步结果？”）。
4. **Stage D: Hybrid RRF Fusion**
   - 使用 Reciprocal Rank Fusion: $RRF(d) = \sum \frac{1}{k + r(d)}$，在 Go 业务层融合稀疏与稠密检索结果。

---

## 2. 评测指标与可复现性记录

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
