# litellm-router — 个人 LLM 集中路由

把 **DeepSeek（主决策）** 和 **MiniMax（后台量大管饱）** 两家 API 聚合成一个统一入口，
供 Claude Code / OpenCode / Codex 等所有 agent 使用。

## 架构（一句话）

```
你的机器（本地角色分配）
  Claude Code
    ANTHROPIC_DEFAULT_MODEL   = deepseek-chat   → 主对话（整段，保持连贯）→ DeepSeek
    ANTHROPIC_SMALL_FAST_MODEL = minimax-small  → 后台杂活（摘要/子任务） → MiniMax
  OpenCode / Codex（各自配 model name）                ↓ 都指向
                                           ┌──────────────┐
远端服务器（2核4G）                          │   LiteLLM    │
                                           │ DeepSeek key │
                                           │ MiniMax key  │
                                           └──────────────┘
```

## 关键决策

- **用 LiteLLM**，不用 one-api / new-api（那些是"中转站"——给别人卖 API 计费的，不是个人路由）
- **不用语义路由**（"看懂请求自动选模型"不可靠，2 家厂商纯属过度设计）
- **不用 claude-code-router**（只服务 Claude Code，且你要的是"主对话连贯"而非按长度切模型）
- **主对话整段留在 DeepSeek**（不按上下文长度切——切了会破坏连贯性）
- **后台杂活走 MiniMax**（利用其量大管饱，消化 Claude Code 后台任务/子代理）

详见 [docs/DECISIONS.md](docs/DECISIONS.md)。

## 快速开始

1. 服务器上部署 LiteLLM：见 [docs/setup.md](docs/setup.md)
2. 客户端接入（Claude Code / OpenCode）：见 [docs/clients.md](docs/clients.md)
3. 验证：`curl http://<服务器>:4000/v1/models`
