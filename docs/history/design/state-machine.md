# RepoLens 状态机设计与演进规范

## 1. RepositorySnapshot 状态机

```text
CREATED
   ↓
MATERIALIZING
   ↓
READY / FAILED
```

- **语义**：该状态机仅表达特定 Commit SHA 的源码是否已安全落盘并可只读访问。
- **不可变性**：一旦进入 `READY`，源码文件、路径及 ContentHash 永不修改。

---

## 2. CodeIndexBuild / RetrievalBuild 状态机

```text
CREATED → BUILDING → READY
                    ↘ FAILED
```

- **解耦优势**：同一 Snapshot 可派生版本化 CodeIndexBuild 与 RetrievalBuild，Diagnosis 固定使用创建时的 build lineage。

---

## 3. DiagnosisRun 业务状态机

```text
       ┌────────────── QUEUED ─────────────┐
       │                 ↓                 │
       │              RUNNING              │
       │          ↙      ↓      ↘          │
       │   SUCCEEDED  FAILED  CANCELLED    │
       │                                   │
       └───────────────┘
```

- **状态分离**：`RETRY_WAIT` 只属于 `AnalysisJob`；取消请求由 `cancel_requested` flag 表达，Diagnosis 业务状态不增加中间状态。

---

## 4. DiagnosisAttempt 执行状态机

```text
RUNNING
   ├─ SUCCEEDED
   ├─ FAILED_RETRYABLE
   ├─ FAILED_TERMINAL
   ├─ CANCELLED
   └─ ABANDONED (Worker Crash / Heartbeat Expired)
```

- **ABANDONED 语义**：专用于标识 Worker 崩溃或丢失 Ownership 后，由 Stale Attempt Recovery Sweeper 确认并收口的旧尝试记录。
