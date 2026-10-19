# ADR 012：业务状态与 Job 执行状态分离

## 状态

已采纳并实现。

## 背景

Diagnosis 的业务状态为 `QUEUED`、`RUNNING`、`SUCCEEDED`、`FAILED` 或 `CANCELLED`；底层 Job 还会经历 `RETRY_WAIT`。混合两者会让业务状态随瞬时重试反复变化。

## 决策

1. 分开保存 DiagnosisRun 与 AnalysisJob 状态；API 同时返回业务状态和执行信息。
2. 自动执行重试由 Job 管理，等待期间 Diagnosis 保持 RUNNING。
3. 对符合条件的重试耗尽任务提供显式重新提交；永久失败不允许通过该入口重排。
