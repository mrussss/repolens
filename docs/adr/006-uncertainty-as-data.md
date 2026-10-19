# ADR 006：将分析不确定性作为数据保存

## 状态

已采纳并实现。

## 背景

外部依赖缺失或类型检查不完整时，静态分析不能可靠解析所有符号关系。隐藏 unresolved 情况或假设确定性会误导后续检索和诊断。

## 决策

1. 每条 SymbolRelation 记录解析类型：`SEMANTIC`、`SYNTACTIC`、`HEURISTIC` 或 `UNRESOLVED`。
2. 每个 CodeIndexBuild 保存解析、类型检查、符号及关系质量统计和 warning。
3. API 与界面展示 AnalysisQuality，帮助用户理解静态分析的覆盖边界。
