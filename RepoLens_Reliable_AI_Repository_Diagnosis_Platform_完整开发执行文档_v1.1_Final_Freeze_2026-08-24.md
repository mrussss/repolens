# RepoLens — Reliable AI Repository Diagnosis Platform
## Go Backend + AI Engineering 求职主项目完整开发执行文档（Final Freeze）

> **版本**：v1.1 Final Freeze  
> **日期**：2026-08-24  
> **主语言**：Go  
> **项目定位**：Go Backend Core + AI Engineering 双主线求职主项目  
> **目标岗位**：Go 后端 / AI Backend / Agent Backend / AI-Native Backend  
> **开发原则**：小业务、深工程；先问题后技术；先 baseline 后复杂方案；所有亮点必须形成“代码 → 失败场景 → 测试/指标 → 简历 → 面试追问”闭环。  
> **项目代号**：RepoLens  
> **正式名称**：Reliable AI Repository Diagnosis Platform

### v1.1 Final Freeze 收口内容

本版本不改变业务主线、不增加技术栈，只封死九个实现语义：

```text
1. RepositorySnapshot 与 RepositoryIndex 解耦
2. DiagnosisAttempt 只在 Worker 真正 Claim 后创建
3. Transport Redelivery 与 Application Retry 分离
4. Worker Crash 使用 stale attempt recovery 闭环
5. Snapshot Storage 明确为 V1 共享本地 Volume
6. Idempotency-Key 增加 request hash 与冲突语义
7. EvalRun 记录完整版本信息，保证结果可复现
8. Git Clone 增加 SSRF / Redirect Guard；日志进入 LLM 前做 Secret Redaction
9. Offline Eval Metric 与 Online Prometheus Metric 严格分离
```

这九项完成后，RepoLens 的设计层面停止扩张；后续只允许实现细节基于测试、Benchmark 或 Eval 调整。

---

# 0. 封版结论

RepoLens 只解决一个问题：

> **给定一个代码仓库和一段 Issue / CI Log / Error Log，如何由 Go 后端可靠地执行一次 AI 诊断任务，并最终输出带源码证据、可追踪、可验证、可评测的结构化诊断报告？**

最终完整主链：

```text
Developer
   ↓
Register Repository
   ↓
Create Immutable RepositorySnapshot
   ↓
Build RepositoryIndex
   ↓
Submit Issue / CI Log / Error Log
   ↓
POST /diagnoses
   ↓
Go API
   ↓
MySQL Transaction
   ├─ DiagnosisRun
   └─ OutboxEvent
   ↓
Outbox Relay
   ↓
RabbitMQ
   ↓
Diagnosis Worker
   ↓
Claim Run
   ↓
Create DiagnosisAttempt
   ↓
Agent Runtime
   ├─ search_code
   ├─ read_file
   ├─ read_docs
   └─ read_ci_log
   ↓
Evidence-backed Report
   ↓
Citation Validation against Snapshot
   ↓
Trace / SSE
   ↓
Offline Eval / Regression
```

三个事实层必须始终分清：

```text
业务真相        MySQL
代码真相        RepositorySnapshot
AI 派生数据     RepositoryIndex / AgentStep / EvalRun
```

项目最终证明两件事：

```text
① 我能用 Go 做一个完整、可靠的后端系统；
② 我能把 Retrieval / Agent / Citation / Eval 真正工程化。
```

如果把 Agent 换成 `MockExecutor`，系统仍然必须是一份能独立成立的 Go 后端项目。

如果忽略普通 CRUD，AI 部分也必须能独立讲清：

```text
Repository Snapshot / Indexing
→ Retrieval
→ Tool Calling Agent
→ Evidence / Citation
→ Eval
```

---

# 1. 项目设计哲学

## 1.1 先业务问题，后技术组件

以后所有设计变更必须先回答：

> **当前已经出现了哪个真实问题？不用新组件为什么解决不了？**

固定开发逻辑：

```text
业务问题
↓
最小实现
↓
主动制造失败
↓
观察证据
↓
引入复杂设计
↓
测试 / Benchmark / Eval
↓
决定保留或拒绝
```

禁止下面这种设计方式：

```text
JD 出现 Kafka
→ 项目加入 Kafka

别人用了 Milvus
→ 项目加入 Milvus

Agent 岗写了 Multi-Agent
→ 项目加入 Multi-Agent
```

技术栈不是能力清单。

**真正的项目能力是：为什么、边界、失败时怎样、怎么验证。**

---

## 1.2 一个业务主线，五个核心工程故事

整个项目最终只卖五件事：

1. **MySQL 事务一致性与 Transactional Outbox**
2. **RabbitMQ + Worker 的可靠异步执行**
3. **代码 Retrieval：Lexical → BM25 → Vector → RRF 的实验驱动升级**
4. **受控的 Tool Calling Agent Runtime**
5. **Citation + Eval：证明 AI 输出不是“看起来像对”**

所有新功能如果不能强化这五条中的至少一条，默认不做。

---

## 1.3 项目不是“技术百科”

项目目标不是：

```text
Redis
Kafka
RabbitMQ
Elasticsearch
Milvus
Kubernetes
gRPC
MCP
Eino
LangGraph
Multi-Agent
Memory
全部出现
```

而是：

```text
一个真实问题
+
3～5 个足够深的工程难点
+
真实失败场景
+
可复现实验
+
完整 Ownership
```

---

# 2. 明确范围

## 2.1 V1 必须完成

```text
User
Repository
RepositorySnapshot
RepositoryIndex
DiagnosisRun
DiagnosisAttempt
Outbox
RabbitMQ Worker
Retry / Idempotency
Cancellation
Graceful Shutdown
Snapshot Shared Storage

Minimal Tool Calling Agent
Code Retrieval Baseline
Trace
SSE
Citation
Eval
```

Retrieval 最终至少完成：

```text
Lexical Baseline
+
BM25
+
固定 Eval
```

Embedding / Vector / RRF 必须做实验，但**是否进入最终生产路径由 Eval 决定**。

---

## 2.2 V1 明确不做

```text
微服务拆分
Kafka
Kubernetes
gRPC
Service Mesh

Multi-Agent
Long-term Memory
Planner / Supervisor
Reflection Framework
复杂 Workflow Graph
复杂 MCP 生态
LangGraph 作为核心依赖
复杂 Eino Graph

自动修改代码
自动提交 PR
Shell Tool
执行 Repository Code
自动 Build / Test
Package Install
任意网络 Tool

企业级 Organization / RBAC
Billing
复杂前端
多模型智能路由
五种 Vector DB
GraphRAG
Reranker（除非 Eval 明确需要）
```

---

## 2.3 Redis 的最终决策

**V1 默认不使用 Redis。**

理由：

- MySQL 已经承担业务 Source of Truth；
- RabbitMQ 已承担异步任务投递；
- 当前项目没有天然热点缓存、Session 共享、限流状态或分布式锁场景必须依赖 Redis；
- 为“Go 后端简历常见 Redis”而加入，属于堆技术栈。

未来只有出现以下真实需求才重新评估：

```text
高频只读热点已证明 DB 成为瓶颈
或
分布式限流 / 短期状态真的需要
或
某一数据结构明显优于 MySQL
```

没有 Benchmark / 业务证据，不加。

---

## 2.4 Auth 的最终边界

V1 只实现最小 Auth，用于：

```text
识别 user_id
隔离 Repository / Diagnosis 数据
验证资源 Ownership
```

Auth **不是本项目亮点**，不扩展为：

```text
OAuth 平台
企业级 RBAC
Organization / Team 权限系统
复杂 Session 基础设施
```

