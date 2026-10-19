# ADR 010：评测数据集与 Ground Truth

## 状态

已采纳并实现。

## 背景

检索和 Agent 评测需要可复现的故障样例、Ground Truth 和明确的数据切分，避免调参时泄漏 held-out 结果。

## 决策

1. 将 synthetic eval 样例分为 Dev Set 和冻结的 Held-out Set，分别用于开发观察和回归评测。
2. 使用自动校验检查标注路径、范围和文件存在性。
3. 调参期间不修改 Held-out 标注。修改数据集或 manifest 时，必须同步更新版本身份和校验记录。

## 评测边界

该数据集用于 synthetic regression，不是当前生产 Structural 检索 promotion 的最终权威评测；生产语义对照使用 RealBench paired retrieval。
