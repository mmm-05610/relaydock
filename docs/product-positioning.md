# 产品定位：面向个人/小团队的轻量订阅账号池网关

> 调研时间 2026-09-08。证据来源：7 个重量级项目的 issue 挖掘（SQLITE_BUSY/迁移地狱/自用模式系列）+ 轻量赛道 12+ 项目源码与 issue 分析。原始数据 /tmp/issuerecon/、/tmp/comp/。

## 1. 市场结论：结构性空位

**一代目**（one-api 36.8k★ / new-api 47.6k★ / LiteLLM 58.3k★）是多租户计费分发平台，对个人用户存在系统性错配：
- **计费强耦合**：倍率/额度/兑换码浸透数据模型，"自用模式"只是补丁（new-api#5827/#4119/#2419）；one-api#1105（46 条评论）公开要求"去掉计费、不要数据库"的纯网关版，两年未落地
- **数据库栈**：LiteLLM 明确拒绝 SQLite（#4583 not_planned）；new-api 默认 SQLite 被计费并发写打崩（SQLITE_BUSY 系列 4+ issue）
- **资源与稳定性**：LiteLLM OOM 系列（#14540/#25219），官方在 #31263（33 赞）承认要"最快最轻量"并转向 Rust——头部自己承认了这个空位
- **升级纪律**：new-api rc 连环坏 + 每版要手工 SQL（#7234）

**二代目**（2025-2026）：订阅经济催生"OAuth 账号池"新物种——CLIProxyAPI（51k★）、sub2api（40.9k★，PG+Redis 拼车平台）、gpt-load（6.6k★，小团队）、octopus（2.6k★，2025-11 新）。但：

| 空位 | 证据 |
| --- | --- |
| 用量/成本统计碎片化 | CLIProxyAPI 把统计从核心移除，用户被迫装 4 个第三方工具；uni-api 没有 |
| 单二进制+内嵌 UI+SQLite+订阅账号池的中间形态 | octopus/gpt-load 刚占位但粗糙：升级炸数据、路由难配、上游改版就坏 |
| 账号健康运营层 | 额度窗口/重置时间/不健康账号清理，只有 sub2api（重型）做了 |
| 多来源统一编排 | 官方 key + 多个中转站 key + 订阅账号的统一资产池，无成熟品 |
| 5-10 人小群拼车治理 | sub2api 太重，TokenAltar 太实验 |

## 2. 核心卖点

**一句话**：一个二进制、零外部依赖、专为个人和小团队打造的订阅账号池网关——原生三协议透传、账号池可靠性工程、用量成本透明。

四个差异化支柱（对着竞品软肋）：

1. **零外部依赖**：单二进制 + SQLite（内嵌）/内存/PG 三态，无 Redis。对着 one-api/new-api 的迁移地狱和 sub2api 的 PG+Redis。
2. **账号池可靠性工程**：每账号并发槽（业界独有）、429 冷却（Retry-After 优先+钳制+抖动）、保守 failover（5xx/网络错误不盲重试）、CAS 恢复、会话粘性（规划）。对着 CLIProxyAPI"均匀打散浪费缓存"、octopus"故障转移在补"。
3. **原生协议纯透传**：OpenAI/Anthropic/Responses 三协议不翻译，thinking/reasoning 无损。对着所有转换型网关的协议改写 treadmill（aiproxy#613 参数丢失反例）。
4. **用量成本透明内置**：三协议 usage 归一化（缓存读写独立计价）+ 账号级归因 + 全文日志旁路。对着 CLIProxyAPI 的 4 个第三方统计工具。

**反功能清单（明确不做，这就是定位）**：多租户用户体系、计费/充值/兑换码、倍率概念（直接单价）、聊天 Playground、支付集成。

## 3. 可发布 MVP 清单（v1.0）

### 已就绪 ✅
- 三协议纯透传数据面（anthropic/responses/chat_completions）+ SSE
- 订阅 OAuth 账号编排（Codex 首发，PKCE+刷新调度）+ API key 账号
- 账号池：并发槽、429 冷却、保守 failover、CAS 恢复、运行态观测
- 虚拟 key：额度、模型白名单、有效期、轮换/吊销、批量、四态状态
- 用量：单端点聚合（summary+series+分布 top5）、账号归因、缓存计价
- 请求日志：筛选/分页/CSV 导出/全文日志旁路（默认关）
- 控制台：六页 React + Semi Design，管理面全功能
- 运维：/healthz、优雅停机、双栈监听、Dockerfile、CI、schema 幂等迁移

### 发布前必须补齐（gap）
| # | 项 | 为什么是 MVP | 成本 |
| --- | --- | --- | --- |
| 1 | **SQLite 内嵌存储**（modernc.org/sqlite 纯 Go 无 CGO） | "数据不丢+零外部依赖"是轻量赛道默认形态（gpt-load/octopus 都默认 SQLite）；当前 mem 重启清零 / PG 要外部库，中间断档 | M |
| 2 | **会话粘性调度**（prompt_cache_key/session-id 哈希绑账号） | 拟真度+缓存省钱，对着 CLIProxyAPI #365 被拒的需求；有缓存计量可闭环展示"省了多少" | M |
| 3 | **README 重写 + 安装脚本 + docker-compose** | 一键部署是轻量赛道第一印象；当前 README 是自用笔记 | S |
| 4 | **/v1/models 元数据补全**（context_length 等前端可选项） | uni-api 系 issue 常年要；客户端兼容性 | S |
| 5 | **账号健康面板**：额度窗口/重置时间展示 | MC 的 quota 水位范式；Codex 有 x-codex-* 响应头可提取 | M |

### 发布后按需（v1.x）
- Gemini 原生协议入站（三协议→四协议）
- 账号级出口代理（每账号独立 IP）
- 渠道级 token 上限（吃免费额度场景）
- 备份快照导出/导入
- 小群拼车治理（配额隔离，无支付）

## 4. 发布动作建议

1. 打 tag v0.1.0 + GitHub Release（二进制产物 + docker image）
2. README 按"轻量赛道第一屏"标准重写：一句话定位 → 30 秒部署 → 截图 → 功能矩阵（对着竞品）
3. 发到聚合渠道引流（如 "awesome-llm" 类列表）；先在几个种子用户（拼车群）验证安装路径
4. issue 模板 + roadmap 文档（把本文件的反功能清单写明，管理预期）
