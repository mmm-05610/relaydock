# 架构决策记录（ADR）

> 本文档记录这个项目的核心决策及其理由。每次架构决策变更，先改这里。
> 当前架构：**自建 Go 透传网关**（替代 LiteLLM），见决策 12。

## 背景

- 两台 **2核2G** 云服务器（阿里云）：① 女朋友账号跑 PostgreSQL 数据节点（115.29.241.36），② 毛庆辉账号跑应用 + 入口（121.40.184.111，可备案）
- 已有 **DeepSeek API**（充值额度）+ **MiniMax Coding Plan 订阅**（量大管饱）
- 日常用多个 coding agent（agent-box 管理 claude / codex / opencode / hermes）
- 目标：把两家额度高效用起来，一个统一入口（猫猫王国），按角色/场景分配模型

---

## 决策演进（时间线）

```
决策 1–11（LiteLLM 时代）              ← 已废弃，仅作历史背景
  └─ LiteLLM Python + 8 条三段式命名 + 自建面板 + 子路径反代

决策 12（当前）：自建 Go 网关            ← 当前架构
  └─ 单二进制 + 纯透传 + PG 渠道表 + 静态面板 + Caddy 入口
```

---

## 决策 1–11（LiteLLM 时代，已废弃）

> **状态：已废**。决策 12 用自建 Go 网关替代了 LiteLLM。下文保留作为历史参考。
> 当前实际架构、组件、端口、命令等见决策 12 与 [architecture.md](architecture.md) / [setup.md](setup.md)。

### 决策 1（已废）：用 LiteLLM 做网关，不用 one-api / new-api

**结论**：LiteLLM（自托管，MIT）。

**理由**：

- one-api / new-api 是"中转站"定位——核心是令牌计费、兑换码、在线支付、给下游二次分发 key。那是**开给别人卖**的模式，我们是**自用**。
- LiteLLM 定位就是"自建网关"：多个上游 key → 一个 OpenAI/Anthropic 兼容端点 → 客户端自选模型。
- LiteLLM 同时暴露 Anthropic `/v1/messages` 和 OpenAI `/v1/chat/completions`，Claude Code（Anthropic 协议）和 Codex/OpenCode（OpenAI 协议）都能直接用。

**代价**：LiteLLM 无华丽面板（有基础 Dashboard + spend tracking），需要自管 Docker + 可选 Postgres。个人自用够。

### 决策 2（仍有效）：不做语义路由

**结论**：不搞"路由器看懂请求自动选模型"。

**理由**：

- 语义路由（按 prompt 内容智能选模型）在 2026 仍不可靠，路由器的启发式大概率不如用户自己判断。
- 只有 2 家厂商，自动选模型的收益≈0，反而引入不确定层。
- 需要该能力时看 AISIX/Higress（Rust/Envoy，语义路由），当前阶段明确不做。

### 决策 3（仍有效）：不用 claude-code-router

**结论**：不引入。

**理由**：

- claude-code-router 的核心价值是按请求类型自动分类（background/think/longContext/webSearch）。
- 它**只服务 Claude Code**——而我们还有 OpenCode / Codex / hermes，一个只服务单客户端的路由器覆盖不了。
- 我们需要的"后台杂活 → MiniMax"可以用 **Claude Code 自带的 `ANTHROPIC_DEFAULT_HAIKU_MODEL` / `CLAUDE_CODE_SUBAGENT_MODEL`** 实现，不需要额外工具。

### 决策 4（仍有效）：主对话整段留在主模型（DeepSeek），不按长度切

**结论**：主对话（任何长度）→ DeepSeek；后台杂活 → MiniMax。

**理由**：

- 使用习惯：一个会话一直聊到底，要求主模型综合完整上下文、保持角色/决策脉络连贯。
- 若按上下文长度把长请求切给 MiniMax：主模型丢失后段上下文，MiniMax 没有前段脉络，**连贯性被切断，两边都不完整**。
- 因此正确切法是按"是否要连贯"切，不是按"多长"切：
  - **主对话**（要连贯、要角色）→ DeepSeek，整段
  - **后台杂活**（摘要/命名/子任务，不需要连贯）→ MiniMax

### 决策 5–11（已废，LiteLLM 部署相关）

略，详见 git 历史。关键事实：LiteLLM 已被 Go 网关全面替代，不再维护。

---

## 决策 12：自建 Go 透传网关（替代 LiteLLM）★ 当前架构

**结论**：放弃 LiteLLM（Python 版 + Rust 版都否掉），自建 **Go 单二进制透传网关**（纯透传零转换）+ 静态前端面板，替代 litellm + spend-dashboard + navpage 三个子服务。完整设计见 [architecture.md](architecture.md)。

### 12.1 LiteLLM 三条路都走不通

| 路径                         | 问题                                                                                                |
| ---------------------------- | --------------------------------------------------------------------------------------------------- |
| LiteLLM Python 版            | 功能略重（~1.3GB 启动峰值），核心是**格式转换**，Anthropic↔OpenAI 转换有损（thinking/reasoning 丢） |
| LiteLLM Rust 版              | 早期 beta、无预构建镜像、responses 路由未覆盖、provider 硬编码无 deepseek/minimax                   |
| 现成替代（otari/one-api 等） | 全是"转换型"网关，纯透传做不好（one-api 改 Content-Type、new-api 空 tools 注入）                    |

### 12.2 真实需求只有四条，都很轻

**统一路由 + key 管理 + 用量统计 + 纯透传**。

关键事实：OpenAI Responses 和 Anthropic Messages 响应**自带 `usage` 字段**，用量统计不需要 tokenizer。参考实现：litellm 用量统计 ~3600 行代码 + 4.6 万行价格表（喂给 100+ provider），我们的核心只需 ~100 行。