时间优先投入到事务、Worker Reliability、Retrieval、Agent、Citation 与 Eval。

---

# 3. 最终技术栈

## 3.1 Core Backend

```text
Language          Go
HTTP              Gin
Database          MySQL 8
ORM               GORM（普通 CRUD）
Critical SQL      database/sql / 手写 SQL
Async             RabbitMQ
Testing           go test + Testcontainers-Go
Container         Docker Compose
Snapshot Storage  Docker shared local volume（V1）
Logging           slog / structured logging
Metrics           Prometheus（后期最小集合）
```

---

## 3.2 AI / Retrieval

```text
LLM Provider      OpenAI-compatible abstraction
Agent             自研最小 bounded Tool Calling Loop
Streaming         SSE
Output            Structured JSON

Retrieval V0      ripgrep / lexical baseline
Retrieval V1      code-aware chunk + BM25
Retrieval V2      embedding / dense retrieval（实验）
Fusion            RRF（Hybrid 实验有效时启用）
Search Engine     Elasticsearch 8（进入 BM25 / dense 阶段时引入）
Eval              固定故障 Dataset + 离线 Runner
```

### Elasticsearch 的定位

Elasticsearch **不是业务数据库**。

它只负责：

```text
Code Retrieval Index
```

MySQL 永远负责：

```text
业务真相
状态机
事务
任务
报告元数据
Outbox
```

Elasticsearch 是否成为最终主路径，不由“技术栈规划”决定，而由 Retrieval Eval 决定。

---

# 4. 用户故事与产品边界

典型场景：

```text
CI Test Failed
或
Production Error
或
模块行为异常
↓
选择 Repository
↓
提交 Issue / Error Log / CI Log
↓
创建 DiagnosisRun
↓
后台异步分析
↓
Agent 搜索代码 / 文档 / 日志
↓
返回：
- Root Cause
- Evidence
- Recommended Checks
- Confidence
- Citations
```

用户真正关心的是：

> **“我把故障交给你，能不能得到一份有代码证据、而且可追踪的诊断结果？”**

不是：

> “你的系统用了几个 Worker、几个框架。”

Worker、MQ、Retrieval 都必须服务于这个用户故事。

---

# 5. 核心领域模型

## 5.1 User

```text
User
- id
- email
- password_hash
- created_at
```

只做普通账户。

不做：

```text
Organization
Team
复杂 RBAC
OAuth Platform
```

---

## 5.2 Repository

```text
Repository
- id
- user_id
- name
- git_url
- default_ref
- status
- created_at
- updated_at
```

V1 只支持：

- public HTTPS Git repository；
- host allowlist；
- shallow clone；
- 仓库大小限制；
- 文件数量限制；
- 单文件大小限制。

---

## 5.3 RepositorySnapshot

`RepositorySnapshot` 代表一个**确定且不可变的代码事实**，不再承载检索索引状态。

```text
RepositorySnapshot
- id
- repository_id
- commit_sha
- ref
- materialized_path
- content_hash
- status
- created_at
- ready_at
```

一个 Diagnosis 必须绑定具体 Snapshot。

原因：

> Repository 会变化，Citation 与 Eval 必须能回到确定的代码版本。

Snapshot 一旦 `READY`：

```text
source content immutable
commit_sha immutable
materialized_path 不复用写入
```

---

## 5.4 RepositoryIndex

`RepositoryIndex` 是从 Snapshot 派生出的**检索产物**。

```text
RepositoryIndex
- id
- snapshot_id
- strategy
- index_version
- status
- chunk_count
- document_count
- embedding_model
- embedding_version
- created_at
- ready_at
- error_code
```

同一个 Snapshot 可以存在多个检索实验或版本：

```text
Snapshot A
├─ Lexical Index
├─ BM25 Index
├─ Vector Index
└─ Hybrid Retrieval Config
```

这样 Phase 5 才能在**完全相同的代码事实**上比较不同 Retrieval 方案，而不会把“代码版本变化”和“检索算法变化”混在一起。

---

## 5.5 DiagnosisRun

代表一次**用户业务请求**。

```text
DiagnosisRun
- id
- user_id
- repository_id
- snapshot_id
- issue_title
- issue_description
- error_log
- status
- cancel_requested
- idempotency_key
- idempotency_request_hash
- final_attempt_id
- version
- created_at
- updated_at
```

`DiagnosisRun` 描述的是：

> 用户想完成的那一次诊断。

它不等于某个 Worker 的执行实例。

---

## 5.6 DiagnosisAttempt

代表一次**真正发生过的执行尝试**。

```text
DiagnosisAttempt
- id
- diagnosis_run_id
- attempt_no
- worker_id
- status
- started_at
- heartbeat_at
- deadline_at
- finished_at
- error_code
- error_message
- retryable
- model
- prompt_tokens
- completion_tokens
- tool_calls
```

关键约束：

> **API 创建 DiagnosisRun 时不创建 Attempt。只有 Worker 真正 Claim 到可执行 Run 后，才创建 DiagnosisAttempt。**

因此：

```text
Run QUEUED
Attempt = 0

Worker Claim
↓
Attempt #1 RUNNING
```

同一个业务 Run 可以因为失败产生：

```text
Attempt #1 FAILED_RETRYABLE
Attempt #2 ABANDONED
Attempt #3 SUCCEEDED
```

`heartbeat_at / deadline_at` 用于识别 Worker Crash 后遗留的 stale attempt。

它自然承载：

- Retry；
- Execution History；
- Worker Crash Recovery；
- Failure Classification；
- 最终状态追踪。

---

## 5.7 AgentStep

```text
AgentStep
- id
- attempt_id
- seq
- step_type
- tool_name
- tool_args_summary
- status
- latency_ms
- input_tokens
- output_tokens
- error_code
- created_at
```

用于：

- Trace；
- SSE Replay；
- Debug；
- Eval；
- 面试时展示一条 Agent 如何执行。

---

## 5.8 Report

```text
Report
- id
- diagnosis_run_id
- attempt_id
- root_cause
- findings_json
- recommended_checks_json
- confidence
- created_at
```

---

## 5.9 Citation

```text
Citation
- id
- report_id
- snapshot_id
- file_path
- start_line
- end_line
- excerpt
- reason
- content_hash
- validation_status
```

Citation 不只是文本。

必须可以验证：

```text
path 存在
+
line range 合法
+
excerpt 与 immutable Snapshot 内容一致
```

---

## 5.10 OutboxEvent

```text
OutboxEvent
- id
- aggregate_type
- aggregate_id
- event_type
- payload
- status
- retry_count
- available_at
- created_at
- published_at
```

`available_at` 同时用于延迟发布 Application Retry Event，避免把业务退避重试绑在 RabbitMQ `nack/requeue` 上。

---

# 6. 状态机

## 6.1 RepositorySnapshot

```text
CREATED
  ↓
MATERIALIZING
  ↓
READY

失败：
MATERIALIZE_FAILED
```

Snapshot 状态只回答：

> **这一份确定 commit 的源码是否已经安全落盘并可只读访问？**

不再用 Snapshot 状态表达 BM25 / Vector 等索引是否完成。

---

## 6.2 RepositoryIndex

```text
CREATED
  ↓
INDEX_QUEUED
  ↓
INDEXING
  ↓
READY

失败：
INDEX_FAILED
```

不同 Retrieval Strategy 可以拥有不同 Index 记录与版本。

---

## 6.3 DiagnosisRun

```text
QUEUED
  ↓
RUNNING
  ↓
SUCCEEDED

可重试失败：
RUNNING
  ↓
RETRY_WAIT
  ↓
QUEUED

终止失败：
RUNNING → FAILED

取消：
QUEUED / RUNNING / RETRY_WAIT
  ↓
CANCEL_REQUESTED
  ↓
CANCELLED
```

