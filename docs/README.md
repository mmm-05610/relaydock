# 云端模型路由（原 litellm-router）

个人 LLM 集中路由：**自建 Go 透传网关**，把 DeepSeek（主决策）和 MiniMax（后台量大管饱）
聚合到一个统一端点，供 Claude Code / Codex / OpenCode 使用，自带透明计量与用量面板。

> 曾用 LiteLLM，因 2G 服务器装不动 + 想完全掌控路由/计量，改为自建 Go 网关（`gateway/`）。

## 架构一句话

```
Claude Code / Codex / OpenCode
  主对话 → deepseek-v4-pro → DeepSeek
  haiku/后台 → MiniMax-M3  → MiniMax
            ↓ 都指向
展示服务器 121.40.184.111（Caddy 入口）
  /v1/* → 自建 Go 网关（gateway，:8080，systemd）
  /panel → 用量面板
            ↓ 计量
存储服务器 115.29.241.36（PostgreSQL，5 表）
```

## 文档导航

- [DECISIONS.md](DECISIONS.md) — 架构决策（自建 Go 网关 vs LiteLLM 的取舍）
- [architecture.md](architecture.md) — 系统架构（网关 / 计量 / key / 双服务器）
- [software-architecture.md](software-architecture.md) — Go 单二进制内部的软件架构与核心不变量
- [design-upstream-account-pool.md](design-upstream-account-pool.md) — 多账号池 MVP 设计
- [research-gateway-competitors.md](research-gateway-competitors.md) — 开源网关和账号池实现调研
- [setup.md](setup.md) — 部署（展示 + 存储双服务器）
- [clients.md](clients.md) — 客户端接入（Claude Code / Codex / OpenCode）
- [panel-design.md](panel-design.md) — 用量面板设计
- [experiment-cost-routing.md](experiment-cost-routing.md) — 成本路由实验记录

## 入口

- 网关验证：`curl http://121.40.184.111/v1/models -H "Authorization: Bearer <virtual key>"`
- 用量面板：`http://121.40.184.111/panel`
