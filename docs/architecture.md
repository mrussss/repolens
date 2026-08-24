# RepoLens 系统架构文档

## 1. 核心定位与架构分层

RepoLens (Reliable AI Repository Diagnosis Platform) 是一个以 Go 为主语言的高可靠 AI 代码仓库故障诊断后端系统。
系统严格区分三个事实层：

```text
业务真相 (Business Truth)      → MySQL 8 (InnoDB, 事务, 乐观并发控制)
代码真相 (Code Truth)          → Immutable RepositorySnapshot (本地只读共享存储)
派生数据 (Derived Truth)       → RepositoryIndex / AgentStep / EvalRun
```

---

## 2. 端到端执行主链路

```text
[Developer]
    ↓ POST /diagnoses (Idempotency-Key + Body)
[Go API (Gin)]
    ↓ 校验 User / Snapshot / Index + 计算 SHA256 Request Hash
[MySQL Transaction]
    ├─ Insert DiagnosisRun (Status=QUEUED, Version=1)
    └─ Insert OutboxEvent (Status=PENDING, Type=DIAGNOSIS_REQUESTED)
    ↓ 202 Accepted 返回给客户端
[Outbox Relay]
    ↓ 定期轮询 available_at <= now() 的事件并投递
[RabbitMQ Exchange (Direct + DLX)]
    ↓ Queue: repolens.diagnosis.task
[Diagnosis Worker]
    ↓ 条件更新 Claim Run (QUEUED/RETRY_WAIT → RUNNING)
    ↓ 插入 DiagnosisAttempt #N (RUNNING, Heartbeat Ticker 启动)
[Agent Runtime]
    ├─ search_code (Lexical / BM25 / Vector / Hybrid RRF)
    ├─ read_file (带 Path Traversal & Symlink & Secret Guards)
    ├─ read_docs (文档只读)
    └─ read_ci_log (错误日志过滤)
    ↓
[Structured Diagnosis Report (JSON)]
    ↓
[Citation Verification Engine]
    ↓ 针对不可变 Snapshot 验证 File Path、Line Range 与 Excerpt Content Hash
    ↓
[Persistence Transaction]
    ├─ Save Report & Validated Citations
    └─ Finish Attempt & Run (SUCCEEDED)
    ↓
[SSE Stream Replay & Broadcast] (Last-Event-ID 支持)
```

---

## 3. 核心设计原则

1. **AI 生命周期与 HTTP 生命周期彻底解耦**：所有长耗时分析均由 Worker 异步执行，API 返回 202 Accepted。
2. **Attempt 仅在 Worker Claim 后创建**：API 创建 Run 时 Attempt 计数为 0，保证 Attempt 精确代表真实发生的执行次数。
3. **只读代码智能平台**：不执行仓库内的任意代码、构建脚本或 Shell 命令，全面防范 SSRF 与路径穿越。
