# ADR 013：Build lineage 完整性约束

## 状态

已采纳并实现。

## 背景

Diagnosis 及 Agent 查询不能混用不同 Repository、commit 或父构建的 Snapshot、CodeIndex 和 Retrieval 数据。

## 决策

创建 Diagnosis 和执行查询时校验 lineage：Snapshot 属于目标 Repository，CodeIndexBuild 属于该 Snapshot，RetrievalBuild 属于该 CodeIndexBuild。任一关系不匹配时拒绝请求并返回 `BUILD_LINEAGE_MISMATCH`。
