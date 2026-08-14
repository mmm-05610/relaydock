# 架构决策记录（ADR）

> 本文档记录这个项目的核心决策及其理由。每次架构决策变更，先改这里。

## 背景

- 两台 **2核2G** 云服务器（阿里云）：① 女朋友账号跑 LiteLLM 数据节点，② 毛庆辉账号跑导航页（可备案）
- 已有 **DeepSeek API**（充值额度）+ **MiniMax Coding Plan 订阅**（量大管饱）
- 日常用多个 coding agent（agent-box 管理 claude / codex / opencode / hermes）
- 目标：把两家额度高效用起来，一个统一入口（猫猫王国），按角色/场景分配模型

## 决策 1：用 LiteLLM 做网关，不用 one-api / new-api

**结论**：LiteLLM（自托管，MIT）。

**理由**：

- one-api / new-api 是"中转站"定位——核心是令牌计费、兑换码、在线支付、给下游二次分发 key。那是**开给别人卖**的模式，我们是**自用**。
- LiteLLM 定位就是"自建网关"：多个上游 key → 一个 OpenAI/Anthropic 兼容端点 → 客户端自选模型。
- LiteLLM 同时暴露 Anthropic `/v1/messages` 和 OpenAI `/v1/chat/completions`，Claude Code（Anthropic 协议）和 Codex/OpenCode（OpenAI 协议）都能直接用。

**代价**：LiteLLM 无华丽面板（有基础 Dashboard + spend tracking），需要自管 Docker + 可选 Postgres。个人自用够。

## 决策 2：不做语义路由

**结论**：不搞"路由器看懂请求自动选模型"。

**理由**：

- 语义路由（按 prompt 内容智能选模型）在 2026 仍不可靠，路由器的启发式大概率不如用户自己判断。
- 只有 2 家厂商，自动选模型的收益≈0，反而引入不确定层。
- 需要该能力时看 AISIX/Higress（Rust/Envoy，语义路由），当前阶段明确不做。

## 决策 3：不用 claude-code-router

**结论**：不引入。

**理由**：

- claude-code-router 的核心价值是按请求类型自动分类（background/think/longContext/webSearch）。
- 它**只服务 Claude Code**——而我们还有 OpenCode / Codex / hermes，一个只服务单客户端的路由器覆盖不了。
- 我们需要的"后台杂活 → MiniMax"可以用 **Claude Code 自带的 `ANTHROPIC_SMALL_FAST_MODEL`** 实现，不需要额外工具。

## 决策 4：主对话整段留在主模型（DeepSeek），不按长度切

**结论**：主对话（任何长度）→ DeepSeek；后台杂活 → MiniMax。

**理由**：

- 使用习惯：一个会话一直聊到底，要求主模型综合完整上下文、保持角色/决策脉络连贯。
- 若按上下文长度把长请求切给 MiniMax：主模型丢失后段上下文，MiniMax 没有前段脉络，**连贯性被切断，两边都不完整**。
- 因此正确切法是按"是否要连贯"切，不是按"多长"切：
  - **主对话**（要连贯、要角色）→ DeepSeek，整段
  - **后台杂活**（摘要/命名/子任务，不需要连贯）→ MiniMax

## 决策 5：LiteLLM 放远端服务器

**结论**：LiteLLM 部署在远端（服务器 B 应用层），不在本地。服务器 A 只跑数据库。

**理由**：

- 稳定常驻、一个入口、多设备/多 agent 共用。
- 两家 key 集中在服务器一处，不在各机器上散落。
- 服务器在国内（腾讯/阿里），DeepSeek/MiniMax 也是国内 API，就近无延迟问题。
- 本地只做"角色分配"（Claude Code 配置），灵活、轻。

## 决策 6：MiniMax 利用率

**结论**：后台杂活默认喂 MiniMax；若仍吃不满，可手动把探索/调研类会话整体切到 MiniMax。

**理由**：

- "后台杂活≈60% token"是 claude-code-router 的统计数据，Claude Code 自带 small-fast 的实际覆盖量需实测。
- 若 MiniMax 利用率仍低，按会话粒度手动切（探索/调研 → MiniMax）同样是既定分工，不破坏主对话连贯性。

## 已验证结论（原待验证项，均已落地）

