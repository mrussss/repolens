# 2026-10-04 六项审计修复与验收

执行依据：根目录 `项目开发与修复书.md`。起点为最新 `main`：
`9ea5cb374877aaaf896c82ca47227a0641c5c065`，远端为 `mrussss/repolens`。
开始时唯一未提交内容是 `docs/audits/` 中的原审计报告；本轮保留该内容，
不将它加入提交。

## 修复与定向证据

| 项目 | 根因和实现 | 回归证据 | 结果 |
| --- | --- | --- | --- |
| F1 Snapshot symlink isolation | root go.mod 绕过安全读取；将 canonical 路径检查共享到 snapshotpolicy.SafePath，模块发现同时受 manifest 与 allowlist 约束。nested go.mod 只从合法普通文件发现；源码读取经过同一安全检查，build.MatchFile 使用已验证的内容。root go.mod 最多 1 MiB，拒绝特殊文件。 | module_isolation_test.go 覆盖普通模块、内外 symlink、父目录及 source root symlink、FIFO、超大模块、manifest/allowlist 与 nested 模块；runtime_stdlib_test.go 证明 Analyzer 在模块发现失败时不产生类型分析结果；现有 snapshot/manifest/Evidence 集成继续通过。 | PASS |
| F2 Report persistence capacity | validator 允许的 JSON 超过 TEXT。新增 migration 018，将 findings/checks/limitations JSON 改为 MEDIUMTEXT；保留已有完整报告、raw output、parsed report/draft 的 MEDIUMTEXT，以及有限标量的 TEXT。 | 真 MySQL 测试升级旧 schema、恢复部分 DDL、保留旧数据；约 72KB findings、超过 64KB 的 checks/limitations、4 MiB 完整报告都能完成 checkpoint 和原子终结；INSERT 故障 trigger 证明超限在写入前由应用拒绝。 | PASS |
| F3 Docker stdlib CodeIntel | 最终 Alpine 缺少 GOROOT 分析资源。复制 builder 的 Go toolchain 与 stdlib，排除 module/build cache；运行环境禁用网络依赖下载；offline importer 拒绝非 canonical import path。 | verify_codeintel_runtime.sh 将同一编译器构建的测试放入实际 repolens-worker:latest，关闭容器网络；fmt.Sprintf 类型分析成功，builder 已缓存的 github.com/google/uuid 仍无法解析。 | PASS |
| F4 Retrieval metric correctness | 文件命中只参与 Recall。两个策略共用 relevance helper，符号或期望文件命中均有效，最早真实 rank 决定 Hit@K/MRR；文件集合去重。 | relevance_test.go 覆盖 rank 1、rank 3、未命中、多文件/重复文件、仅符号、两类 ground truth 并存、乱序 observation；BM25/Structural 同一契约。 | PASS |
| F5 Running trace visibility | 前端未传 attempt_id，RUNNING 没有 final attempt。前端按当前 generation 选择 RUNNING attempt，终态选择 FinalAttemptID，retry 清除旧 trace 并切换；取消状态读取最新 attempt；轮询不重叠。 | DiagnosisView 测试覆盖 RUNNING/QUEUED、历史与当前 attempt、SUCCEEDED/FAILED、retry、取消与 visibility 并发；trace_attempt_test.go 验证真实 steps API 及跨 run 拒绝。 | PASS |
| F6 UI pagination | API client 丢弃页码和 total，UI 没有翻页。仓库和诊断历史保留 metadata 并提供上一页/下一页；仓库页停止旧 poller 并丢弃过期请求，保持当前页刷新；仓库选择器逐页读取完整列表。 | 25 条记录，第一页 20 条、第二页 5 条；两种 UI 均可访问。测试覆盖旧列表响应、旧 revision 响应、当前页 PREPARING→READY、回翻、停止旧页轮询及 API metadata。 | PASS |

## 容量契约与兼容性

