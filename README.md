# RepoLens v2.1

RepoLens 是一个本地单用户 Go 代码诊断工具：它把固定版本的 Go 仓库物化为不可变 Snapshot，用 AST 与离线 best-effort `go/types` 建立版本化 CodeIndex，再用纯 Go BM25 + Structural Retrieval 为受控 Agent 提供证据，最后校验源码 Citation。

核心链路：

```text
Web → Go API → MySQL → DB-backed Analysis Jobs → Worker

Repository → immutable Snapshot → CodeIndexBuild → RetrievalBuild
           → 5 read-only tools → bounded Agent → validated citations
```

## 快速开始

需要 Docker、Docker Compose、Go 1.22+（仅本地开发）和 Node.js 20+（仅本地 Web 开发）。

```bash
cp .env.example .env
docker compose up --build
```

打开 <http://127.0.0.1:8080>。Setup 页面可以保存 OpenAI-compatible Provider；API Key 只写入 API/Worker 共享的 0600 本地 secret 文件，不会返回浏览器、trace 或日志。没有 API Key 时可使用 Try Demo 的确定性 FakeProvider。

Compose 最终只运行 `mysql`、`api`、`worker`，默认仅绑定 loopback。仓库内容是不可信数据；系统不会执行用户仓库的 build、test、generate 或 package install。

## 产品流程

1. 注册公开 Git 仓库并选择 ref。
2. 创建 Snapshot；Worker 解析 exact commit，完成文件 materialization 和 manifest hash 后才置为 READY。
3. 创建 CodeIndexBuild，查看 Symbols、References、Related Tests 和 AnalysisQuality。
4. 创建 RetrievalBuild。
5. 选择固定 Snapshot / CodeIndexBuild / RetrievalBuild，提交 CI/Test failure。
6. 轮询 Diagnosis、Report、Evidence 和 Agent Trace。

Diagnosis 会冻结 Snapshot、两个 build、Provider endpoint/model、prompt/agent 版本和配置 hash。之后重新构建索引不会改变既有 Diagnosis；相同 endpoint 的 API Key rotation 可以继续使用，endpoint 或 model drift 会被拒绝。

## API（节选）

主 API 前缀是 `/api/v1`：

```text
GET/PUT/DELETE /settings/provider
POST      /settings/provider/test
POST      /demo/trigger
POST      /repositories
POST      /repositories/:id/index
POST      /snapshots/:id/code-index-builds
POST      /code-index-builds/:id/retrieval-builds
POST      /diagnoses
GET       /diagnoses/:id/report
GET       /diagnoses/:id/steps
```

Diagnosis 的业务状态只有 `QUEUED`、`RUNNING`、`SUCCEEDED`、`FAILED`、`CANCELLED`；执行层的 retry 只存在于 AnalysisJob 的 `RETRY_WAIT`。

## 验证

```bash
gofmt -l cmd internal tests
go vet ./...
go test ./...
go test -race ./...
cd web && npm ci && npm run build
cd .. && go run ./cmd/eval
docker compose config
```

若本机有 Docker，可执行：

```bash
REPOLENS_REQUIRE_REAL_INTEGRATION=1 go test ./tests/integration_real/...
docker compose build
```

完整 release gate：`./scripts/release_gate.sh`。

## 范围与限制

- RepoLens v2.1 是 local single-user developer tool，不适合直接暴露到公网。
- 主要支持一个公开 GitHub 仓库和一个 root Go module；外部依赖可能保持 unresolved。
- 类型分析是离线、best-effort；Reference/Call 不是完整 runtime call graph。
- Related Test 是证据排序，不是完整 test impact analysis。
- BM25 是 portfolio-scale 的纯 Go 检索，不是分布式代码搜索。
- Agent 只读、受步数/调用次数/输出大小限制；仓库文本与 CI log 视为不可信输入。
- Citation validation 证明源码一致性，不证明模型结论的逻辑正确性；secret redaction 是 best-effort。
- Eval 数据集小且经过整理，Dev 与 frozen held-out 分离；Structural Retrieval 只有通过 promotion rule 才能成为生产策略。
- v1.1 的 RabbitMQ、Outbox、Elasticsearch、Vector/RRF、SSE 和旧 Auth 仅存在于历史版本，不属于 v2.1 core。
- v2.1 不实现自动 Snapshot/Index retention 或 GC；`make clean-data` 是破坏性的全量本地重置，请谨慎使用。

## 项目结构

```text
cmd/{api,worker,eval}       可执行程序
internal/jobs               DB-backed claim/lease/retry worker
internal/indexing           Snapshot materialization
internal/codeintel          AST、symbols、relations、quality
internal/retrieval          BM25、artifact、structural retrieval
internal/agent + tools      bounded Agent 与 5 个只读工具
internal/evidence           report、citation validation
migrations                  MySQL authoritative schema
web                         React + TypeScript + Vite
testdata                    parser fixtures、demo、eval cases
```