可选内部阶段：

```text
RETRIEVING
ANALYZING
VERIFYING
```

但数据库主状态不要设计成几十个枚举。

所有状态迁移必须使用：

```text
expected old status
+
version / conditional update
→
new status
```

禁止“最后一个 UPDATE 赢”。

---

## 6.4 DiagnosisAttempt

Attempt 创建即意味着 Worker 已经开始一次真实执行：

```text
RUNNING
  ↓
SUCCEEDED
```

或：

```text
FAILED_RETRYABLE
FAILED_TERMINAL
CANCELLED
ABANDONED
```

`ABANDONED` 专门表示：

> Worker Crash / Lost Ownership 后，由 Recovery 逻辑确认该 Attempt 已不再可靠继续。

Attempt 与 Run 的状态必须在明确事务边界内协调更新。

---

# 7. 主业务链一：Repository Snapshot + Indexing

```text
POST /repositories/:id/index
        ↓
Validate Repository / Ref
        ↓
MySQL Transaction
        ├─ RepositorySnapshot = MATERIALIZING
        ├─ RepositoryIndex = INDEX_QUEUED
        └─ OutboxEvent(REPOSITORY_INDEX_REQUESTED)
        ↓
COMMIT
        ↓
Outbox Relay
        ↓
RabbitMQ
        ↓
Index Worker
        ↓
Safe Clone / Checkout
        ↓
Materialize Immutable Snapshot
/data/repositories/{repository_id}/{snapshot_id}/source
        ↓
Snapshot = READY
        ↓
File Filter
        ↓
Code-aware Chunk
        ↓
Build RepositoryIndex
        ↓
RepositoryIndex = READY
```

Snapshot 与 Index 必须分开：

```text
Snapshot = 代码事实
Index    = 检索派生物
```

Indexing 必须是异步任务。

原因：

- clone 可能耗时；
- 文件解析可能耗时；
- embedding 更耗时；
- HTTP 生命周期不应绑定整个索引生命周期。

Diagnosis 创建前至少要求：

```text
Snapshot READY
+
所选 Retrieval Strategy 对应 RepositoryIndex READY
```

---

# 8. Repository / Input 安全边界

项目定位是：

> **Read-only Code Intelligence**

不是代码执行平台。

## 8.1 Clone 限制

V1 优先采用严格 allowlist，而不是支持任意 Git Server。

必须限制：

```text
HTTPS only
Git host allowlist（默认 github.com；按需增加明确域名）
禁止 file://
禁止 SSH
禁止 localhost / loopback
禁止 RFC1918 private address
禁止 link-local / metadata address
禁止 DNS / HTTP redirect 最终落到 private address
shallow clone
clone timeout
最大 repository size
最大 file count
最大 single-file size
```

核心目标是避免 Git URL 变成 SSRF / 内网探测入口。

---

## 8.2 文件限制

跳过：

```text
binary
.git
node_modules
vendor（按策略）
大型 generated files
常见 build artifacts
secret-like files
```

禁止：

```text
symlink escape
path traversal
```

---

## 8.3 Issue / CI Log / Error Log 输入保护

这些输入可能最终进入外部 LLM，因此必须先做：

```text
max input bytes
normalization
secret pattern redaction
Authorization header redaction
common token / API key redaction
```

不追求企业级 DLP，只解决当前产品最自然的数据外泄风险。

---

## 8.4 Snapshot Storage

V1 明确使用 Docker shared local volume：

```text
/data/repositories/
└─ {repository_id}/
   └─ {snapshot_id}/
      └─ source/
```

约束：

```text
Snapshot READY 后只读
不同 snapshot_id 不复用可写目录
Tool / Citation Validator 读取同一份 immutable source
容器通过同一 volume mount 访问
```

V1 不引入 MinIO / S3。

理由：

> 当前部署目标是单机 Docker Compose；对象存储只有在 Worker 跨机器扩展或需要远端持久化时才成为真实需求。

需要扩容时，通过 `SnapshotStore` interface 替换本地实现，而不是现在预先增加中间件。

---

## 8.5 永不执行

V1 不执行：

```text
Repository Code
Build Script
Shell Script
Test
Package Install
Git Hook
```

这不仅是范围控制，也是安全设计。

---

# 9. Code-aware Chunk

V1 不做编译器级完整 AST Pipeline。

优先设计：

```text
File Filter
↓
Language Detect
↓
Function / Type / Fixed Window Chunk
↓
Metadata Preservation
```

每个 Chunk：

```text
Chunk
- snapshot_id
- path
- language
- symbol
- start_line
- end_line
- content
- content_hash
```

如果进入 dense retrieval：

```text
- embedding
- embedding_model
- embedding_version
```

设计目标：

- 能映射回原文件和行号；
- 支持 Citation；
- 单 Chunk 不过大；
- 尽量保持函数 / 类型语义完整；
- 同 Snapshot 重建可通过 hash 跳过不变内容；
- Embedding 模型升级能识别旧版本。

---

# 10. Retrieval：必须实验驱动

这是本项目最重要的 AI 设计之一。

但绝对禁止：

> “因为 RAG 项目都用 Vector DB，所以直接上 Vector Search。”

---

## 10.1 Stage A：Lexical Baseline

首先实现最简单的：

```text
ripgrep / lexical search
```

典型 Query：

- function name；
- error code；
- symbol；
- stack trace；
- filename；
- log keyword。

固定 Dataset 上记录：

```text
File Hit@5
File Hit@10
MRR
P95 Retrieval Latency
```

---

## 10.2 Stage B：Code-aware + BM25

如果 baseline 的表达、排序能力不足：

```text
Code-aware Chunk
+
BM25
```

重新运行相同 Dataset。

对比：

```text
Lexical vs BM25
```

---

## 10.3 Stage C：Embedding 实验

只有存在明显的**语义召回问题**才引入 Embedding。

典型问题：

```text
“哪里处理连接关闭后的旧异步结果？”
“哪个模块负责认证队列过载？”
“哪部分负责失败重试边界？”
```

这类 Query 不一定包含源码中的精确 symbol。

加入：

```text
Embedding
+
Dense kNN
```

重新评测。

---

## 10.4 Stage D：Hybrid + RRF

如果 BM25 与 Vector 命中不同类型的正确证据：

```text
BM25 Top-K
+
Vector Top-K
↓
RRF
↓
Final Top-K
```

RRF 在 Go 服务层实现。

不引入 learning-to-rank。

---

## 10.5 最终保留规则

必须输出真实实验：

| Retrieval | Hit@5 | Hit@10 | MRR | P95 | Index Cost |
|---|---:|---:|---:|---:|---:|
| Lexical | 实测 | 实测 | 实测 | 实测 | 实测 |
| BM25 | 实测 | 实测 | 实测 | 实测 | 实测 |
| Vector | 实测 | 实测 | 实测 | 实测 | 实测 |
| Hybrid-RRF | 实测 | 实测 | 实测 | 实测 | 实测 |

最终规则：

```text
如果 Hybrid 显著改善关键指标
→ 保留 Elasticsearch dense + RRF

如果 BM25 已足够
→ 最终生产路径保留 BM25
→ 在 ADR 中记录拒绝 Vector 的实验依据
```

**无论哪一种结果，都是好项目。**

因为重点是：

> 能通过实验决定架构，而不是通过流行度决定架构。

---

# 11. 主业务链二：Create Diagnosis

API：

```text
POST /diagnoses
Idempotency-Key: ...
```

服务端计算：

```text
idempotency_request_hash = hash(normalized request body)
```

