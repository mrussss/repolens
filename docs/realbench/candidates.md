# RealBench v1 候选与冻结记录

> `realbench-v1` 是第一版 pilot external benchmark，由 3 个真实 Go 项目历史 Bug 组成，用于验证完整外部评测链路，不代表大规模真实世界泛化结论。

本轮冻结 3 个来自不同公开 Go 仓库的历史缺陷。每个案例都使用 issue 公开后的 buggy parent commit 作为输入，不把修复提交、修复 diff 或 Ground Truth 传入检索路径。

| Case | Repository | Buggy commit | Fix commit | Primary file | Evidence |
|---|---|---|---|---|---|
| REAL-001 | go-chi/chi | `a54874f0e2f12647a19e82ee70dfa8185014100c` | `2c567813d274099814f6912244ec670fea3b70a4` | `middleware/wrap_writer.go` | [issue #1067](https://github.com/go-chi/chi/issues/1067), [PR #1068](https://github.com/go-chi/chi/pull/1068) |
| REAL-002 | spf13/cobra | `ad460ea8f249db69c943a365fb84f3a59042d54e` | `f26981f3ad65293bc5465e722b7fbca3e0bb013e` | `completions.go` | [issue #2257](https://github.com/spf13/cobra/issues/2257), [PR #2258](https://github.com/spf13/cobra/pull/2258) |
| REAL-003 | hashicorp/go-retryablehttp | `571a88bc9c3b7c64575f0e9b0f646af1510f2c76` | `0aa17a73624a122ce7ba46ff0b4bbf47d4df3bda` | `client.go` | [issue #121](https://github.com/hashicorp/go-retryablehttp/issues/121), [PR #122](https://github.com/hashicorp/go-retryablehttp/pull/122) |

## 冻结检查

- 3 个仓库、3 类不同根因：重复计数、参数切片别名变异、HTTP body 重放信息丢失。
- buggy/fix 均为完整 40 位 SHA，并通过 GitHub merge parent 与 fix commit 关系核验。
- `primary_relevant_files` 只指向实际根因文件；测试文件仅作为 supporting evidence。
- 输入只包含 issue 标题、描述和错误复现信息；修复 SHA、根因、相关文件和 provenance 只存在于 `ground_truth.json`。
- runner 只允许固定 SHA checkout、源码解析、索引和只读检索，不执行第三方测试、构建脚本或生成器。

案例的检索结果以实际 runner 输出为准；在基线运行前不在 README 或本文件中预写 Hit@K/MRR 数字。