- ✅ **MiniMax 模型**：`MiniMax-M3`、`MiniMax-M2.7-highspeed`（Coding Plan 订阅）。base_url = `https://api.minimaxi.com`（国内端点；OpenAI 协议走 `/v1`，Anthropic 协议走 `/anthropic`）。不是旧文档猜的 `MiniMax-Text-01`。
- ✅ _*Claude Code 接受非 claude-* 模型名_*：通过 LiteLLM 的 `model_name` 映射即可（如 `DeepSeek:deepseek-v4-pro(anthropic)`），无需 `claude-*` 别名。
- ✅ **LiteLLM 数据库**：新版（1.96+）已移除 SQLite，必须用 PostgreSQL（虚拟 keys + spend tracking 依赖）。

## 决策 7：数据库用 PostgreSQL（放服务器 A）

**结论**：LiteLLM 配 PostgreSQL（`postgres:16-alpine`），PG 独占服务器 A（数据层），LiteLLM 在 B 远程连。

**理由**：

- 新版 LiteLLM 已移除 SQLite，`database_url` 只认 `postgresql://`。
- 虚拟 keys（给女朋友发 key、按 key 限额）+ spend tracking（消耗统计）都依赖 PG。
- PG 独占 A（约 250MB），A 成为"纯数据节点"，只暴露 5432 给 B（安全组放行 B 的 IP）。
- 跨服务器 DB 连接走同地域阿里云内网骨干（延迟 <1ms），对 LLM 网关可忽略；换来分层干净。

## 决策 8：模型命名三段式 + 双协议透传

**结论**：model_name 采用 `供应商:模型名(协议)` 格式（如 `DeepSeek:deepseek-v4-pro(anthropic)`），每个模型 × 每种协议端点 = 一条独立配置，当前共 8 条。

**理由**：

- LiteLLM 的 Anthropic↔OpenAI 协议转换**并非无损**（thinking/reasoning_content 会丢，推理模型多轮对话断裂）。
- 因此不依赖转换，改用**透传**：客户端用什么协议，就配同协议的上游端点，模型名后缀 `(anthropic)`/`(openai)` 区分。
- 一个 model_name 只能对应一个端点，多协议就多名字；三段式命名让归属/模型/协议一眼可辨。

## 决策 9：消耗统计面板自建

**结论**：自建轻量 FastAPI 面板（`litellm-spend-dashboard`），替代 LiteLLM 企业版报表。

**理由**：

- LiteLLM 免费版的"按模型聚合报表"是企业功能（需 license），但数据接口 `/spend/logs` 免费可用。
- 自建面板：FastAPI 后端代理（master key 不出浏览器）+ 自绘 SVG 图表，轻量适配 2核2G。

## 决策 10：第二台服务器（B）= 应用 + 展示 + 入口

**结论**：第二台 2核2G（毛庆辉账号，可备案）= 服务器 B，跑应用层（LiteLLM）+ 展示层（面板 + 导航页）+ 入口层（Caddy）；女朋友那台 = 服务器 A，只跑数据层（PG）。

**理由**：

- 备案要求"备案主体名下有服务器"，服务器在女朋友账号无法备案（服务码授权要企业账号）。
- 导航页定位"个人门户"：公开内容（博客/文档/简历）+ 私有服务（网关/面板），玻璃拟态风格。
- B 是唯一公网入口，A 只暴露 5432 给 B，其他全藏。

## 决策 11：三层架构 + 子路径反代

**结论**：服务按「数据层 / 服务层 / 展示层 + 入口层」分层，B 用 Caddy 子路径反代（`/gateway`、`/panel`），只暴露 80/443。

**分层**：

- 数据层（A）：PostgreSQL，只被服务层访问
- 服务层（B）：LiteLLM（连 A 的 PG）
- 展示层（B）：消耗面板（连 litellm）+ 导航页
- 入口层（B）：Caddy 反代，唯一公网入口

**子路径反代**（不暴露 4000/8080）：

| 路径         | 反代到                                 |
| ------------ | -------------------------------------- |
| `/gateway/*` | LiteLLM（`SERVER_ROOT_PATH=/gateway`） |
| `/panel/*`   | 面板（caddy `handle_path` 剥前缀）     |
| `/`          | 导航页                                 |

**理由**：

- 依赖单向不跨层：展示层 → 服务层 → 数据层。
- 子路径反代让 B 只暴露 80/443，UI/面板/API 端口（4000/8080）不对外。
- LiteLLM 用 `SERVER_ROOT_PATH` 支持子路径；面板用 caddy 剥前缀（面板是根路径）。
