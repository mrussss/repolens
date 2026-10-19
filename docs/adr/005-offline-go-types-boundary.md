# ADR 005：离线 Go 类型分析边界

## 状态

已采纳并实现。

## 背景

完整的 `go/types` 分析通常需要下载外部 module。RepoLens 不执行仓库命令，也不为静态分析访问网络；缺失依赖时类型检查可能无法解析完整关系。

## 决策

1. `internal/codeintel/importer/` 从内存中的 AST 分析仓库内部 package。
2. 对无法离线解析的外部 package 使用合成的空 `types.Package` 占位，使分析可以继续。
3. 将解析和类型检查质量作为 AnalysisQuality 展示；离线结果属于 best-effort，不伪装成完整类型分析。
