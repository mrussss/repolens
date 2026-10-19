# 测试与验证

## 验证层级

`.github/workflows/ci.yml` 是每次 push 和 pull request 运行的 Fast CI。它包含：

- Web 依赖安装、测试和构建；
- Go 格式检查、`go vet`、核心测试及 race 测试；
- 组件集成测试和离线 eval；
- `docker compose config --quiet` 配置检查。

Fast CI **不运行**真实 MySQL 集成、真实 MySQL Golden Path、Compose 镜像构建或产品 Smoke。Fast CI 通过只说明上述步骤通过。

完整本地验收入口是 [`scripts/release_gate.sh`](../scripts/release_gate.sh)：

```bash
./scripts/release_gate.sh
```

Heavy Release Gate 在 Fast CI 的覆盖之外，还运行必需模式真实 MySQL 集成、真实 MySQL Golden Path、Compose 镜像构建、Worker 镜像内的离线 CodeIntel 边界检查，以及 Compose 产品 Smoke。真实 MySQL 集成要求零跳过，并设置 30 分钟测试时限以容纳完整的隔离数据库套件。脚本退出时会执行 `docker compose down`，但不会删除 named volumes；运行前要确认默认容器名、端口和 Compose 项目数据不会与正在使用的本地环境冲突。

`./scripts/verify_codeintel_runtime.sh` 可单独验证实际 Worker 镜像：它在禁网环境中运行 `TestRuntimeStdlibAndOfflineBoundary`，检查标准库类型分析和第三方依赖隔离。Release Gate 在镜像构建后会调用它。

## 开发时的针对性验证

根据改动范围运行相应检查；这些命令不能代替完整 Release Gate：

```bash
GOFLAGS=-mod=readonly go test ./cmd/... ./internal/...
GOFLAGS=-mod=readonly go test -race ./cmd/... ./internal/...
GOFLAGS=-mod=readonly go test -race ./tests/integration/...
(cd web && npm test -- --run && npm run build)
go run ./cmd/eval
```

真实 MySQL 集成和 Golden Path 需要 Docker：

```bash
REPOLENS_REQUIRE_REAL_INTEGRATION=1 GOFLAGS=-mod=readonly go test -race ./tests/integration_real/...
REPOLENS_REQUIRE_REAL_INTEGRATION=1 GOFLAGS=-mod=readonly go test -race ./tests/e2e/...
```

## RealBench

RealBench 命令、数据集和 production-consistent paired retrieval 用法见[评测说明](realbench/README.md)。冻结结果和逐 case 证据保留在 `docs/history/benchmarks/realbench/`。RealBench 的历史 Bug 检索指标不等同于真实 Provider 的根因诊断准确率。
