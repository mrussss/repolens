# ADR 001：使用数据库任务队列替代 RabbitMQ 与 Outbox Relay

## 状态

已采纳并实现。

## 背景

RepoLens v1 使用 RabbitMQ 和 Outbox relay。项目定位是本地单用户仓库诊断工具，维护消息代理、Outbox 轮询、交换机和死信队列会增加部署与运维负担。

## 决策

1. 使用 `internal/jobs/` 中基于数据库的 Job runtime。
2. MySQL 8 使用 `SELECT ... FOR UPDATE SKIP LOCKED` 并发领取；SQLite 测试使用事务锁定。
3. 通过 execution generation、claim token、lease renewer 和 Reaper 管理执行权、续租与过期任务恢复。
4. 当前任务类型为 `MATERIALIZE_SNAPSHOT`、`BUILD_CODE_INDEX`、`BUILD_RETRIEVAL` 和 `RUN_DIAGNOSIS`。

## 结果

无需外部消息代理，部署组件更少。业务对象和排队任务在同一数据库事务中创建；执行状态仍需遵守 claim fencing 与 lease recovery 约束。
