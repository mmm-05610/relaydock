# 云端模型路由（原 litellm-router）— Project Notes for Claude

个人 LLM 集中路由：**自建 Go 网关**（替代 LiteLLM）+ 客户端角色分配（本地）。
把 DeepSeek（主决策）和 MiniMax（后台量大管饱）聚合成一个统一端点，
并做透明计量（用量面板）与渠道/key 管理。

## 决策（先读这个）

**[docs/DECISIONS.md](docs/DECISIONS.md)** 是架构决策记录。改动架构先改它。

核心决策（当前落地）：

1. **自建 Go 透传网关，不用 LiteLLM**（曾用 LiteLLM，因 2G 服务器装不动 + 想完全掌控路由/计量，改为自建 `gateway/`）
2. **不用 one-api/new-api**（中转站=计费分发，非自用路由）
3. **主对话整段留 DeepSeek**，不按上下文长度切（破坏连贯）
4. **后台杂活走 MiniMax**（Claude Code `ANTHROPIC_DEFAULT_HAIKU_MODEL` / `CLAUDE_CODE_SUBAGENT_MODEL`），消化后台任务/子代理

## 架构（一句话）

```
你的机器（本地角色分配）
  Claude Code / Codex / OpenCode
    主对话 → deepseek-v4-pro      → DeepSeek
    haiku/后台 → MiniMax-M3       → MiniMax
                       ↓ 都指向
展示服务器 121.40.184.111（Caddy 入口）
  ├── /v1/* → 自建 Go 网关（gateway，:8080，systemd gateway.service）
  ├── /panel → 用量面板（自建 HTML + API）
  └── /docs → 文档库
              ↓ 计量数据写
存储服务器 115.29.241.36（PostgreSQL，5 表）
```

## 代码结构

- `gateway/` — 自建 Go 网关
  - `cmd/gateway/main.go` — 主入口（依赖装配 + 路由注册 + 优雅停机）；`cli.go` 子命令（set-upstream）
  - `internal/gateway/` — 数据面（透传 + 计量落库）+ 管理面（key/渠道/用量 API）
  - `internal/proxy/` — 上游请求构造、Transport（显式连接池 + 分阶段超时）、SSE 转发
  - `internal/routing/` — 路由快照（原子发布，热更新不影响在途请求）
  - `internal/store/` — PostgreSQL 存储（pg.go）/ 内存（mem.go）
  - `internal/metering/` — 计量（usage_anthropic / usage_chat / usage_responses + fallback）
  - `internal/keys/` — virtual key 管理 + 认证（SHA-256 查表）
  - `internal/config/` — 渠道配置加载
  - `cmd/migrate` / `cmd/dbclean` — 迁移 / 清理
- `web/` — 独立管理控制台（Vite + React 19 + Semi Design + ECharts，构建产物 dist/ 由网关 FileServer serve）
- `config.yaml` — 渠道配置（provider → models → routes → pricing）
- CI：`.github/workflows/ci.yml`（gofmt + vet + test -race + build）；`Dockerfile`（单二进制镜像）

## 部署（双服务器）

- **展示服务器**（121.40.184.111）：Caddy（入口）+ `gateway.service`（systemd，:8080）
- **存储服务器**（115.29.241.36）：PostgreSQL（gateway 库，5 表：upstream_keys/keys/usage_logs/channels/models）
- 客户端不用 4000 端口（那是 LiteLLM 时代）——统一走 `http://121.40.184.111`（Caddy）

## 客户端接入

- Claude Code：`ANTHROPIC_BASE_URL=http://121.40.184.111` + 主/hai。ku/subagent 模型 env（见 docs/clients.md）
- Codex / OpenCode：`base_url=http://121.40.184.111/v1`（OpenAI 兼容，wire_api responses）

## 环境约束

- 两台 2核2G 服务器（展示 + 存储），国内，Docker（Caddy）只有入口在用
- 客户端在国内网络，DeepSeek/MiniMax 均为国内 API
- 密钥：客户端用带预算的 virtual key，不用 master key

## 已知待验证项

- MiniMax 模型 id / 订阅端点（当前 `MiniMax-M3`，Coding Plan）
- 各 agent 的 `[1m]` 后缀是否被网关接受（建议网关侧剥后缀归一化）
