# RepoLens v2.2 检索策略与离线评测

## 1. 当前生产检索路径

RepoLens v2.2 当前生产使用：

```text
Pure Go BM25 (RetrievalBuild.Strategy = BM25)
```

- **Pure Go BM25**：进程内运行，使用代码感知 tokenizer 对文件、符号和路径进行确定性 lexical ranking；不依赖外部搜索集群或 embedding 服务。
- **Structural Retrieval（实验）**：旧 synthetic gate 当前未通过（不代表完整生产 Structural 的质量结论）；只在显式 `BM25_STRUCTURAL` 构建上执行，默认生产请求不进行结构化扩展或 rerank。
- **版本与 lineage**：RetrievalBuild 固定 `Snapshot → CodeIndexBuild → RetrievalBuild` 链路，artifact 发布后通过 hash 和 READY 状态校验。
- **Initial Retrieval**：Agent 启动前由确定性的 QueryBuilder 从标题、少量描述关键词和错误堆栈中的包/文件/函数/符号提取 query，再以固定 Top-K 和总字节预算生成 Evidence Packet。完整 Error Log 不会原样拼进 BM25 query。
- **Evidence Packet**：候选至少包含仓库相对路径、起止行、源码 excerpt、检索分数和 `BM25` 原因；重复或高度重叠候选去重后才进入 Agent。

BM25 是稳定、可复现的生产基线；生产语义对照使用 RealBench paired retrieval；本轮仅观察指标，不执行 promotion。正式晋升仍需要后续独立验收。

## 2. 历史方案与实验方向

以下方案不属于当前 v2.2 生产链路：

- **Dense Vector Search / Embedding**：曾用于探索语义召回，属于 v1.x 历史方案或未来实验方向。
- **Hybrid RRF Fusion**：曾用于融合稀疏与稠密结果，属于历史实验记录或 future work；当前代码不要求 Vector DB、Embedding Provider 或 RRF。
- **Elasticsearch**：v2.2 不部署、不依赖 Elasticsearch；历史 benchmark 中出现的相关结果不能当作当前部署说明。

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

旧 synthetic regression 的 `Hit@1`、`Hit@5` 和 MRR 共享一个 relevance 契约：结果的符号精确匹配
`ExpectedSymbol`，或文件精确匹配任意 `ExpectedFiles`，即为 relevant。
两类 ground truth 同时存在时，它们表示可接受的相关证据目标，采用 OR 语义。
最早的真实结果排名决定 Hit@K 和倒数排名；没有命中时均为零。
文件 Recall 使用去重后的期望文件集合，不重复计数。仅有符号 ground truth 时，
Recall 保持前五名符号命中的既有语义。BM25 和 Structural 使用同一 helper。

## 4. 构建与实际执行契约

`ProductionRetriever` 按 pinned `RetrievalBuild.Strategy` 分流：`BM25` 直接执行
`idx.Search(query, TopK)`，结果标记 `RetrievalSource=symbol_bm25`、
`RetrievalReason=BM25`；`BM25_STRUCTURAL` 调用历史 V1 flat-rerank Engine，
`BM25_STRUCTURAL_V2` 调用独立的实验 one-hop Engine。
未知策略返回稳定的 unsupported strategy 错误。READY、snapshot/build lineage、
artifact build ID、hash 和 manifest strategy 均在搜索前核验。

当前 AnalyzerVersion 为 `v2.2.2`，RetrievalVersion 为 `v2.2.1`。
策略、检索版本、tokenizer 版本及配置 SHA256 均参与构建唯一身份；配置 hash 包含
BM25 k1=1.2、b=0.75 和对应结构参数。Pipeline fingerprint 的生产策略为 `BM25`，
结构参数为 `none`，避免复用修复前的排序和 semantic related-test 结果。
Artifact manifest 固定 build ID、strategy 和 index hash；build ID 关联其完整版本身份。
历史 READY 构建保持原有 pinned identity，标记 BM25 的构建现在实际执行 BM25。

`go run ./cmd/eval` 保留旧 synthetic regression 与 `CheckPromotionRule`，不硬编码结果；
该 gate 不是 BM25_STRUCTURAL production promotion 的最终权威评测。
`go run ./cmd/realbench compare-retrieval --dataset v2 --all` 通过 ProductionRetriever
对照单 Snapshot 的两种 pinned strategy，使用 BuildQuery、TopK=8、PrimaryFiles、Recall@8/MRR。
详见 [RealBench 使用说明](realbench/README.md)。本轮不建立新 promotion gate，生产默认仍为 BM25。
ADR 001/008 的早期 promotion 记录属于历史结论，本页定义当前生产状态。

## 5. Bounded semantic expansion V2（实验）

- `BM25`：production default，保持原 BM25 文档、k1=1.2、b=0.75 和 tokenizer。
- `BM25_STRUCTURAL`：历史 experimental V1 flat rerank，四个 boost、ConfigHash 和行为保留。
- `BM25_STRUCTURAL_V2`：experimental bounded one-hop semantic expansion；不叠加 V1 boost，不调用 ListRelatedTests。

V2 使用 BM25 Top2K（本评测 TopK=8，即 Top16），只从原 BM25 Top8 seeds 扩展。
固定 seeds<=8、每 seed 新增<=1、query 总新增<=4、depth=1；只接受 confidence>=0.95 的
forward SEMANTIC CALL_CANDIDATE 和 reverse SEMANTIC TEST_RELATION / DIRECT_SEMANTIC_USAGE。
REFERENCE、reverse call、弱 test relation、多跳与扩大 lexical depth 均不参与。

专用 store 边界按 pinned build、seed ID/hash、方向和 relation 条件过滤，join 当前 build 的 resolved Symbol，
每类 DISTINCT 查询 LIMIT 8；最多 8 次 symbol lookup + 16 次 relation query，返回关系行最多 128。
该 lookahead 上限同样进入 V2 ConfigHash；数据库内部扫描量不等于返回行数。
候选必须在 pinned artifact 中存在，按 SymbolKeyHash 去重；原 Top16 邻居不获得额外 placement。
旧 BM25/V1 hash 的原始字符串保持不变，RetrievalVersion 无需统一升级。

新增候选 effective_rank=seed_rank+1；同位 base 优先，然后 call 优先、seed rank、SymbolKeyHash。
每 seed 内 target 以 hash ASC、confidence DESC、reason code ASC 稳定选择，不进行 query-relevance 学习。
重复 target 保留最早 seed 和所有**有界查询实际观测到的**去重来源；它不保证收集全部图边。
V2 返回数组的顺序是最终排名。`Score` 保留原 base lexical score，新增候选为 0，**不是 V2 的排序键**。
`structural_v2` typed trace 解释实际 placement；RealBench 另保存完整合并 pool 和 query/row 计数。

```bash
go run ./cmd/realbench compare-retrieval --dataset v2 --all --candidate BM25_STRUCTURAL_V2
```

现有不传 `--candidate` 命令继续比较 BM25 vs V1。
这 10 个 RealBench v2 cases 已用于 failure analysis：**DEV / DIAGNOSTIC EVIDENCE ONLY**，
不能作为 V2 promotion held-out。全量 dev stop/go 要求新增 PrimaryFile 真正进入 Top8、
mean Recall@8>0.85 且高于配对 BM25、无逐 case recall regression、零 product failure、严格预算。
`V2_DEV_GO` 仅允许准备 3~5 个 untouched Go bugs；`STOP_STRUCTURAL` 表示停止投资，production 保持 BM25。
本轮不执行 unseen validation 或 promotion。