推荐唯一约束：

```text
UNIQUE(user_id, idempotency_key)
```

行为必须固定：

```text
第一次：
Key=A + Body=X
→ Create Run #123

重复相同请求：
Key=A + Body=X
→ Return existing Run #123

错误复用 Key：
Key=A + Body=Y
→ 409 Idempotency Conflict
```

创建处理：

```text
Validate User / Repository / Snapshot / Index
↓
BEGIN
↓
Check Idempotency-Key + Request Hash
↓
Create DiagnosisRun(status=QUEUED)
↓
Create OutboxEvent(DIAGNOSIS_REQUESTED)
↓
COMMIT
↓
Return 202 Accepted
```

此时：

```text
DiagnosisRun exists
DiagnosisAttempt count = 0
```

只有 Worker 真正 Claim 到该 Run 后才创建 Attempt。

不要：

```text
POST /diagnoses
↓
直接调用 LLM
↓
HTTP 等 40 秒
```

AI 生命周期必须和 HTTP 生命周期解耦。

---

# 12. Transactional Outbox

## 12.1 为什么需要

错误方案：

```text
BEGIN
Create DiagnosisRun
COMMIT
↓
Publish RabbitMQ
```

如果：

```text
DB COMMIT 成功
但 RabbitMQ 暂时不可用
```

会得到：

```text
数据库说任务存在
但任务永远没有被执行
```

---

## 12.2 正确链路

```text
BEGIN
↓
Create DiagnosisRun
Create OutboxEvent
↓
COMMIT
```

注意：

> **这里不创建 DiagnosisAttempt。Attempt 属于 Worker 真实执行生命周期，不属于 API 接纳生命周期。**

独立 Relay：

```text
SELECT pending outbox
WHERE available_at <= now()
↓
Publish RabbitMQ
↓
Mark published
```

---

## 12.3 必须理解

Outbox 不提供 Exactly Once。

可能发生：

```text
publish 成功
↓
Relay 在 mark published 前 crash
↓
重启
↓
再次 publish
```

所以：

> **Publisher 允许重复，Consumer 必须幂等。**

`available_at` 还承担 Application Retry 的延迟调度，但它只是轻量延迟事件，不把 RepoLens 做成通用 Scheduler。

---

# 13. RabbitMQ 与 Worker Reliability

RabbitMQ 负责：

> **任务通知与投递。**

MySQL 负责：

> **任务状态真相。**

必须掌握：

```text
at-least-once
ack / nack
redelivery
duplicate delivery
DLQ
consumer crash
prefetch
graceful shutdown
```

这里故意不把所有失败都叫 Retry。

---

## 13.1 Consumer 幂等

消费消息时至少检查：

```text
Event ID / Message ID
+
DiagnosisRun 当前状态
+
Conditional State Transition
```

不能简单相信：

```text
“MQ 不会重复。”
```

---

## 13.2 Worker Claim 与 Attempt 创建

Worker 收到可执行消息后：

```text
BEGIN
↓
Conditional Claim DiagnosisRun
QUEUED / RETRY_WAIT → RUNNING
↓
Create DiagnosisAttempt(status=RUNNING)
↓
COMMIT
```

Claim 失败说明：

```text
已被其他 Worker 执行
或
任务已经完成 / 取消
或
当前不是合法执行状态
```

此时不要再次产生副作用。

---

## 13.3 ACK 原则

只有在当前消息对应的业务决策已经可靠持久化后：

```text
ACK
```

例如 Worker：

```text
执行完成
↓
Report / Citation / Run / Attempt 状态写入事务成功
↓
ACK
```

如果：

```text
DB 写成功
↓
ACK 前 crash
```

RabbitMQ 可能 Redeliver。

第二次消费必须通过业务状态识别“结果已完成”，然后安全 ACK，而不是重新生成副作用。

---

## 13.4 Worker Crash Recovery

核心故障：

```text
Worker A
↓
Run = RUNNING
Attempt #1 = RUNNING
↓
Worker A crash
```

如果只看 `Run = RUNNING`，任务可能永久卡死。

V1 使用轻量 stale-attempt recovery：

```text
DiagnosisAttempt
- worker_id
- heartbeat_at
- deadline_at
```

Recovery Sweeper 定期寻找：

```text
Attempt.status = RUNNING
AND heartbeat_at / deadline_at 已超时
```

确认 stale 后：

```text
BEGIN
Attempt #1 → ABANDONED
Run → RETRY_WAIT
Create OutboxEvent(DIAGNOSIS_RETRY_REQUESTED, available_at=...)
COMMIT
```

随后正常走：

```text
Outbox Relay
↓
RabbitMQ
↓
Worker B
↓
Claim
↓
Attempt #2
```

这个机制只解决 Worker Crash Recovery，不演化成通用分布式 Lease / Scheduler 平台。

---

## 13.5 Transport Redelivery ≠ Application Retry

必须明确区分：

```text
RabbitMQ Redelivery
= Transport Recovery
```

典型场景：

```text
消息已投递
Worker 在 ACK 前 crash
RabbitMQ 再次投递同一消息
```

而：

```text
LLM 429 / 5xx
= Application Retry
```

Application Retry 的推荐路径：

```text
Attempt #1 FAILED_RETRYABLE
↓
Run → RETRY_WAIT
↓
Create Retry OutboxEvent(available_at=future)
↓
ACK 当前 RabbitMQ Message
↓
到期后 Relay 发布新消息
↓
Attempt #2
```

不要依赖：

```text
NACK + immediate requeue
```

做业务退避，否则容易形成 hot loop。

---

# 14. Retry / Failure Semantics

Application Retry 必须分类。

## 14.1 Retryable Application Failure

```text
LLM 429
LLM 5xx
temporary network failure
temporary repository IO
temporary Elasticsearch error
```

注意：

> `RabbitMQ redelivery` 不属于 Application Retry，它属于 Transport Redelivery。

---

## 14.2 Terminal Application Failure

```text
invalid repository
repository too large
path denied
bad request
tool permission denied
invalid snapshot
unsupported input
malformed user configuration
```

---

## 14.3 Application Retry Policy

例如：

```text
max_attempts = 3

Attempt #1 FAILED_RETRYABLE
↓
Run = RETRY_WAIT
↓
Retry OutboxEvent(available_at=t1)
↓
Attempt #2
↓
...
```

达到 `max_attempts`：

```text
Attempt #3 FAILED_RETRYABLE
↓
Run = FAILED
```

**不要把正常业务重试耗尽叫作 DLQ。**

DLQ 主要用于消息基础设施无法正常处理的情况，例如：

```text
malformed message
unknown event type
schema / deserialization failure
poison message
```

具体 Backoff 数字之后由实现与测试确定，不需要为了“高级”设计复杂调度器。

---

## 14.4 每个错误必须回答

```text
谁发现？
谁分类？
是 Transport Redelivery 还是 Application Retry？
状态怎么更新？
是否 ACK？
是否产生 Retry OutboxEvent？
用户最终看到什么？
Trace 留什么？
```

这才叫 failure semantics。

---

# 15. Cancellation

API：

```text
POST /diagnoses/:id/cancel
```

先持久化：

```text
cancel_requested = true
```

Worker 必须在：

- Agent step 之间；
- Tool Call 前；
- Tool Call 后；
- LLM request 前后；
- 长 retrieval 阶段；

检查：

```text
context
+
DB cancellation flag
```

同进程：

```text
context.CancelFunc
```

可以快速触发。

跨进程：

> **Cancellation 是 cooperative cancellation，不承诺瞬时 kill。**

必须能解释：