### 12.3 核心设计

- **协议无关透传**：网关不认识协议，只做「认证 → 按 model 查 routes → 原样转发」；协议只存在于两处：渠道的模型路由条目、计量的 usage 提取器（anthropic / responses / chat_completions）。
- **渠道在 PG 中管理**：channels 表 + models 表存「渠道 → 模型 → 多协议路由 + 价格」，面板增删改 + 热更新（`reloadChannels()`），`gateway/config.yaml` 退化为首次部署的种子数据。
- **key 全对称加密**：上游 key AES-256-GCM 存 A 机 PG（master key 走 `GATEWAY_MASTER_KEY` env）；虚拟 key 只存 SHA-256 哈希。
- **计量旁路**：读上游 usage（anthropic 顶层 cache 字段 / responses `details.cached_tokens` / chat `prompt_tokens`）+ 字符估算 fallback（`metering/fallback.go`，pre-call 兜底）+ 缓存计价（每模型独立单价，不是写死倍数）。
- **单二进制零依赖**：Go 编译成 ~10MB 二进制（B 机 `gateway/bin/gateway`），systemd `gateway.service` 跑前台，B 机的 Caddy 反代 /v1/* 和 /api/* 到 `:8080`。
- **静态面板由 gateway 直接 serve**：`mux.Handle("/", http.FileServer(http.Dir(staticDir)))`（默认 `../web/dist`），Caddy 只做 TLS + 反代入口。
- **管理 API 认证 = PANEL_PASSWORD**（Bearer 对称口令，env 注入），不是 master key。

### 12.4 已验证结论（curl 实测 2026-08-14）

| 供应商   | anthropic                | responses                    |
| -------- | ------------------------ | ---------------------------- |
| DeepSeek | `/anthropic/v1/messages` | `/responses`（根路径）       |
| MiniMax  | `/anthropic/v1/messages` | `/v1/responses`（在 /v1 下） |

- upstream model 名直接用 `deepseek-v4-pro` / `MiniMax-M3`（config 里 model 段写啥就发啥）。
- usage 字段语义：anthropic `input_tokens` 不含缓存（顶层 cache 字段）；responses `input_tokens` 含缓存（`input_tokens_details.cached_tokens`）。
- responses 流式终止事件有 `completed` 和 `incomplete` 两个（截断时是后者），extractor 都要认。
- 客户端 model 名是「直白」的（如 `deepseek-v4-pro`），不再用 LiteLLM 时代的 `供应商:模型名(协议)` 三段式——协议由 URL path 决定，模型表里 `routes` 字典按 path 索引。

---

## 决策 13：双服务器分层（数据层 / 服务层 / 入口层）

**结论**：

- A 机（115.29.241.36，女朋友账号）= 纯数据节点，只跑 PostgreSQL（独立库 `gateway`）。
- B 机（121.40.184.111，毛庆辉账号，可备案）= 服务层（Go 网关 binary + systemd）+ 入口层（Caddy，TLS + 反代 + 静态）。

**理由**：

- 备案要求"备案主体名下有服务器"，服务器在女朋友账号无法备案（服务码授权要企业账号）。
- A 机安全组只放行 5432 给 B 机；B 机只暴露 80/443 给公网；客户端不再用 4000（LiteLLM 时代）端口，统一 `http://121.40.184.111`（Caddy）。
- Go 网关是单二进制，无 Docker 依赖，systemd 跑前台；Caddy 跑入口反代，二者都轻量，能在 2核2G 上稳定常驻。

---

## 决策 14：客户端 model 名直白，协议由 URL path 决定

**结论**：客户端请求 body 里 `model` 用直白名字（如 `deepseek-v4-pro`、`minimax-m3`、`deepseek-v4-flash`），协议由请求路径（`/v1/messages` / `/v1/responses` / `/v1/chat/completions`）决定。

**理由**：

- LiteLLM 时代用三段式（`供应商:模型名(协议)`）是因为 LiteLLM 内部按 `model_name` 路由，需要把协议信息编码进名字。自建网关不需要。
- 网关 `m.FindRoute(clientPath)` 按 URL path 查 `routes` 字典，命中后取 `upstream` + `upstream_model` + `usage_protocol`。同一个客户端模型名可以在多个协议下透传。
- 简化客户端配置：Claude Code 设 `ANTHROPIC_DEFAULT_MODEL=deepseek-v4-pro` 即可，不用再为不同协议开多个变量。

---

## 决策 15：上游 key 优先从 PG 读，env 作为回退

**结论**：启动时先查 PG（`upstream_keys` 表 AES 密文），没有则回退到环境变量（`DEEPSEEK_API_KEY` / `MINIMAX_API_KEY`）。

**理由**：

- PG 录入是默认路径（管理 API `POST /api/channels/{provider}/key` 或 CLI `gateway keys set-upstream --provider <name>`），安全（加密落库）。
- 环境变量作为开发期/紧急情况的回退，方便本地 `go run` 调试（不需要 PG 也能跑起来，bootstrap key 自动生成）。
- 切换两套方式不需要改代码，重启 gateway 生效。

---

## 待验证项

- 渠道表热更新：当前是 `reloadChannels()` 全量替换，大渠道下需要确认并发安全（读路径无锁，但模型列表变更期间可能有请求路由到旧配置）。
- 流式中断时的 input 估算精度：`EstimateInputTokens(body)` 用字符/3 估算，正负 10–20%，已作为 unmetered fallback。
- `[1m]` 后缀（Claude Code 声明上下文）：网关侧目前未剥后缀，依赖 Claude Code 客户端自己处理；待验证上游是否接受。
