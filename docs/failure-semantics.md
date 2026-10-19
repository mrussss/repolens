# 失败处理与可靠性边界

| 情况 | 系统处理 | 保证与边界 |
| --- | --- | --- |
| 多个 Worker 竞争同一 Job | 在数据库中按状态、generation、claim token 和 lease 原子领取。MySQL 使用行锁和 `SKIP LOCKED`。 | 一个有效 lease 同时只有一个执行所有者。 |
| Worker 执行中退出 | lease 到期后由 Reaper 关闭丢失的 Attempt，并按 Job 策略恢复。 | 若 Provider 派发结果已知为 `PROVIDER_OUTCOME_UNKNOWN`，不会转成自动重试。 |
| Provider 派发前失败 | 可确认请求未发出时，按 Job 策略重试。 | 不会重复外部执行。 |
| Provider 返回明确 429/5xx | 在冻结的 Provider 重试预算内尝试；预算耗尽后可按 Job 策略安排新 Attempt。 | Provider 层只重试明确可重试的响应。 |
| Provider 派发后结果未知 | Job 终态失败并记录 `PROVIDER_OUTCOME_UNKNOWN`，不自动重放。 | 用户显式 Retry 会开始新的执行，仍可能再次计费。 |
| Job 数据无效 | Worker 记录永久分类错误并将 Job 标为 `FAILED`。 | 错误任务留在数据库中，便于诊断。 |
| 同一个幂等键搭配不同请求 | 对请求摘要进行比较，不一致时返回 HTTP `409 Conflict`。 | 不会把不同请求误当成同一次提交。 |
| Worker 收到 SIGTERM | 停止领取新 Job，取消父上下文并等待处理中任务和 lease renewer 收尾。 | 已有执行权不会被第二个 Worker 悄悄接管；退出后由 lease recovery 处理。 |

## 报告大小限制

`ValidateReportStructure` 对序列化后的完整报告（含 JSON 转义和 canonical Citation excerpt）设置 4 MiB 上限。持久化的报告 JSON 字段、raw output、parsed report 和 report draft 也各有 4 MiB 边界。迁移 `018_v2_2_report_capacity.sql` 将 `findings_json`、`recommended_checks_json` 和 `limitations_json` 扩展为 MEDIUMTEXT；摘要、根因和解析错误等有界 TEXT 字段上限为 65,535 字节。

写入前会检查字节数。超限以永久错误 `REPORT_TOO_LARGE` 结束，不自动重试 Provider，也不依赖数据库返回 `Data too long`。最终 Citation 校验还会重新序列化并检查完整报告大小。
