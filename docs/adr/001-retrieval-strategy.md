# ADR 001: 检索架构选型与 Hybrid RRF 决策

## 状态
已封版 (Accepted & Frozen)

## 背景
在代码故障诊断场景中，用户提交的 Query 既包含明确的 Symbol/Error Code（如 `Nil pointer in config.go`），也包含高度抽象的语义化描述（如“处理超时重试的边界逻辑在哪”）。

## 评估与实验结果
在 32 个真实故障用例的标准评测集上进行了四种检索策略对比：

1. **Lexical Baseline**：对精确符号和堆栈关键词效果优异，但对语义同义词无法召回，MRR 为 0.72。
2. **BM25 (Code-Aware)**：引入驼峰分词与 TF-IDF 权重后，符号与分词匹配大幅提升，Hit@5 达到 93.8%，MRR 为 0.88。
3. **Dense Vector Search**：对概念与功能性描述召回率高，但对精确行号与符号存在一定漂移。
4. **Hybrid RRF (Reciprocal Rank Fusion)**：在 Go 服务层结合 BM25 与 Dense 结果，Hit@5 达到 100%，MRR 达到 1.000。

## 决策
1. 生产主链路默认使用 **Hybrid RRF** 策略，兼顾符号精确性与语义模糊召回；
2. 检索产物与 `RepositorySnapshot` 彻底解耦，检索索引版本演进不影响已持久化的代码事实与 Citation 验证。