- 为什么不能随便 kill goroutine；
- 为什么 Tool / Provider API 要接收 context；
- cancel 后已有 DB 副作用如何处理。

---

# 16. Graceful Shutdown

Worker 收到 SIGTERM：

```text
停止接收新任务
↓
等待 in-flight task
↓
在 shutdown deadline 内完成
↓
ACK / persist state
↓
关闭 RabbitMQ / DB / HTTP
```

如果 deadline 到：

```text
cancel context
↓
当前 Attempt 进入确定状态
↓
退出
```

必须主动测试：

```text
执行中 SIGTERM
```

而不是只测试空闲退出。

---

# 17. Minimal Tool Calling Agent

V1 不依赖 Eino / LangChain / LangGraph 作为核心。

首先自己实现最小循环：

```text
User Diagnosis Context
↓
LLM
↓
Tool Call?
 ├─ No → Structured Final
 └─ Yes
      ↓
   Validate Tool Name
      ↓
   Validate Args
      ↓
   Permission Guard
      ↓
   Execute Tool
      ↓
   Tool Result
      ↓
   Append Context
      ↓
   Next Step
```

---

## 17.1 Guards

必须有：

```text
max_steps
max_tool_calls
deadline
context cancellation
max_tool_output_bytes
repeat-call detection
structured output validation
token / cost accounting
```

可选：

```text
max_same_tool_calls
max_same_args_hash
```

---

## 17.2 Provider Abstraction

```go
type LLMProvider interface {
    Generate(ctx context.Context, req GenerateRequest) (GenerateResponse, error)
}
```

支持：

```text
RealProvider
FakeProvider
```

CI 默认使用 FakeProvider。

---

# 18. Tools

V1 只允许四类只读 Tool：

```text
search_code
read_file
read_docs
read_ci_log
```

---

## 18.1 search_code

输入：

```text
query
top_k
scope
```

输出：

```text
path
symbol
start_line
end_line
snippet
score
retrieval_source
```

---

## 18.2 read_file

输入：

```text
path
start_line
end_line
```

必须有：

```text
path traversal guard
symlink guard
secret guard
binary guard
max bytes
timeout
```

---

## 18.3 read_docs

只读取 Snapshot 中允许的：

```text
README
docs/**
设计文档
配置说明
```

---

## 18.4 read_ci_log

不要一次性把全部日志塞给模型。

支持：

```text
keyword search
line range
error block
max bytes
```

---

# 19. Tool Security

模型只能：

> **提出 Tool 请求。**

服务端决定：

> **工具是否存在、参数是否合法、有没有权限、能返回多少。**

至少实现：

```text
Path Traversal Protection
Symlink Escape Protection
Secret Filename Guard
Binary Guard
Max Output Bytes
Timeout
Context Cancellation
```

典型拒绝：

```text
../../etc/passwd
.git/config
.env
id_rsa
binary executable
huge generated file
```

---

# 20. Structured Diagnosis Report

最终输出固定 JSON Schema：

```json
{
  "summary": "...",
  "root_cause": "...",
  "findings": [
    {
      "title": "...",
      "reasoning": "...",
      "citations": [
        {
          "path": "internal/worker/worker.go",
          "start_line": 120,
          "end_line": 145
        }
      ]
    }
  ],
  "recommended_checks": [],
  "confidence": 0.0
}
```

不要让模型输出一个无法机器处理的长 Markdown 文本作为唯一结果。

UI 可以把 JSON 渲染成 Markdown。

---

# 21. Citation Verification

LLM 提交 Citation 后，后端必须重新验证：

```text
Snapshot
↓
path 是否存在
↓
line range 是否合法
↓
读取实际内容
↓
excerpt / content_hash 对齐
↓
标记 VALID / INVALID
```

模型不能自行宣布：

> “这个引用是正确的。”

Citation 是系统验证后的证据对象。

---

# 22. Trace + SSE

SSE 只用于：

> **展示后台任务的进度 / Trace。**

不承载任务本身。

事件例子：

```text
diagnosis.queued
attempt.started
retrieval.started
retrieval.completed
agent.step.started
tool.called
tool.completed
citation.validated
report.completed
diagnosis.failed
```

---

## 22.1 AgentStep 持久化

顺序：

```text
AgentStep persist
↓
publish SSE event
```

因此 SSE 断开不影响任务。

---

## 22.2 SSE Replay

客户端断线：

```text
SSE disconnect
↓
Diagnosis continues
```

重新连接：

```text
Last-Event-ID
↓
DB query AgentStep > last_id
↓
Replay
↓
继续实时 stream
```

必须做这个测试。

这比“用了 SSE”本身更有价值。

---

# 23. Eval Dataset 与可复现性

Eval 是项目必须完成的正式模块，不是最后为了 README 临时补一个脚本。

建议固定：

```text
30～50 个 fault cases
```

## 23.1 EvalCase

每个 Case：

```text
EvalCase
- case_id
- dataset_version
- repository
- snapshot_sha
- issue
- error_log
- expected_root_cause
- relevant_files
- relevant_line_ranges
- forbidden_claims
```

数据集不要追求大。

重点是：

- 可复现；
- 有 Ground Truth；
- 能做 Regression；
- 能记录失败案例。

---

## 23.2 EvalRun

每次离线评测必须记录“是哪一版系统跑出来的”：

```text
EvalRun
- id
- dataset_version
- git_commit
- snapshot_sha / snapshot_set_version
- retrieval_strategy
- retrieval_version
- index_version
- prompt_version
- agent_version
- model
- embedding_model
- started_at
- finished_at
```

因此当指标变化时，可以回答：

```text
是 Dataset 变了？
Prompt 变了？
Agent Loop 变了？
Model 变了？
Embedding 变了？
Retrieval / Index 变了？
还是代码本身变了？
```

这使 Eval 从“展示数字”变成真正的 Regression 工具。

---

# 24. Eval Metrics

## 24.1 Retrieval

```text
File Hit@5
File Hit@10
MRR
Recall@K（可选）
P95 Retrieval Latency
```

---

## 24.2 Citation

```text
Citation Validity Rate
Relevant Citation Rate
Correct Span Rate
Unsupported Citation Rate
```

---

## 24.3 Diagnosis

```text
Root Cause Success Rate
Required Evidence Coverage
Forbidden Claim Rate
```

Root Cause 可以先采用：

```text
规则 + 人工标注
```

不强求构建学术级自动评分系统。

---

## 24.4 System

```text
P50 / P95 Diagnosis Latency
Average Tool Calls
Tool Failure Rate
Retry Rate
Token Usage
Estimated Cost
Cancellation Success
```

---

# 25. Benchmark 原则

任何写到简历里的数字必须回答：

```text
测试环境是什么？
数据集是什么？
Case 数量多少？
Baseline 是什么？
并发多少？
P50 / P95 怎么算？
跑几次？
模型是什么？
Prompt 是否固定？
为什么提升？
```

禁止提前写：

```text
准确率提升 37%
P95 降低 50%
支持 10W QPS
```

没有真实数据就不写。

---

# 26. Testing Strategy

## 26.1 Unit Test

重点：

```text
state transition
idempotency
retry classifier
RRF
tool validation
path guard
report schema
citation validator
```

---

## 26.2 Integration Test

Testcontainers：

```text
MySQL
RabbitMQ
Elasticsearch（Phase 5 Retrieval Experiment 后）
```

覆盖：

```text
Create Diagnosis → Outbox（API 不创建 Attempt）
Outbox → RabbitMQ
Idempotency-Key same body → same Run
Idempotency-Key different body → 409
duplicate message
worker claim race
worker crash → stale attempt recovery
ACK loss after DB success
transport redelivery
application retry via delayed OutboxEvent
malformed message → DLQ
conditional update
snapshot / index separation
index / search
SSE replay
```

