# ADR 002：从固定文本窗口迁移到 Go Code Intelligence

## 状态

已采纳并实现。

## 背景

RepoLens v1 按固定行数切分源码，可能把语法结构和函数上下文拆开，降低检索及后续诊断的可读性。

## 决策

1. 使用 `internal/codeintel/` 中基于 Go AST 的分析。
2. 为源码文件、符号和符号关系保存版本化分析结果，并保留解析质量信息。
3. 旧 regex/window baseline 仅用于历史评测和兼容，不参与当前生产检索。
