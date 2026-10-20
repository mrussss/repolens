# ADR 004：稳定的符号身份与源码坐标

## 状态

已采纳并实现。

## 背景

Go 方法接收者可能使用值、指针或泛型类型写法；同名声明和 `init()` 也需要稳定且可复现的身份。

## 决策

1. 符号原始键由 module、package、规范化接收者、kind 和名称组成，再计算 SHA256 得到 `symbol_key_hash`。
2. 方法接收者规范化时去掉外层括号、指针标记和泛型参数。
3. 合法的重复 `init()` 声明在哈希前追加 Snapshot 相对路径及物理起始坐标，普通符号键保持原格式。
4. 符号范围、关系坐标和相关测试坐标使用 Snapshot 中的物理位置；Go `//line` 指令不会改变坐标契约。
5. 持久化时原子拒绝重复符号哈希。
6. 符号提取只处理 `ast.File.Decls` 中的包级函数、方法、类型和接口声明。函数体以及包级变量初始化闭包中的局部声明不进入包级 Symbol Identity。

包级提取边界修复使用 AnalyzerVersion `v2.2.4`，通过版本化 CodeIndexBuild 与 pipeline fingerprint 重建派生产物。合法包级符号的 key 格式、SymbolSchemaVersion、ParserVersion 和 RetrievalVersion 保持既有契约；历史 READY build 和 artifact 继续按原身份固定。