---

## 26.3 Fake LLM Provider

CI 不依赖真实付费模型。

FakeProvider 必须能模拟：

```text
normal answer
tool call
tool timeout
provider timeout
429
5xx
malformed JSON
invalid tool args
repeated tool call
empty answer
```

---

## 26.4 E2E

```text
register repository
↓
create snapshot
↓
index
↓
wait READY
↓
create diagnosis
↓
worker execute
↓
agent tools
↓
report
↓
verify citation
↓
eval
```

---

## 26.5 Failure Test

每个 Phase 必须主动制造失败。

### Async

```text
DB commit 后 RabbitMQ 临时不可用
duplicate message
ACK 前 crash
consumer crash
retry exhausted
SIGTERM with in-flight job
```

### Agent

```text
tool timeout
invalid tool args
path denied
repeated tool call
provider 429
provider timeout
malformed structured output
```

### Retrieval

```text
wrong symbol
semantic query
large file
deleted file
stale snapshot
```

---

# 27. Observability

不搭大平台。

## 27.1 Structured Logging

统一字段：

```text
request_id
user_id
repository_id
snapshot_id
diagnosis_id
attempt_id
message_id
tool_name
model
latency_ms
error_class
```

---

## 27.2 Minimal Prometheus Metrics

后期加入：

```text
http_requests_total

diagnosis_total
diagnosis_failed_total
diagnosis_latency_seconds

worker_inflight
worker_queue_delay_seconds

mq_redelivery_total
application_retry_total
stale_attempt_recovered_total

retrieval_requests_total
retrieval_latency_seconds
retrieval_candidates_total
retrieval_errors_total

llm_latency_seconds
tool_calls_total
tool_failures_total
token_usage_total
```

`Hit@5 / Hit@10 / MRR / Root Cause Success Rate` 属于有 Ground Truth 的 **Offline Eval**，不作为线上 Prometheus 指标。

Prometheus 不是项目主亮点。

它的价值是：

> 故障出现后有证据可查。

---

# 28. 推荐项目结构

```text
cmd/
├─ api/
│  └─ main.go
├─ worker/
│  └─ main.go
├─ relay/
│  └─ main.go
└─ eval/
   └─ main.go

internal/
├─ auth/
├─ user/
├─ repo/
│  ├─ handler.go
│  ├─ service.go
│  ├─ model.go
│  └─ store.go
├─ snapshot/
├─ repoindex/
├─ diagnosis/
│  ├─ handler.go
│  ├─ service.go
│  ├─ state.go
│  ├─ attempt.go
│  └─ store.go
├─ outbox/
├─ mq/
├─ worker/
│  ├─ consumer.go
│  ├─ claim.go
│  ├─ heartbeat.go
│  └─ recovery.go
├─ indexing/
│  ├─ clone.go
│  ├─ filter.go
│  ├─ chunk.go
│  └─ indexer.go
├─ retrieval/
│  ├─ interface.go
│  ├─ lexical.go
│  ├─ bm25.go
│  ├─ vector.go
│  └─ rrf.go
├─ llm/
│  ├─ provider.go
│  ├─ openai_compatible.go
│  └─ fake.go
├─ agent/
│  ├─ runtime.go
│  ├─ loop.go
│  ├─ registry.go
│  └─ guard.go
├─ tools/
│  ├─ search_code.go
│  ├─ read_file.go
│  ├─ read_docs.go
│  └─ read_ci_log.go
├─ evidence/
│  ├─ report.go
│  └─ citation.go
├─ trace/
├─ sse/
├─ eval/
│  ├─ case.go
│  ├─ run.go
│  ├─ runner.go
│  └─ metrics.go
└─ platform/
   ├─ mysql/
   ├─ rabbitmq/
   ├─ elasticsearch/
   ├─ snapshotstore/
   ├─ logger/
   ├─ metrics/
   ├─ config/
   └─ shutdown/

migrations/

testdata/
├─ repositories/
├─ ci_logs/
└─ eval_cases/

deploy/
└─ docker-compose.yml

docs/
├─ architecture.md
├─ state-machine.md
├─ failure-semantics.md
├─ retrieval-eval.md
├─ agent-runtime.md
└─ adr/
```

原则：

```text
HTTP
↓
Application / Domain Service
↓
Interfaces
↓
Infrastructure
```

避免：

- Handler 直接写 SQL；
- Agent 直接访问 MySQL；
- RabbitMQ callback 堆全部业务逻辑；
- package 循环依赖；
- 为了“Clean Architecture”制造几十层空抽象。

---

# 29. 开发 Phase 总览

```text
Phase 0  Go Backend Skeleton
Phase 1  Reliable Async Runtime
Phase 2  Repository Snapshot + Indexing Baseline
Phase 3  Minimal Agent Runtime
Phase 4  Trace + SSE + Citation
Phase 5  Retrieval Experiment + Eval
Phase 6  Hardening + Product Polish
Phase 7  Resume / Interview Freeze
```

每一 Phase 都有：

```text
Implementation Gate
Failure Gate
Ownership Gate
```

**代码写完不等于 Phase 完成。**

---

# 30. Phase 0 — Go Backend Skeleton

## 目标

先完全不做 AI。

建立一个正常的 Go Backend。

---

## 实现

```text
Go module
config
Gin
structured logging

User
Auth
Repository
RepositorySnapshot
RepositoryIndex（schema 可先有，Phase 2 才真正构建）
DiagnosisRun

MySQL
migration
GORM
critical SQL
pagination
validation
error handling
context

Docker Compose
Testcontainers
CI
```

Diagnosis 暂时返回：

```text
Mock Report
```

---

## 必须做深

MySQL：

```text
schema
primary key
unique constraint
composite index
transaction
isolation
MVCC
row lock
deadlock
EXPLAIN
pagination
```

---

## Failure Gate

主动测试：

```text
重复 Idempotency-Key + 相同 Request Hash
相同 Idempotency-Key + 不同 Request Hash → 409
并发创建同一业务请求
非法状态迁移
DB transaction rollback
unique conflict
context timeout
```

---

## Ownership Gate

必须闭卷回答：

```text
一个 HTTP 请求怎么走？
事务边界在哪里？
哪些字段建索引？为什么？
GORM 和手写 SQL 怎么分工？
错误如何包装和返回？
context 如何传播？
并发状态更新如何防止 lost update？
```

---

## Phase 0 结束条件

```text
go test ./...
Docker Compose 可启动
数据库 migration 可重建
主要 API E2E PASS
```

并且：

> 不谈 AI，这已经是一份普通 Go 后端骨架。

---

# 31. Phase 1 — Reliable Async Runtime

## 目标

把长耗时 Diagnosis 从 HTTP 生命周期中解耦，并把“消息重复、Worker Crash、业务 Retry”三类故障语义分清。

---

## 实现

```text
OutboxEvent
Outbox Relay
RabbitMQ
Diagnosis Worker

Worker Claim
DiagnosisAttempt
Attempt heartbeat / deadline
Stale Attempt Recovery

At-Least-Once
Idempotency
Transport Redelivery
Application Retry
DLQ
Cancellation
Graceful Shutdown
```

Agent 继续使用 FakeExecutor。

---

## 关键链

创建任务：

```text
POST /diagnoses
↓
MySQL Transaction
↓
DiagnosisRun(QUEUED)
+
OutboxEvent
↓
COMMIT
↓
Relay
↓
RabbitMQ
```

真正执行：

```text
Worker
↓
Conditional Claim Run
↓
Create Attempt #1
↓
FakeExecutor
↓
Persist Report / Attempt / Run
↓
ACK
```