完整报告序列化后（包括 JSON escaping）最多 4 MiB；所有报告 JSON、RawOutput、
ParsedReportJSON、ParsedReportDraftJSON 每字段也最多 4 MiB。
MEDIUMTEXT 容量为 16 MiB 减一字节，超过应用上限时返回稳定的
`REPORT_TOO_LARGE` / `ErrReportTooLarge`，Worker 永久失败，避免自动重复 Provider
或把业务尺寸错误包装为数据库基础设施故障。引用终结后的完整报告重新序列化并
检查上限。summary/root_cause/parse_error 的 TEXT 持久化边界是 65,535 bytes；
结构 validator 对正文的既有更小限制保持有效。无需改变已有 raw/checkpoint 列类型。

Migration 018 使用现有 forward/resumable migration 机制，明确承认三个列的旧
TEXT 定义；每个 ALTER 可在部分 DDL 已提交后安全恢复，不修改旧迁移。
parser 版本提升至 v2.2.2，analyzer 至 v2.2.1，新 pipeline fingerprint 不会复用
旧分析产物；保留历史数据。Web list API 的前端返回值变为 Page<T>，所有调用者
已适配，HTTP 后端列表契约保持一致。

## 自审

- Snapshot：相关 module/source consumer 都遵守 canonical 路径、manifest 和
  allowlist；没有发现剩余的同类模块元数据旁路。普通源码、检索和 Evidence
  consumer 使用共享 policy 或 snapshotstore。
- Report：应用、序列化与正式 MySQL schema 的容量边界一致；最终写入和
  checkpoint 写入都有 byte 检查。
- Docker：验证对象是最终 worker 镜像，实际执行 Analyzer，网络关闭；第三方
  依赖仍严格 offline。
- Retrieval：ExpectedFile rank 1 得到 Hit@1=1、Hit@5=1、MRR=1；rank 3 的 MRR=1/3。
- Trace：FinalAttemptID 为空的 RUNNING run 能查看当前 attempt 已产生的步骤；
  不接受其它 run 的 attempt。
- Pagination：两种列表超过 20 条时均有下一页访问路径，旧页轮询和异步结果不
  改写当前页。

## 验证命令

| 实际命令 | 结果 |
| --- | --- |
| `go test ./internal/... -count=1` | PASS |
| `go test -race ./cmd/... ./internal/... -count=1` | PASS |
| `go test ./tests/integration ./tests/e2e -count=1` | PASS |
| `REPOLENS_REQUIRE_REAL_INTEGRATION=1 go test -race ./tests/integration ./tests/e2e -count=1` | PASS，E2E 82.919s |
| `REPOLENS_REQUIRE_REAL_INTEGRATION=1 go test -race ./tests/integration_real -count=1` | PASS，569.623s |
| `REPOLENS_REQUIRE_REAL_INTEGRATION=1 go test ./tests/integration_real -run 'ReportCapacity\|LargeValidReport' -count=1` | PASS，95.968s，包含超过 TEXT 的 checks/limitations |
| `go test -race ./internal/worker -run OversizedReport -count=1` | PASS，包含引用终结后超限 |
| `go test -race ./internal/codeintel/... ./internal/platform/snapshotstore ./internal/snapshotpolicy ./internal/evidence ./tests/integration -count=1` | PASS，最终 source root symlink 收尾检查 |
| `cd web && npm test -- --run` | PASS，6 个文件、35 项测试 |
| `cd web && npm run build` | PASS，TypeScript 与 Vite production build |
| `go vet ./...` | PASS |
| `go run ./cmd/eval` | PASS，32/32 ground truth 文件验证；FakeProvider 结果不作为真实 Provider 质量证据 |
| `docker compose config --quiet` | PASS |
| `docker compose build` | PASS，最终 API/Worker 镜像 |
| `./scripts/verify_codeintel_runtime.sh` | PASS，实际 worker 镜像、network none、stdlib 成功、外部依赖拒绝 |
| `gofmt -l cmd/ internal/ tests/` | PASS，无输出 |
| `git diff --check` | PASS |

完整 `release_gate.sh` 的 Compose 产品 smoke 阶段未在本轮运行；上表列出实际
执行的命令。本轮未对已有产品服务容器执行部署或停止操作。
详细输出保存在本地 `/tmp/repolens-fixes-*.log`，不加入版本控制。

Git 提交与远端 SHA 的最终核对结果见本次交付回复。
