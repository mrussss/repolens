# Provider 执行语义

自动重试取决于请求是否派发，以及 Provider 的执行结果是否明确。每次 Diagnosis 使用冻结的 `DiagnosisExecutionSpec` 中的 Provider 身份和重试预算。

## 请求派发前

能够确认请求尚未发出的错误（例如建立连接时被拒绝）可按 Job 重试策略自动重试。

## 收到明确响应

明确收到 HTTP 429 或 5xx 时，Provider 可在本次执行冻结的重试预算内重试。预算耗尽后，Job 重试策略可能安排新的执行尝试。

## 派发后结果未知

请求可能已经发出，但没有收到完整响应时，系统将其记录为永久 Job 错误 `PROVIDER_OUTCOME_UNKNOWN`，不会自动重放请求，因为 Provider 可能已经完成并计费。用户仍可显式重试 Diagnosis；显式重试可能再次产生 Provider 调用。

Job 的 claim fencing 仍然有效：已失去执行权的 Worker 不能提交终态。若终态写入响应含糊，Worker 会根据数据库中的持久状态核对结果，同时保留未知结果不自动重放的规则。

## 保证边界

系统禁止对未知结果进行自动重放，但不承诺跨用户显式重试的 at-most-once 外部副作用。