业务重试：

```text
Retryable Failure
↓
Attempt FAILED_RETRYABLE
↓
Run RETRY_WAIT
↓
Retry OutboxEvent(available_at)
↓
ACK current message
```

Crash Recovery：

```text
Worker Crash
↓
stale RUNNING Attempt
↓
Recovery Sweeper
↓
Attempt ABANDONED
Run RETRY_WAIT
↓
Retry OutboxEvent
```

---

## Failure Gate

必须故意测试：

```text
DB COMMIT 后 MQ unavailable
Outbox duplicate publish
duplicate message
ACK lost simulation
worker crash after claim
worker crash after DB success before ACK
stale heartbeat recovery
client retry
application retry exhausted
malformed message → DLQ
cancel
SIGTERM with in-flight task
```

---

## Ownership Gate

闭卷回答：

```text
为什么 DiagnosisRun 和 Outbox 同事务？
为什么 API 创建时不创建 Attempt？
Worker 如何 Claim？
为什么不能 Exactly Once？
ACK 丢失后发生什么？
Outbox 为什么可能重复发布？
Consumer 如何幂等？
Transport Redelivery 和 Application Retry 有什么区别？
为什么 LLM 429 不直接 nack + requeue？
Worker Crash 后 RUNNING 任务如何恢复？
Attempt 为什么不能和 Run 合并？
哪些错误进入 DLQ，哪些只是 Run FAILED？
Worker shutdown 时 in-flight 怎么办？
```

---

## Phase 1 结束条件

到这里：

> **已经是一份完整的 Go Backend 主项目，可以独立投普通 Go 后端。**

而且不能只“跑通 happy path”，必须用 Failure Gate 证明状态可以恢复。

---

# 32. Phase 2 — Repository Snapshot + Retrieval Baseline

## 目标

让系统第一次真正“理解一个确定版本的代码仓库”，同时把**代码事实**与**检索派生物**分开。

---

## 实现

```text
Safe Clone + SSRF Guard
RepositorySnapshot
Shared SnapshotStore（local volume）
RepositoryIndex
File Filter
Language Detect
Code-aware Chunk
Lexical Search
```

先不要 Embedding。

---

## 主链

```text
Register Repo
↓
Create Snapshot(MATERIALIZING)
+
Create RepositoryIndex(INDEX_QUEUED)
+
Outbox
↓
Async Index Worker
↓
Safe Clone
↓
Materialize immutable source to shared volume
↓
Snapshot READY
↓
Filter
↓
Chunk
↓
Build Lexical Index
↓
RepositoryIndex READY
```

---

## Failure Gate

测试：

```text
invalid Git URL
host denied
localhost / private IP denied
redirect to private IP denied
clone timeout
repo too large
binary
symlink escape
secret file
large generated file
stale / deleted snapshot path
Issue / CI Log secret redaction
```

---

## Ownership Gate

回答：

```text
为什么 Diagnosis 必须绑定 Snapshot？
Snapshot 和 RepositoryIndex 为什么必须拆开？
为什么不能直接读取当前 repo？
V1 为什么用 shared local volume，不上 MinIO？
未来跨机器 Worker 时怎么演进 SnapshotStore？
Chunk 为什么这样切？
为什么 V0 不做完整 AST？
Citation 如何映射回 path + line？
Git URL 为什么存在 SSRF 风险？
```

---

# 33. Phase 3 — Minimal Agent Runtime

## 目标

自己真正理解 Tool Calling Agent。

---

## 实现

```text
LLM Provider abstraction
FakeProvider

Agent Loop
Tool Registry
Tool Guard

search_code
read_file
read_docs
read_ci_log

Structured Diagnosis Report
```

---

## Runtime Gate

必须有：

```text
max_steps
deadline
context cancel
tool timeout
tool output limit
repeat call guard
schema validation
token accounting
```

---

## Failure Gate

测试：

```text
invalid tool name
invalid args
tool timeout
path denied
repeat same call
provider 429
provider timeout
malformed JSON
empty output
```

---

## Ownership Gate

闭卷画：

```text
LLM
→ Tool Call
→ Validate
→ Guard
→ Execute
→ Tool Result
→ LLM
→ Final
```

并回答：

```text
为什么不先用 Eino？
Tool permission 由谁决定？
Agent 如何避免无限循环？
Tool failure 如何反馈模型？
context 如何穿过整个 Agent？
```

---

# 34. Phase 4 — Trace + SSE + Citation

## 目标

从“Agent 能跑”升级到：

> **Agent 可观察、结果可验证。**

---

## 实现

```text
AgentStep persistence
Trace query
SSE
Last-Event-ID replay
Citation model
Citation validator
```

---

## Failure Gate

必须测试：

```text
SSE disconnect
任务继续
重新连接
Replay

invalid citation path
invalid line
mismatched excerpt
stale snapshot citation
```

---

## Ownership Gate

回答：

```text
为什么 SSE 断开不能取消 Diagnosis？
为什么先 persist Step 再 stream？
SSE 和 WebSocket 为什么选 SSE？
Citation 为什么必须由后端验证？
```

---

# 35. Phase 5 — Retrieval Experiment + Eval

## 目标

把 AI 项目从：

> “感觉好用”

升级为：

> **有固定数据集、Baseline 和 Regression。**

---

## Step 1：建立 Dataset

先至少完成：

```text
30 个可复现 fault cases
```

以后可扩到 50。

同时实现 `EvalRun` 版本记录：

```text
dataset_version
git_commit
retrieval_strategy / version
index_version
prompt_version
agent_version
model
embedding_model
```

任何对比实验必须来自同一 Dataset / Snapshot 基线。

---

## Step 2：跑 Lexical Baseline

记录：

```text
Hit@5
Hit@10
MRR
P95
```

---

## Step 3：引入 Elasticsearch BM25

必须回答：

> Lexical 的哪个问题让 BM25 有价值？

然后跑同一数据集。

---

## Step 4：Embedding Experiment

只有语义 Query 的召回存在明确不足时加入。

---

## Step 5：Hybrid RRF

如果 BM25 / Vector 互补：

```text
RRF
```

---

## Step 6：Architecture Decision

输出 ADR：

```text
adr/00x-retrieval-strategy.md
```

内容：

```text
Problem
Baseline
Experiment
Metrics
Tradeoff
Final Decision
```

---

## Failure / Eval Gate

必须保留：

```text
成功 Case
失败 Case
误召回 Case
Citation 错误 Case
模型误判 Case
```

不要只展示最好看的结果。

---

## Ownership Gate

回答：

```text
BM25 解决什么？
Embedding 解决什么？
为什么 RRF？
Top-K 怎么选？
为什么不用 Milvus？
为什么 ES 同时承担 lexical + dense？
如果 Vector 没提升，怎么办？
```

---

# 36. Phase 6 — Hardening + Product Polish

只做能增强可信度的工程工作。

## 实现

```text
Prometheus minimal online metrics
Stale Attempt Recovery Demo
SSRF / Secret Redaction Demo
Failure Demo
One-command Eval
Docker Compose
README
Architecture Diagram
State Machine Diagram
Failure Semantics Doc
ADR
minimal Web UI / CLI
```

---

## 最小 UI

只需要展示：

```text
Repository
Snapshot
Issue / CI Log
Run Status
Agent Timeline
Report
Citation
Eval
```

不要做：

> 聊天框首页 + 大量动画。

这个项目不是 ChatGPT Clone。

---

# 37. Phase 7 — Resume / Interview Freeze

最终只能把**真实完成并验证**的能力写到简历。

## 推荐五条简历故事

### 1. Transaction / Outbox

