# ADR 007：原子发布 Retrieval artifact

## 状态

已采纳并实现。

## 背景

若索引直接写入正在使用的目录，进程崩溃或任务取消可能留下部分文件，导致读取到不完整 artifact。

## 决策

1. 在 `.tmp/<retrieval_build_id>-<claim_token>/` 下写入临时文件。
2. 生成索引、元数据和 `manifest.json`，并对写入内容计算 SHA256。
3. 校验后通过目录原子 rename 发布到最终路径。
4. 仅在 claim 仍有效时，将 RetrievalBuild 在数据库事务中标记为 READY。
