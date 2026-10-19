# ADR 001：检索引擎与生产策略

## 状态

已采纳。当前生产构建策略为 BM25；Structural 检索仍是显式选择的实验路径。

## 背景

RepoLens v1 依赖 Elasticsearch、稠密向量和 RRF 融合。对于本地单用户工具，这些外部服务增加了启动和维护成本。v2 引入 Go AST Code Intelligence 与进程内 BM25。

## 决策

1. 使用纯 Go BM25 和代码感知 tokenizer 作为默认生产检索路径，不要求外部搜索集群或 embedding 服务。
2. RetrievalBuild 固定策略、版本和 lineage；artifact manifest、hash 与 READY build 必须一致。
3. Structural 检索保留为显式的实验构建策略。当前评测未达到 promotion 条件，默认生产请求不执行结构扩展或 rerank。
4. 旧 Elasticsearch 和早期 regex/window 检索不属于当前运行时依赖。

## 当前约束

生产默认是 `RetrievalBuild.Strategy=BM25)，并实际执行 Pure BM25。实验结果及复现命令见[检索与评测说明](../retrieval-eval.md)和[RealBench 使用指南](../realbench/README.md)。小规模历史 Bug 指标不构成生产诊断准确率声明。
