# litellm-router — Project Notes for Claude

个人 LLM 集中路由：LiteLLM（远端网关）+ Claude Code 自带角色分配（本地）。
把 DeepSeek（主决策）和 MiniMax（后台量大管饱）聚合成一个端点。

## 决策（先读这个）

**[docs/DECISIONS.md](docs/DECISIONS.md)** 是唯一权威的架构决策记录。改动架构先改它。

核心三条：

1. **用 LiteLLM**，不用 one-api/new-api（中转站=计费分发，非自用路由）
2. **主对话整段留 DeepSeek**，不按上下文长度切（破坏连贯）
3. **后台杂活走 MiniMax**（Claude Code `ANTHROPIC_SMALL_FAST_MODEL`），不用 claude-code-router

## 文档索引

- [docs/setup.md](docs/setup.md) — 服务器部署（2核4G + Docker Compose + SQLite）
- [docs/clients.md](docs/clients.md) — Claude Code / OpenCode / Codex 接入
- [config.yaml](config.yaml) — LiteLLM 模型路由表

## 已知待验证项（实现时确认）

- MiniMax 确切模型 id（`MiniMax-Text-01` 或 `MiniMax-M2`）+ 订阅端点 base URL
- Claude Code 是否接受非 `claude-*` 模型名（不接受就用 config.yaml 里的别名方案）
- LiteLLM 在 2核4G 的内存占用（SQLite 模式起步）

## 环境约束

- 服务器 2核4G，国内（腾讯/阿里云），Docker Compose
- 客户端在国内网络，DeepSeek/MiniMax 均为国内 API
- 不要裸暴露 4000 端口到公网（master key 风险），见 setup.md 安全节

## 入口

- 部署：`docs/setup.md`
- 验证：`curl http://<服务器>:4000/v1/models`
