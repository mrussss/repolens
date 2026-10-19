# Agent Runtime 与安全边界

Agent Runtime 使用自有的受控工具调用循环。模型输出的工具名、参数和结果都要经过服务端检查；仓库代码、注释和错误日志均视为不可信数据。

```text
DiagnosisExecutionSpec
  → Provider 请求
  → 检查工具名和参数
  → 检查预算及读取策略
  → 执行只读工具并记录 AgentStep
  → 继续调用或进入仅生成最终报告的收尾阶段
  → 校验结构与 Citation
```

## 可用工具

Registry 只注册五个只读工具：`search_code`、`get_symbol`、`find_references`、`find_related_tests` 和 `read_file`。Agent 不能调用 shell、执行仓库命令或自行访问网络。

`read_file` 只允许读取固定 Snapshot 中符合共享 Snapshot policy 的相对路径：拒绝路径越界、符号链接、非普通文件、排除目录和敏感文件类型。读取受单次字节预算限制；超出的结果按完整行界限截取。工具创建的 Evidence 会记录实际行号和内容摘要，供后续 Citation 校验。

## 调用预算与收尾

默认 Guard 限制每轮步数、工具调用次数、`search_code` 次数、重复调用次数、单个工具结果字节数和模型输出 token。耗尽探索预算后，Runtime 会取消剩余工具调用，保留一个不带工具定义的收尾请求，让模型基于已有证据输出结构化报告。若最终输出截断或无法解析，会记录失败结果，不将不完整文本当作有效 JSON 报告。

## Citation 校验

模型只能引用当前 Attempt 工具返回的 `evidence_id`。最终化时，服务端会检查该 Evidence 是否属于当前 Diagnosis/Attempt 和固定 lineage，并按已签发的源码路径、行号、内容哈希及 excerpt 校验报告。检查通过表示引用与所读 Snapshot 内容一致；它不证明模型推断的根因正确。
