# RelayDock

**一个二进制、零外部依赖的订阅账号池网关** —— 把 ChatGPT/Claude 订阅账号和各厂 API key 聚合成统一端点，供 Codex / Claude Code / OpenCode 等 coding agent 使用。

```
Claude Code / Codex / OpenCode
        │  虚拟 key（gw-xxx）
        ▼
┌──────────────────────────┐
│  RelayDock（Go 单二进制）  │
│  · 账号池调度 + 故障转移    │
│  · 三原生协议纯透传        │
│  · 用量成本 + 全文日志     │
└──────────────────────────┘
   │ OAuth 凭据自动续期
   ▼
ChatGPT / DeepSeek / MiniMax / ...
```

## 30 秒部署

```bash
# 源码运行（默认 SQLite，数据落在 ./data/relaydock.db）
cd gateway
PANEL_PASSWORD=你的口令 go run ./cmd/gateway

# 或 Docker
docker compose up -d
```

打开 `http://localhost:8080` 进控制台：添加渠道 → 添加账号（API key 或 OAuth 登录）→ 签发虚拟 key → agent 指向 `http://your-host:8080/v1`。

## 为什么是这个项目

市面上中转站和大团队路由项目很多（one-api / new-api / LiteLLM），但它们是多租户计费平台：要 MySQL/Redis、带倍率/充值/兑换码、升级常炸。**个人和小团队要的其实很简单**：

- **一个二进制，零外部依赖**：SQLite 内嵌（自动建表），无 Redis，不用装数据库；内存模式可选
- **订阅账号池**：OAuth 登录收编订阅账号（Codex 首发），token 自动续期，多账号分摊 + 429 冷却 + 保守故障转移
- **原生协议纯透传**：OpenAI / Anthropic / Responses 三协议不翻译，thinking/reasoning 无损——不碰协议就永远不用追上游改版
- **用量成本透明**：三协议 usage 归一化、缓存读写独立计价、按 key×模型×账号归因、CSV/JSONL 导出、全文请求日志（默认关）
- **控制台内置**：React + Semi Design，渠道/账号池健康/日志/Key 全功能，不用装第二个项目

**明确不做**：多租户用户体系、计费/充值/兑换码、倍率概念（直接单价）、聊天 Playground。

## 功能矩阵

| 能力 | RelayDock | CLIProxyAPI | gpt-load | one-api/new-api |
|---|---|---|---|---|
| 单二进制 + 内嵌控制台 | ✅ | ❌（UI 在另一个项目） | ❌（Docker） | ✅（重） |
| 零外部依赖（SQLite 内嵌） | ✅ 默认 | ✅（无统计） | ✅ 默认 | ❌（计费打崩 SQLite） |
| 订阅 OAuth 账号池 | ✅（Codex，扩展中） | ✅（核心） | ✅ | ❌ |
| 每账号并发槽 | ✅（业界少有） | ❌ | ❌ | ❌ |
| 429 冷却 + 保守 failover + CAS 恢复 | ✅ | 部分 | 部分 | ❌ |
| 三协议纯透传（不翻译） | ✅ | ✅ | 部分 | ❌（转换） |
| 用量/成本/缓存计价内置 | ✅ | ❌（外包生态） | ✅ | ✅（耦合计费） |
| 全文请求日志（默认关） | ✅ | ❌ | ✅ | ❌（被拒） |
| 会话粘性（缓存感知） | ✅ | ❌ | ❌ | ❌ |

## 文档

- [docs/architecture.md](docs/architecture.md) — 系统架构
- [docs/software-architecture.md](docs/software-architecture.md) — 内部边界与核心不变量
- [docs/DECISIONS.md](docs/DECISIONS.md) — 决策记录（含"为什么不做协议转换"）
- [docs/clients.md](docs/clients.md) — agent 接入
- [docs/product-positioning.md](docs/product-positioning.md) — 定位与 MVP 依据

## License

MIT