围绕：

```text
MySQL
Transaction
Schema
Index
Outbox
```

---

### 2. Async Reliability

围绕：

```text
RabbitMQ
Worker Claim
DiagnosisAttempt
At-Least-Once / Redelivery
Application Retry
Stale Attempt Recovery
Idempotency
Cancellation
```

---

### 3. Failure Engineering

围绕：

```text
duplicate delivery
worker crash / stale attempt
ack loss
retry exhausted
malformed message / DLQ
timeout
graceful shutdown
Testcontainers
```

---

### 4. Retrieval / Agent

围绕最终真实保留的：

```text
BM25
或
Hybrid Retrieval

+
Tool Calling
+
Guardrail
```

---

### 5. Citation / Eval

```text
Dataset
Citation Validation
Hit@K
Root Cause
Latency
Token Cost
Regression
```

---

# 38. Ownership Gate — 全项目统一毕业标准

每一个模块只有满足下面条件才算“自己会”。

```text
能闭卷画架构
能闭卷追一条调用链
能解释关键设计
能解释不用其他方案的原因
能自己改一个需求
能主动制造一个故障
能自己定位
能自己修复
能解释最终行为
```

例如 RabbitMQ 阶段：

```text
kill worker → stale attempt recovery
duplicate message
ack loss
LLM 429 → delayed application retry
malformed message → DLQ
retry exhausted → Run FAILED
```

例如 Agent：

```text
tool timeout
invalid args
repeat call
provider 429
provider timeout
```

例如 Retrieval：

```text
能拿固定 Dataset
真实比较 baseline
解释为什么最终保留 / 拒绝 Vector
```

---

# 39. Go Backend 毕业检查表

## Language

```text
interface
pointer
error wrapping
defer
context
goroutine
channel
select
mutex
errgroup
race condition
goroutine leak
```

## HTTP

```text
middleware
validation
timeout
graceful shutdown
idempotency key
202 async semantics
SSE
```

## MySQL

```text
B+Tree
index
composite index
transaction
MVCC
isolation
row lock
gap lock
deadlock
EXPLAIN
unique constraint
conditional update
```

## MQ

```text
at-least-once
ack / nack
transport redelivery
application retry
DLQ boundary
duplicate delivery
outbox
worker claim
stale attempt recovery
consumer crash
prefetch
```

## Reliability

```text
state machine
Run vs Attempt
retry boundary
transport vs application failure
idempotency
stale ownership recovery
cancellation
shutdown
failure semantics
```

---

# 40. AI Engineering 毕业检查表

## Retrieval

```text
为什么不能整个 Repo 塞 Context
Chunk 为什么这样切
Lexical 解决什么
BM25 解决什么
Embedding 解决什么
RRF 为什么需要
Top-K 怎么选
如何做 Retrieval Eval
```

## Agent

```text
Tool Calling Loop
Tool Registry
Tool Ownership
Tool Guard
Max Steps
Context Budget
Timeout
Cancellation
Tool Failure
Structured Output
```

## Safety

```text
Path Traversal
Symlink Escape
Secret Guard
Binary Guard
Output Size
为什么第一版不开放 Shell
```

## Eval

```text
Ground Truth 如何构造
Citation 如何验证
Root Cause 如何评
Baseline 是什么
如何做 Regression
Cost / Latency 怎么测
```

---

# 41. 项目最终能力矩阵

| 能力 | 目标深度 |
|---|---:|
| Go | ★★★★★ |
| HTTP / API | ★★★★★ |
| MySQL / SQL | ★★★★★ |
| Transaction / Lock / Index | ★★★★★ |
| RabbitMQ | ★★★★☆ |
| Worker / Async | ★★★★★ |
| Retry / Idempotency | ★★★★★ |
| Context / Cancellation | ★★★★★ |
| Testing | ★★★★★ |
| Failure Semantics | ★★★★★ |
| Repository Safety | ★★★★☆ |
| Retrieval | ★★★★☆ |
| Agent | ★★★★★ |
| Tool Safety | ★★★★★ |
| SSE / Trace | ★★★★☆ |
| Citation | ★★★★★ |
| Eval | ★★★★★ |
| Observability | ★★★☆☆ |
| Redis | V1 不做 |
| Kafka | 不做 |
| Kubernetes | 不做 |
| Multi-Agent | 不做 |
| Memory | 不做 |
| MCP | V1 不做 |

---

# 42. 与优秀求职项目的对标原则

RepoLens 不追求比成熟项目“组件更多”。

它要吸收的是不同优秀项目背后的意识。

## 42.1 向 SimpleBank 学

学习：

```text
小业务
+
数据库 / 事务 / 测试做深
```

要求：

> AI 再亮眼，也不能让 RepoLens 的 SQL、事务、测试比一个优秀普通 Go 项目更浅。

---

## 42.2 向 Agent Service Toolkit 学

学习：

```text
Agent
不是一个函数

Agent Service
=
API
Persistence
Streaming
Failure Handling
Observability
Testing
Deploy
```

不复制 LangGraph 技术栈。

---

## 42.3 向 Fin-Agent 学

学习：

```text
明确业务
Hybrid Retrieval
Citation
Eval
Metrics
Failure Cases
```

不复制金融业务，也不照搬 Planner / Multi-Agent。

---

## 42.4 向成熟求职项目学

最终所有亮点必须形成：

```text
真实问题
↓
代码设计
↓
失败场景
↓
测试 / Benchmark / Eval
↓
简历描述
↓
面试追问
```

---

# 43. 最终 Anti-Scope Checklist

以后有人建议加入新技术时，先问：

```text
1. 当前有什么明确问题？
2. 已有方案哪里失败？
3. 有测试或指标证明吗？
4. 新组件解决哪个失败？
5. 是否会引入新的复杂度？
6. 一个人能吃透吗？
7. 能形成面试故事吗？
```

如果前 4 条答不上：

> **不加。**

---

# 44. 最终封版路线

```text
Phase 0
普通 Go Backend
↓
Phase 1
可靠异步 Runtime
↓
Phase 2
Repository Snapshot / Index + Lexical Baseline
↓
Phase 3
Minimal Tool Calling Agent
↓
Phase 4
Trace + SSE + Citation
↓
Phase 5
BM25 / Vector / Hybrid + Eval Experiment
↓
Phase 6
Failure Engineering + Observability + Polish
↓
Phase 7
Resume / Interview Freeze
```

最终项目身份：

# **RepoLens — Reliable AI Repository Diagnosis Platform**

一句话：

> **一个以 Go 为主语言的可靠 AI 代码仓库故障诊断后端：将长耗时 AI Diagnosis 建模为可重试、可取消、可追踪的异步任务，通过受控 Tool Calling Agent 检索代码并生成源码 Citation，使用固定故障数据集对 Retrieval、诊断质量、延迟与 Token 成本进行回归评测。**

项目永远维持：

```text
Reliable Go Backend
+
Evidence-based AI Diagnosis
```

而不是：

```text
Scheduler
+
RAG
+
Agent
+
一堆中间件
```

---

# 45. 最终执行纪律

从此版本开始：

```text
主业务不换
Go + AI 双主线不换
MySQL / RabbitMQ / Agent / Eval 核心不换
不增加第三条产品主线
不追逐新框架
不为了 JD 临时塞技术
不为了“看起来高级”扩大范围
```

允许变化的只有：

```text
某个内部实现
某个 SQL
某个状态迁移方案
某个 Retrieval 算法
某个 Retry 策略
某个 Eval 指标
```

所有变化都必须有：

```text
问题
+
证据
+
取舍
```

这就是 RepoLens v1.1 Final Freeze 的最终开发边界。
