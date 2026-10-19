# ADR 014：冻结 Provider 执行配置

## 状态

已采纳并实现。

## 背景

OpenAI-compatible endpoint 的 URL 规范化、模型身份和凭据配置变化可能导致同一 Diagnosis 执行漂移或失败。

## 决策

1. 规范化 Base URL，并在生产配置中要求 HTTPS。
2. Diagnosis 创建时冻结 Provider endpoint/model 身份及相关执行选项；Worker 执行时核对兼容性，避免无提示地切换 endpoint 或 model。
3. API Key 不写入 Diagnosis 快照或公开健康信息；相同 endpoint/model 下允许轮换本地密钥。
4. 对外暴露的 endpoint 指纹不包含 API Key。
