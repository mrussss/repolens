# ADR 003：派生构建的版本与 lineage

## 状态

已采纳并实现。

## 背景

Code Intelligence 和 Retrieval artifact 必须绑定确定版本和输入，避免并发构建或重复诊断时混用不同源码、分析器和索引。

## 决策

1. 产品链路为 `Repository → AnalysisRevision → Snapshot → CodeIndexBuild → RetrievalBuild`。
2. 构建身份包含相应的 parser、analyzer、symbol schema、检索策略及配置版本。
3. Snapshot 就绪后自动创建 CodeIndexBuild，CodeIndexBuild 就绪后自动创建 RetrievalBuild。
4. Revision 只有在一条完整、就绪且归属一致的 lineage 上才能进入 READY；既有 READY 资源不原地替换。
