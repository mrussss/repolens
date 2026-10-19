# 本地部署

RepoLens 面向本地单用户使用。Docker Compose 会启动 MySQL 8、API 和 Worker：

```bash
cp .env.example .env
docker compose up --build
```

浏览器访问 <http://127.0.0.1:8080>。API 和 MySQL 默认只绑定 loopback；API 同时提供 Web 界面。Worker 从 MySQL 领取后台任务，并与 API 共享 Snapshot 和 Provider 密钥目录。

MySQL 数据、Snapshot 和 Provider 密钥保存在 named volumes 中。`docker compose down` 会停止并移除容器，保留这些卷。`make clean-data` 会破坏性地清理本地数据，执行前请确认确实需要重置。

在 Setup 页面配置 OpenAI-compatible Provider。API Key 保存在 API/Worker 共享的本地密钥文件中，文件权限为 `0600`，不会返回给浏览器；密钥脱敏仍是 best-effort。Try Demo 使用预置示例数据创建报告和 Trace，不会调用 Provider。真实诊断需要配置 Provider 后提交 Diagnosis。

仓库内容按不可信输入处理。系统不执行目标仓库的 build、test、generate 或 package install。该部署面向本地单用户环境，不应直接暴露到公网。

生产镜像包含编译 Worker 所需的 Go 工具链和标准库源码，不复制构建器的 module 或 build cache。离线 importer 仅接受规范的标准库路径或 Snapshot 根 module 内的包；第三方依赖保持 unresolved。镜像构建后可验证实际 Worker runtime：

```bash
./scripts/verify_codeintel_runtime.sh
```

生产检索在进程内运行纯 Go BM25，新构建固定 `RetrievalBuild.Strategy=BM25`。Structural 检索尚未通过当前 promotion 条件，只能通过显式 `BM25_STRUCTURAL` 或 `BM25_STRUCTURAL_V2` 构建进行实验；默认部署不依赖外部搜索集群或 embedding 服务。详见[检索与评测说明](retrieval-eval.md)。
