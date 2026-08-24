# RepoLens 故障语义与可靠性设计 (Failure Semantics)

## 1. 传输层重投递 (Transport Redelivery) 与应用层重试 (Application Retry) 的严格分离

| 维度 | Transport Redelivery | Application Retry |
|---|---|---|
| **触发原因** | Worker 在 ACK 前 Crash、MQ 网络抖动、ACK 丢失 | 外部 LLM 429、5xx 瞬时服务不可用、IO 临时超时 |
| **处理策略** | 消息重新投递，Consumer 依靠 MySQL 幂等与状态机校验 | Attempt 标记为 `FAILED_RETRYABLE`，Run 置为 `RETRY_WAIT`，生成带 `available_at` 指数退避的 OutboxEvent |
| **MQ 行为** | 显式重投递 | **安全 ACK 当前消息**（杜绝 hot nack loop 导致 CPU 100%），等待退避到期后由 Relay 发布新任务 |

---

## 2. Worker Crash 与 Stale Attempt 恢复机制

1. Worker 执行期间在独立 Goroutine 中定时更新 Attempt 的 `heartbeat_at`。
2. 独立的 **Recovery Sweeper** 定期扫描 `Attempt.status = RUNNING AND (heartbeat_at < now - 30s OR deadline_at < now)`。
3. 发现超时尝试后，在单一事务中：
   - 将该 Attempt 状态置为 `ABANDONED`；
   - 将 DiagnosisRun 置为 `RETRY_WAIT`；
   - 插入 `OutboxEvent(DIAGNOSIS_RETRY_REQUESTED)`。
4. 下一个周期的 Worker 将 Claim 该 Run 并创建 Attempt #N+1 进行无缝接续。

---

## 3. 死信队列 (DLQ) 边界划分

DLQ 仅用于基础设施和数据格式故障：
- 畸形 JSON 数据（无法反序列化）；
- 未知 EventType；
- 缺失关键关联 ID 的 Poison Message。

业务重试耗尽（如 3 次尝试均失败）正常收敛为 `DiagnosisRun = FAILED`，不写入 DLQ。

---

## 4. 幂等性与冲突处理语义

- 唯一约束：`UNIQUE(user_id, idempotency_key)`
- 服务端计算：`SHA256(normalized request body)`
- **相同 Key + 相同 Hash**：幂等返回已有 DiagnosisRun（HTTP 200/202）；
- **相同 Key + 不同 Hash**：返回 `409 Conflict`，明确指示客户端错误复用了唯一标识。
