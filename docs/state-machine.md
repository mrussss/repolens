# RepoLens 状态机设计与演进规范

## 1. RepositorySnapshot 状态机

```text
CREATED
   ↓
MATERIALIZING
   ↓
READY / MATERIALIZE_FAILED
```

- **语义**：该状态机仅表达特定 Commit SHA 的源码是否已安全落盘并可只读访问。
- **不可变性**：一旦进入 `READY`，源码文件、路径及 ContentHash 永不修改。

---

## 2. RepositoryIndex 状态机

```text
CREATED
   ↓
INDEX_QUEUED
   ↓
INDEXING
   ↓
READY / INDEX_FAILED
```

- **解耦优势**：同一 Snapshot 可派生多个不同 Retrieval Strategy（Lexical、BM25、Vector、Hybrid）的 Index 记录，便于在完全相同的代码事实上进行公平对比实验。

---

## 3. DiagnosisRun 业务状态机

```text
       ┌────────────── QUEUED ─────────────┐
       │                 ↓                 │
       │              RUNNING              │
       │          ↙      ↓      ↘          │
       │   RETRY_WAIT SUCCEEDED  FAILED    │
       │       ↓                           │
       │    QUEUED                         │
       │                                   │
       └──────→ CANCEL_REQUESTED ──────────┘
                         ↓
                     CANCELLED
```

- **乐观并发控制**：所有状态迁移必须携带 `WHERE id = ? AND version = ? AND status = ?`，并递增 `version = version + 1`，彻底杜绝 Lost Update。

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
