# 云端模型路由（原 litellm-router）

个人 LLM 集中路由：**自建 Go 透传网关**，把 **DeepSeek（主决策）** 和 **MiniMax（后台量大管饱）**
聚合成一个统一入口，供 Claude Code / Codex / OpenCode 等所有 agent 使用，
并自带透明计量（用量面板）、渠道/模型管理、virtual key 权限。

> 曾用 LiteLLM 做网关，因 2G 服务器装不动 + 想完全掌控路由/计量，改为自建 Go 网关（`gateway/`）。

## 架构（一句话）

```
你的机器（本地角色分配）
  Claude Code
    ANTHROPIC_MODEL = deepseek-v4-pro     → 主对话（整段，保持连贯）→ DeepSeek
    ANTHROPIC_DEFAULT_HAIKU_MODEL = MiniMax-M3 → 后台杂活 → MiniMax
  Codex / OpenCode（各自配 model + base_url）   ↓ 都指向
                                       ┌──────────────────────────┐
展示服务器 121.40.184.111（Caddy 入口）│  自建 Go 网关（gateway）  │
  /v1/* → gateway :8080               │  路由 + 计量 + key 权限   │
  /panel → 用量面板                   └──────────┬───────────────┘
                                                ↓ 计量数据
存储服务器 115.29.241.36（PostgreSQL，5 表：usage_logs/keys/channels/models/upstream_keys）
```

## 关键决策

- **自建 Go 网关，不用 LiteLLM**（替代了，LiteLLM 在 2G 装不动 + 想掌控路由/计量）
- **不用 one-api / new-api**（中转站=计费分发，非自用路由）
- **主对话整段留 DeepSeek**（不按上下文长度切——切了破坏连贯性）
- **后台杂活走 MiniMax**（利用其量大管饱，消化后台任务/子代理）
- **透明计量**：自建 usage_logs + 用量面板，每条请求记录 model/token/cost

详见 [docs/DECISIONS.md](docs/DECISIONS.md)。

## 快速开始

1. 部署网关 + 面板：见 [docs/setup.md](docs/setup.md)
2. 客户端接入（Claude Code / Codex / OpenCode）：见 [docs/clients.md](docs/clients.md)
3. 验证：`curl http://121.40.184.111/v1/models -H "Authorization: Bearer <你的 virtual key>"`
4. 看用量：`http://121.40.184.111/panel`

## 文档

- [DECISIONS.md](docs/DECISIONS.md) — 架构决策
- [architecture.md](docs/architecture.md) — 系统架构
- [setup.md](docs/setup.md) — 部署
- [clients.md](docs/clients.md) — 客户端接入
- [panel-design.md](docs/panel-design.md) — 用量面板设计
