# RepoLens Agent Runtime 与安全防护体系

## 1. 受控的 Tool Calling Loop

系统采用自研受控 Agent 循环，不盲目引入重型 LangChain / Eino 框架：

```text
[User Diagnosis Context]
   ↓
[LLM Provider]
   ↓
Tool Call Request?
   ├─ No  → Parse Structured JSON Report
   └─ Yes
        ↓
     Validate Tool Name (Registry)
        ↓
     Validate Arguments & Schema
        ↓
     Security Guards Check
        ↓
     Execute Tool (Bounded Timeout & Max Bytes)
        ↓
     Append Result to Context & Record AgentStep
        ↓
     Next Step (Guard: MaxSteps, RepeatCalls)
```

---

## 2. 核心只读工具与安全防护边界

1. `search_code`: 限制 Scope、TopK 及输入长度。
2. `read_file`:
   - **Path Traversal Guard**: 校验相对路径及 `..` 越界；
   - **Symlink Escape Guard**: 调用 `EvalSymlinks` 确认软链接目标未逃逸 Snapshot 根目录；
   - **Sensitive Filename Guard**: 严禁读取 `.env`, `.git/config`, `id_rsa`, `*.pem`, `*.key` 等敏感凭据；
   - **Binary Guard**: 拦截 `.bin`, `.exe`, `.so` 等二进制文件；
   - **Output Size Guard**: 限制单次返回最大字节数（默认 64KB），防止 Context 撑爆。
3. `read_docs`: 仅限读取 `README` 及 `docs/**` 文档。
4. `read_ci_log`: 支持行号切片及关键字过滤。

---

## 3. Citation 机器校验逻辑

大模型在输出中给出的 Citations 不得直接采信：
1. 校验 `file_path` 在指定 Snapshot 下是否存在；
2. 校验 `start_line` 与 `end_line` 范围合法性；
3. 从不可变 Snapshot 中提取实际源码行，计算 SHA256 Content Hash 并校验与 Excerpt 匹配度；
4. 校验通过标记为 `VALID`，否则标记为 `INVALID` 并记录具体错误原因。
