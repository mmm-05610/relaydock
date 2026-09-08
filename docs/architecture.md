# 架构设计 v2：自建 Go 透传网关（当前）

> 当前架构：自建 Go 网关替代 LiteLLM（决策 12）。决策演进见 [DECISIONS.md](DECISIONS.md)。
> 本文件描述**实际代码**对应的事实：组件、端口、路径、表名、命令均与 `gateway/` 代码一致。

## 1. 背景与动机

LiteLLM 三条路都走不通，结论收敛到"自建"：

| 路径                         | 问题                                                                                                |
| ---------------------------- | --------------------------------------------------------------------------------------------------- |
| LiteLLM Python 版            | 功能略重（~1.3GB 启动峰值），核心是**格式转换**，Anthropic↔OpenAI 转换有损（thinking/reasoning 丢） |
| LiteLLM Rust 版              | 早期 beta、无预构建镜像、responses 路由未覆盖、provider 硬编码无 deepseek/minimax                   |
| 现成替代（otari/one-api 等） | 全是"转换型"网关，纯透传做不好（one-api 改 Content-Type、new-api 空 tools 注入）                    |

真实需求只有四条，都很轻：**统一路由 + key 管理 + 用量统计 + 纯透传**。

关键事实：OpenAI Responses 和 Anthropic Messages 响应**自带 `usage` 字段**，用量统计不需要 tokenizer。参考实现：litellm 用量统计 ~3600 行代码 + 4.6 万行价格表（喂给 100+ provider），我们的核心只需 ~100 行。

## 2. 设计原则

1. **纯透传**：请求 body 原样转发、响应原样返回，零格式转换。
2. **前后端两个服务**：一个 Go 单二进制（后端 + 静态面板 serve）+ 一个静态前端面板。不再有 litellm/spend-dashboard/navpage 三个子服务。
3. **复用 A 机 PG**：用量、key、渠道、模型、上游 key 数据存 A 机 PostgreSQL（已是纯数据节点，独立库 `gateway`）。
4. **单二进制零依赖**：Go 编译成 ~10MB 二进制（`gateway/bin/gateway`），无运行时依赖，B 机 systemd 跑前台。
5. **渠道在 PG 管理，配置 YAML 只作种子**：启动时若 PG 里没渠道，从 `gateway/config.yaml` 导入一次；后续面板增删改都写 PG + `reloadChannels()` 热更新。

## 3. 总体架构

```
                    ┌─────────────────────────────────────────┐
                    │  B 机（2核2G · 公网入口 · 可备案）         │
  agent ──HTTPS──▶  │  Caddy（:80/:443 TLS + 反代）           │
  Claude Code       │   ├─ /            → Go 网关内置 FileServer │
  Codex             │   ├─ /v1/*        → Go 网关（透传）        │
  OpenCode          │   └─ /api/*       → Go 网关（管理 API）    │
  Hermes            │                                          │
                    │  Go 网关（systemd gateway.service :8080）   │
                    │   ├─ /v1/messages /v1/responses           │
                    │   │   /v1/chat/completions  协议无关透传   │
                    │   ├─ 认证 + 额度硬挡                       │
                    │   ├─ 计量（读 usage → PG，旁路）            │
                    │   └─ 管理 API /api/keys /api/channels     │
                    └───────────────┬──────────────────────────┘
                                    │ 内网 <1ms
                    ┌───────────────▼──────────────────────────┐
                    │  A 机（2核2G · 纯数据节点 · 不对外）         │
                    │  PostgreSQL :5432（安全组只信任 B 机 IP）   │
                    │   ├─ upstream_keys（AES-GCM 密文）         │
                    │   ├─ keys（SHA-256 哈希 + 额度）           │
                    │   ├─ usage_logs                           │
                    │   ├─ channels                             │
                    │   └─ models                               │
                    └──────────────────────────────────────────┘
                                    │ 公网
                    ┌───────────────▼──────────────────────────┐
                    │  上游供应商（国内 API）                     │
                    │  DeepSeek  /anthropic  /responses         │
                    │           /v1/chat/completions            │
                    │  MiniMax   /anthropic  /responses         │
                    │           /v1/chat/completions            │
                    └──────────────────────────────────────────┘
```

**分层**：

| 层     | 位置 | 内容                                                  |
| ------ | ---- | ----------------------------------------------------- |
| 数据层 | A 机 | PostgreSQL（6 张表）                                  |
| 服务层 | B 机 | Go 网关（透传 + 认证 + 计量 + 管理 API）              |
| 展示层 | B 机 | 管理控制台（web/ 构建产物，由 Go 网关内置 FileServer serve） |
| 入口层 | B 机 | Caddy（TLS + 反代，唯一公网入口）                     |

## 4. 组件设计

### 4.1 Go 网关（后端 + 静态服务，单二进制）

语言 **Go**，框架标准库 `net/http`（Go 1.22+ `ServeMux` 方法路由），依赖仅 `pgx` + `yaml.v3`。

**包结构**（`gateway/`）：

```
cmd/gateway/main.go   — 主入口（依赖装配 + 路由注册 + 优雅停机）
cmd/gateway/cli.go    — 子命令（keys set-upstream）
cmd/migrate/          — schema 迁移（手动）
cmd/dbclean/          — 用量日志清理（手动）
internal/
  gateway/            — 数据面（透传 + 计量落库）+ 管理面（key/渠道/账号/用量 API）
    gateway.go          — 依赖装配 + 路由注册 + 快照重建
    data.go             — handleModels / handleProxy（认证 → 路由 → 转发 → 计量）
    attempts.go         — 账号池执行路径（failover / 本地 429/503 / 归因）
    accounts.go         — 账号管理 API（CRUD + test + 运行时摘要）
    admin.go            — 其余管理 API handlers
  proxy/              — 上游请求构造 / Transport / SSE 转发
    transport.go        — 显式连接池 + 分阶段超时（不读 *_proxy 环境变量）
    request.go          — BuildRequest（认证/协议 header）+ ReplaceModel
    relay.go            — SSE 逐行转发 + 计量旁路 feed
  pool/               — 上游账号池
    state.go            — AccountSpec/State/Ref + 原子 tryAcquire + 幂等 Lease
    pool.go             — 选择算法（占用比例 + 轮换）+ Reconcile 热更新对账
    classify.go         — 保守错误分类（429/401/403/402 换账号；5xx/网络错误不换）
  routing/            — 路由快照（原子发布；热更新不影响在途请求）
  config/             — config.yaml 解析 + 渠道/模型/路由/价格结构
    config.go         — Config/Channel/Model/Route/Pricing 类型
    preset.go         — 内置 provider preset（面板可视化）
  store/              — 存储抽象（PG + 内存两套实现）
    store.go          — 接口定义
    pg.go             — pgx 实现（生产）
    mem.go            — 内存实现（开发 / 无 DATABASE_URL）
  keys/               — key 管理
    keys.go           — Manager（虚拟 key 签发/吊销/轮换/额度）
    crypto.go         — AES-256-GCM 加解密（master key）+ HMAC 指纹
  metering/           — 计量
    meter.go          — Meter 编排 + Usage/Pricing/Cost
    usage_anthropic.go   — anthropic 协议 extractor
    usage_responses.go   — responses 协议 extractor
    usage_chat.go        — chat_completions 协议 extractor
    fallback.go          — 字符/3 估算（pre-call 兜底）
```

**主入口路由表**（`cmd/gateway/main.go`）：

```go
// 数据面
mux.HandleFunc("GET /v1/models", handleModels)  // OpenAI 兼容模型列表
mux.HandleFunc("/v1/",           handleProxy)   // 所有 /v1/* 协议无关透传

// 管理 API（均需 Bearer PANEL_PASSWORD，dev 环境留空 = 无认证）
mux.HandleFunc("POST /api/keys",                   handleCreateKey)
mux.HandleFunc("GET  /api/keys",                   handleListKeys)
mux.HandleFunc("POST /api/keys/revoke",            handleRevokeKey)
mux.HandleFunc("GET  /api/keys/{hash}",            handleGetKey)
mux.HandleFunc("PUT  /api/keys/{hash}",            handleUpdateKey)
mux.HandleFunc("POST /api/keys/{hash}/rotate",     handleRotateKey)
mux.HandleFunc("POST /api/keys/{hash}/quota",      handleUpdateQuota)
mux.HandleFunc("GET  /api/usage",                  handleUsage)
mux.HandleFunc("GET  /api/dashboard",              handleDashboard)
mux.HandleFunc("GET  /api/usage/timeseries",       handleTimeseries)
mux.HandleFunc("GET  /api/usage/grouped",          handleGrouped)
mux.HandleFunc("GET  /api/logs",                   handleLogs)
mux.HandleFunc("GET  /api/channels",               handleChannels)
mux.HandleFunc("POST /api/channels",               handleCreateChannel)
mux.HandleFunc("PUT  /api/channels/{provider}",    handleUpdateChannel)
mux.HandleFunc("DELETE /api/channels/{provider}",  handleDeleteChannel)
mux.HandleFunc("POST /api/channels/{provider}/key",          handleSetChannelKey)
mux.HandleFunc("POST /api/channels/{provider}/test",         handleTestChannel)
mux.HandleFunc("GET  /api/channels/{provider}/balance",      handleChannelBalance)
mux.HandleFunc("GET  /api/channels/{provider}/remote-models",handleRemoteModels)
mux.HandleFunc("POST /api/channels/{provider}/models",       handleCreateModel)
mux.HandleFunc("PUT  /api/channels/{provider}/models/{name}",handleUpdateModel)
mux.HandleFunc("DELETE /api/channels/{provider}/models/{name}",handleDeleteModel)
mux.HandleFunc("GET  /api/channels/{provider}/accounts",     handleListAccounts)
mux.HandleFunc("POST /api/channels/{provider}/accounts",     handleCreateAccount)
mux.HandleFunc("PUT  /api/channels/{provider}/accounts/{id}",handleUpdateAccount)
mux.HandleFunc("DELETE /api/channels/{provider}/accounts/{id}",handleDeleteAccount)
mux.HandleFunc("POST /api/channels/{provider}/accounts/{id}/test",handleTestAccount)
mux.HandleFunc("GET  /api/settings/upstream",      handleUpstreamStatus)
mux.HandleFunc("POST /api/settings/password",      handleUpdatePassword)
mux.HandleFunc("GET  /api/upstream/balance",       handleUpstreamBalance)

// 静态面板（FileServer，作为 mux 兜底）
mux.Handle("/", http.FileServer(http.Dir(staticDir)))  // 默认 ../web/dist
```

**监听端口**：`:8080`（`addr := ":8080"`）。Caddy 反代 /v1, /api, / 到 :8080。

### 4.2 前端面板（静态站）

`web/` 下的 Vite + React 19 + Semi Design 独立控制台（构建产物 `dist/`），由 Go 网关内置的 `http.FileServer` 直接 serve。`fetch` 调 `/api/*`（统一 Bearer 口令 + 401 拦截）。

页面结构（信息架构对齐 one-api / gpt-load 家族的成熟 admin 形态）：

```
侧边栏（可折叠）
├── 概览          /api/dashboard + timeseries：今日成本/请求/成功率/缓存命中 + 7 日趋势
├── 用量分析      timeseries + grouped：趋势图 + 按模型/按 Key 对比
├── 请求日志      筛选表格，行展开看链路归因（渠道/账号/attempts/错误）
├── 虚拟 Key      签发/编辑/轮换/吊销（明文仅创建时一次可见）
├── 渠道与账号    渠道 CRUD + 模型路由 CRUD + 上游账号池健康（inflight/冷却）+ 测试
└── 设置          上游凭据状态 / 余额查询 / 面板口令
```

开发：`cd web && npm run dev`（Vite proxy /api → :8080）；构建：`npm run build` → `dist/`。

> 也可让 Caddy 直接 serve 静态文件（减少一次反代），见 [setup.md](setup.md) § Caddyfile。

### 4.3 Caddy（入口）

| 路径     | 反代到                                                    |
| -------- | --------------------------------------------------------- |
| `/`      | Go 网关内置 FileServer（或 Caddy `file_server` 直 serve） |
| `/v1/*`  | Go 网关 `:8080`                                           |
| `/api/*` | Go 网关 `:8080`                                           |

### 4.4 PostgreSQL（数据层）

A 机 PG，Go 网关内网连接。schema 见 §7。**6 张表**：channels / models / upstream_keys / upstream_accounts / keys / usage_logs。

### 4.5 上游账号池（多账号 failover）

设计见 [design-upstream-account-pool.md](design-upstream-account-pool.md)，第一版已实现：

- `upstream_accounts` 表：渠道下多份独立凭据（AES-GCM 密文 + HMAC 指纹去重）+ 每账号 `max_concurrency`；
- 渠道**没有显式账号时走 `upstream_keys` 隐式单 key 路径，行为与多账号功能上线前完全一致**（兼容红线）；
- 选择：按占用比例（inflight/max_concurrency）最低优先，同分轮换；检查与递增在同一把锁内（`tryAcquire`）；
- **原子 Acquire + 幂等 Lease**（`sync.Once` 归还，inflight 永不为负、不超上限）；
- 保守 failover：只有**账号级 429 / 401 / 403 / 402** 在客户端响应未提交时换账号（429 按 `Retry-After` 冷却 + 抖动）；5xx、网络错误结果未知，**不重试不惩罚**；
- 全部满槽 → 本地 429（`Retry-After: 5`）；全部冷却/禁用 → 本地 503；稳定 JSON 错误体；
- 配置热更新经快照原子发布，在途请求持有旧 `AccountRef` 正常归还（`-race` 验证）；
- 归因：`usage_logs` 增加 `channel_id` / `account_id` / `attempts`；
- 管理 API（信息架构对齐 gpt-load/new-api 收敛形态）：列表带池级健康汇总（总数/健康/冷却/禁用 + 严重度排序）、批量导入（凭据指纹去重）、批量运维（启停/删除/恢复）、单账号恢复（清冷却+失败状态，仅冷却中合法）、真实调用测试、7 天用量聚合（usage_logs 按 account_id）；运行态含成功/失败/连续失败/最后错误/最后状态码/最后使用时间（进程内，重启清零）；
- 控制台「渠道与账号」页：渠道 Drawer + 账号池 Tab（汇总条 + 状态筛选 + 批量操作条 + 健康列 + 5s 轻量轮询）。

### 4.6 OAuth 订阅账号编排（Codex 首发）

`oauth_profiles`（config.yaml，随快照发布）定义订阅类型的授权参数；参数取自对应官方 CLI 的公开常量（codex 源自 openai/codex-rs）。

```
添加账号 → OAuth 登录 → 选 profile → 发起授权（PKCE + state）
  → 浏览器登录（回调页报错属预期）→ 复制回调 URL/ code 贴回
  → 网关用 code + verifier 换 token（chatgpt_account_id 取自 id_token claim）
  → 加密落库为 oauth 型账号，进账号池
后台：刷新调度器（30s 扫描，过期前 5 分钟续期；失败冷却 10 分钟）
数据面：oauth 型凭据经内存 cache 动态解析，
  chatgpt_codex 样式附 chatgpt-account-id + OpenAI-Beta: responses=experimental
```

- API：`GET /api/oauth/profiles`；`POST .../accounts/oauth/start`、`GET .../oauth/{stage}`、
  `POST .../oauth/{stage}/code`、`DELETE .../oauth/{stage}`（stages 进程内 15 分钟 TTL）
- 表：upstream_accounts 增加 credential_type / oauth_profile / encrypted_token / token_expires_at / last_refresh_at
- 测试回传 code 支持「完整回调 URL」或「裸 code」两种粘贴方式

## 5. 协议路由（协议无关 + 渠道管理）

**转发层协议无关**：网关不认识任何协议，只做「认证 → 按 model 查渠道 → 按 URL path 查 route → 原样转发」。协议只存在于两处：

1. 渠道的模型路由条目（`routes` 字典按客户端 path 索引）
2. 计量的 usage 提取器（`anthropic` / `responses` / `chat_completions`）

```
任意 /v1/* 路径 + body.model
  → 认证 → 查 model 的渠道配置
  → 路径匹配 routes[clientPath]
  → 拿到 upstream 完整 URL + upstream_model + usage_protocol
  → 原样转发（换 base_url + 上游 key + 改 model 名）
```

**渠道 = 上游供应商 → 模型（多协议路由 + 价格）**。配置存 **PG**（`channels` + `models` 表），面板增删改 + 热更新，`gateway/config.yaml` 退化为首次部署的种子数据。

**config 结构**（`gateway/config.yaml`，渠道为中心）：

```yaml
channels:
  - provider: deepseek
    name: DeepSeek
    balance_type: balance # balance(余额) | quota(余量百分比)
    balance_url: https://api.deepseek.com/user/balance
    models_url: https://api.deepseek.com/models
    auth_mode: bearer # bearer(默认) | x_api_key
    models:
      - name: deepseek-v4-pro # 客户端 model 名（直白，无三段式）
        routes: # 多协议路由（纯透传）
          "/v1/messages":
            {
              upstream: "https://api.deepseek.com/anthropic/v1/messages",
              model: "deepseek-v4-pro",
              usage: anthropic,
            }
          "/v1/responses":
            {
              upstream: "https://api.deepseek.com/responses",
              model: "deepseek-v4-pro",
              usage: responses,
            }
          "/v1/chat/completions":
            {
              upstream: "https://api.deepseek.com/v1/chat/completions",
              model: "deepseek-v4-pro",
              usage: chat_completions,
            }
        pricing:
          input_per_m: 3 # 元/M
          output_per_m: 6
          cache_read_per_m: 0.025
          cache_write_per_m: 3
```

**关键设计**：

- `upstream` 存完整 URL（不拼接；各供应商路径结构不同）。
- **一个模型多协议**：同客户端模型名配多条协议路由，纯透传无损。别的中转站靠格式转换（Anthropic↔OpenAI）实现多协议，转换有损（丢 thinking/reasoning）——这是自建网关的核心优势。
- **价格用人民币（元/M）**：DeepSeek 官方价（pro 3/6/flash 1/2），MiniMax 官方五折价（M3 2.1/8.4）。
- **缓存是独立单价**（`cache_read_per_m` / `cache_write_per_m`），不是写死倍数（DeepSeek 缓存读 0.025/0.02，不是输入的 10%）。
- 加新供应商 = 渠道表加一行 + （可选）`internal/config/preset.go` 加一条 provider 预设。

## 6. 核心流程

### 6.1 透传链路（`handleProxy`）

```
1. 读请求 body（JSON，含 model 字段）
2. 认证：Bearer → SHA-256 → 查 keys 表（无效 401）
3. 路由：body.model → cfg.FindModel(name)
        → m.FindRoute(r.URL.Path) → 拿上游 URL + upstream_model + usage_protocol
4. 渠道/模型/权限检查（enabled / allowed_models）
5. 拿到上游 key：upstreamKeys[provider]（启动时已从 PG 解密缓存内存）
6. 改 body.model = upstream_model（若不同 → replaceModel）
7. 构造上游请求：替换 base_url + 上游 key + 协议 header（anthropic-version 等），其余 body/header 原样
8. 转发：
   - 流式（SSE）：bufio.Scanner 逐行 Write+Flush + meter.NewStreamAccumulator.Feed 监听 usage
   - 非流式：io.ReadAll 完整 body + meter.MeterNonStream 读 usage
9. 计量：defer recordUsage → 算 cost + InsertUsageLog + AddQuotaUsed（旁路，失败只 log 不阻断）
```

### 6.2 计量（读 usage + fallback，旁路）

**总纲：计量是旁路，不阻断、不延迟主链路；计量失败 = 少记一笔，请求照常。**

上游每次返回都自带 token 用量（字段 `usage`），网关不用自己数 token：

```json
{ "usage": { "input_tokens": 1500, "output_tokens": 800 } }
```

**归一化 Usage 结构**（`internal/metering/meter.go`）：每协议一个 extractor，把原始 usage 拍平成统一结构，成本只看统一结构。

```go
type Usage struct {
    InputTokens      int64 // 纯输入（不含缓存）
    OutputTokens     int64
    CacheReadTokens  int64 // 缓存命中
    CacheWriteTokens int64 // 缓存创建
}

type UsageExtractor interface {
    ExtractNonStream(body []byte) Usage            // 非流式
    ExtractStreamEvent(data []byte) (Usage, bool)  // 流式：SSE 行 → (部分 usage, 是否命中)
}
```

**字段语义坑**（每协议 extractor 必须单独处理）：

```json
// Anthropic：input_tokens 是"纯输入"，缓存是顶层独立字段
{ "input_tokens": 500, "cache_creation_input_tokens": 1000, "cache_read_input_tokens": 500 }

// Responses：input_tokens 是"总输入"，缓存藏在 details 里
{ "input_tokens": 1500, "input_tokens_details": { "cached_tokens": 500 } }

// chat_completions：prompt_tokens 含缓存（OpenAI 文档）
{ "prompt_tokens": 1500, "completion_tokens": 800, "prompt_tokens_details": { "cached_tokens": 500 } }
```

各 extractor 把字段归一化（`normalizeAnthropic` / `normalizeResponses` / `normalizeChat`），归一化后 `cost.go` 统一算，不碰协议。所有字段 `or 0` 兜 null。

**三段编排**：

- **① pre-call 估 input**：`metering.EstimateInputTokens(body)` 字符/3，兜底流式中断也不丢 input 账。
- **② 转发中监听**：`bufio.Scanner` 逐行 `Write`+`Flush` 透传，同时 `parseUsageEvent` 识别 usage 事件：

  | 协议             | 流式 usage 事件                                      | 内容                                |
  | ---------------- | ---------------------------------------------------- | ----------------------------------- |
  | Anthropic        | `message_start`                                      | input_tokens + cache                |
  | Anthropic        | `message_delta`（最后）                              | output_tokens                       |
  | Responses        | `response.completed` / `response.incomplete`（最后） | 完整 usage（截断时是 `incomplete`） |
  | chat_completions | 默认不带，需 `stream_options.include_usage`          | 流式只记 input 估算                 |

  （anthropic 拆两个事件拼；responses 读最后一个事件，`completed` 和 `incomplete` 都带 usage。）

- **③ post-call 落库**：`defer recordUsage(...)`，算 cost + 写 `usage_logs` + 累加 `quota_used`。

**成本计算**（`metering.Cost`）：

```go
cost = input/1e6 * input_per_m
     + cache_read/1e6  * cache_read_per_m   // 缓存命中
     + cache_write/1e6 * cache_write_per_m  // 缓存创建
     + output/1e6 * output_per_m
```

**优先级铁律**：有 usage 用 usage（100% 准），没 usage 才用估算（±10–20%）。

**边界**：

- 流式中断：`usage.InputTokens == 0 && usage.OutputTokens == 0` 时退估算 + 打 `unmetered=true`。
- 上游 4xx/5xx：有 usage 就读，没有记 0（落库仍记录 `status` + `error`）。
- 计量/写库失败：只 log，不阻断主请求。

### 6.3 渠道热更新

- 面板调用 `POST/PUT/DELETE /api/channels/*` 或 `.../models/*` → 写 PG → `reloadChannels()` 全量重读 `channels` 表到 `cfg.Channels`。
- 内存读写无锁，热更新期间可能出现「正在执行的请求路由到旧配置」的瞬时不一致（个人用，规模小，未做细粒度同步）。

## 7. 数据模型（PG schema，6 张表）

```sql
-- 上游真 key：AES-256-GCM 密文，master key 解密
CREATE TABLE upstream_keys (
  id              BIGSERIAL PRIMARY KEY,
  provider        TEXT NOT NULL UNIQUE,   -- deepseek | minimax
  encrypted_key   BYTEA NOT NULL,         -- nonce||ciphertext(GCM tag)
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 虚拟 key（发给 agent）：只存 SHA-256 哈希
CREATE TABLE keys (
  id             BIGSERIAL PRIMARY KEY,
  key_hash       TEXT UNIQUE NOT NULL,
  name           TEXT NOT NULL,
  owner          TEXT,                      -- 谁拥有（多用户归属）
  agent_type     TEXT,
  quota_limit    NUMERIC(12,6),             -- 额度（按 cost 累加），NULL = 不限
  quota_used     NUMERIC(12,6) NOT NULL DEFAULT 0,
  enabled        BOOLEAN NOT NULL DEFAULT TRUE,
  allowed_models TEXT NOT NULL DEFAULT '',  -- 允许访问的模型（逗号分隔，空=不限）
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 用量日志
CREATE TABLE usage_logs (
  id                BIGSERIAL PRIMARY KEY,
  key_id            BIGINT REFERENCES keys(id),
  model             TEXT NOT NULL,         -- 客户端 model 名
  upstream_model    TEXT,                  -- 上游实际 model 名
  protocol          TEXT NOT NULL,         -- anthropic | responses | chat_completions
  input_tokens      INTEGER,
  output_tokens     INTEGER,
  cache_read_tokens  INTEGER,
  cache_write_tokens INTEGER,
  cost              NUMERIC(12,6),         -- 元
  latency_ms        INTEGER,
  status            INTEGER,               -- HTTP 状态码（200/401/404/502...）
  error             TEXT,                  -- 失败原因（空 = 成功）
  request_id        TEXT,                  -- 客户端请求 ID（用于排错）
  unmetered         BOOLEAN NOT NULL DEFAULT FALSE,  -- 没拿到 usage、退估算的标记
  channel_id        BIGINT,                -- 最终承载响应的渠道
  account_id        BIGINT,                -- 最终承载响应的上游账号（隐式账号 NULL）
  attempts          INTEGER NOT NULL DEFAULT 1,  -- 上游尝试次数
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_usage_key_time ON usage_logs (key_id, created_at DESC);

-- 上游账号（渠道下的独立凭据 + 并发容量，docs/design-upstream-account-pool.md）
CREATE TABLE upstream_accounts (
  id               BIGSERIAL PRIMARY KEY,
  channel_id       BIGINT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  name             TEXT NOT NULL,
  encrypted_key    BYTEA NOT NULL,         -- AES-256-GCM 密文
  key_fingerprint  TEXT NOT NULL,          -- HMAC（服务端密钥参与），仅去重/日志关联
  max_concurrency  INTEGER NOT NULL DEFAULT 0,  -- 0 = 不限
  enabled          BOOLEAN NOT NULL DEFAULT TRUE,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(channel_id, name),
  UNIQUE(channel_id, key_fingerprint)
);

-- 渠道（上游供应商）
CREATE TABLE channels (
  id           BIGSERIAL PRIMARY KEY,
  provider     TEXT UNIQUE NOT NULL,
  name         TEXT NOT NULL,
  balance_type TEXT NOT NULL DEFAULT 'balance',  -- balance(余额) | quota(余量百分比)
  balance_url  TEXT NOT NULL DEFAULT '',
  models_url   TEXT NOT NULL DEFAULT '',
  enabled      BOOLEAN NOT NULL DEFAULT TRUE,
  preset       JSONB NOT NULL DEFAULT '{}',     -- {endpoints:{proto:url}, pricing:{model:{...}}}（面板可视化）
  auth_mode    TEXT NOT NULL DEFAULT 'bearer',  -- 上游认证 header：bearer(默认) | x_api_key
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 模型（客户端模型 → 路由 + 价格），挂在渠道下
CREATE TABLE models (
  id                BIGSERIAL PRIMARY KEY,
  channel_id        BIGINT REFERENCES channels(id) ON DELETE CASCADE,
  name              TEXT NOT NULL,               -- 客户端模型名
  routes            JSONB NOT NULL DEFAULT '{}', -- {"/v1/messages": {upstream,model,usage}, ...}
  input_per_m       NUMERIC(12,6) NOT NULL DEFAULT 0,  -- 元/M
  output_per_m      NUMERIC(12,6) NOT NULL DEFAULT 0,
  cache_read_per_m  NUMERIC(12,6) NOT NULL DEFAULT 0,
  cache_write_per_m NUMERIC(12,6) NOT NULL DEFAULT 0,
  enabled           BOOLEAN NOT NULL DEFAULT TRUE,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(channel_id, name)
);
```

**关键不变量**：

- `keys.quota_used` 由网关 `AddQuotaUsed(key_hash, cost)` 累加（写库旁路）。
- `usage_logs.cost` 已用模型配置的价格表算好（`metering.Cost(usage, pricing)`）。
- `channels` 删除时 `models` 级联删除；`keys` 删除时 `usage_logs.key_id` 置 NULL（保留历史）。
- `usage_logs.protocol` 是字符串枚举（anthropic / responses / chat_completions），对应 `metering/meter.go` 里 `NewMeter()` 注册的三个 extractor。

## 8. key 管理（全对称加密）

两类 key，两条生命周期：

|          | 虚拟 key（发给 agent）  | 上游 key（真钱）       |
| -------- | ----------------------- | ---------------------- |
| 数量     | 每 agent 一个，增删频繁 | 2 个，极少动           |
| 存储     | PG 存 SHA-256 哈希      | PG 存 AES-256-GCM 密文 |
| 泄露后果 | 只能访问网关，可吊销    | 别人直接花你的钱       |

**虚拟 key 生命周期**（`internal/keys/keys.go`）：

1. **签发**：`"gw-" + rand(32B)` → 存 SHA-256 哈希，明文只返回一次（GitHub token 式）。
2. **校验**：每次请求 Bearer → SHA-256 → 查表（O(1)）。
3. **额度**：pre-call 硬挡（`quota_used >= quota_limit` → 401/403），post-call 累加（cost 加到 `quota_used`）。
4. **吊销/轮换**：吊销 = `enabled=false`（保留历史）；轮换 = 禁用旧 + 签发新（明文仅返回一次）。
5. **权限**：`allowed_models` 逗号分隔，空 = 不限；网关 `key.CanAccessModel(name)` 检查。

**上游 key 生命周期**（`internal/keys/crypto.go` + `cmd/gateway/main.go` `loadUpstreamKeys`）：

1. **录入**（两种方式）：
   - CLI：`gateway keys set-upstream --provider <deepseek|minimax>`（交互输入，AES-GCM 加密写 PG）。
   - API：`POST /api/channels/{provider}/key`（明文请求，AES-GCM 加密写 PG + 更新内存缓存）。
2. **解密**：启动时 `loadUpstreamKeys()` 解密一次缓存到 `upstreamKeys map[provider]string`，运行时直接用。
3. **回退**：若 PG 里没读到该 provider，回退到 env `DEEPSEEK_API_KEY` / `MINIMAX_API_KEY`（开发友好）。

**管理 API 认证**（`requireAuth`）：

- 所有 `/api/*` 需 Bearer = `PANEL_PASSWORD`（env 注入，subtle.ConstantTimeCompare 防时序攻击）。
- 留空 = 无认证（仅开发；启动日志会 `⚠️` 警告）。

**安全模型（全对称，不引入公私钥）**：

| 目标          | 方案                                                                   |
| ------------- | ---------------------------------------------------------------------- |
| 上游 key 保密 | AES-256-GCM 加密存 A 机 PG                                             |
| master key    | 32 字节对称 key（`openssl rand -hex 32`），B 机 env，唯一落 env 的秘密 |
| 虚拟 key      | SHA-256 哈希，明文签发露一次                                           |
| 管理 API 认证 | `PANEL_PASSWORD` 对称口令                                              |
| 分层防御      | A 机安全组只信任 B，B 唯一公网入口                                     |

## 9. 已验证结论（curl 实测 2026-08-14）

| #   | 项                                                | 结论                                                                                                                                               |
| --- | ------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1   | DeepSeek anthropic/responses 的 upstream model 名 | ✅ `deepseek-v4-pro` / `deepseek-v4-flash` 直接接受                                                                                                |
| 2   | MiniMax `/responses` 端点                         | ✅ `/v1/responses`（在 /v1 下；根路径 `/responses` 才 404）                                                                                        |
| 3   | 流式 usage 字段路径                               | ✅ anthropic `message_start`+`message_delta`；responses `completed`/`incomplete` 最后一个事件                                                      |
| 4   | usage 字段语义                                    | ✅ 实测印证 litellm：anthropic `input_tokens` 不含缓存（顶层 cache 字段）；responses `input_tokens` 含缓存（`input_tokens_details.cached_tokens`） |
| 5   | thinking/reasoning 透传                           | ✅ anthropic 有 `thinking` 块、responses 有 `reasoning` 块，纯透传原样保留                                                                         |

## 10. 部署

- **Go 网关**：`CGO_ENABLED=0 go build -o bin/gateway ./cmd/gateway` → ~10MB 单二进制，systemd `gateway.service` 跑 B 机前台（监听 `:8080`）。
- **Caddy**：TLS + 反代 `/v1, /api, /` → `:8080`（可选地 Caddy 直接 serve 静态面板）。
- **管理控制台**：`web/dist/`（Vite 构建产物），由网关内置 `http.FileServer` serve（或 Caddy 直接 serve）；源码在 `web/`。
- **配置**：`gateway/config.yaml`（首次种子导入）+ PG `channels`/`models` 表（运行时）+ 环境变量（`DATABASE_URL` / `GATEWAY_MASTER_KEY` / `PANEL_PASSWORD` / 上游 key 回退）。
- **迁移**：LiteLLM 下线，Go 网关接 A 机 PG 的独立库 `gateway`（不动 litellm 旧库）。`gateway/schema.sql` 一把导入。

## 11. 代码量

| 模块                           | 实际行数        |
| ------------------------------ | --------------- |
| `cmd/gateway/main.go`          | 1261            |
| `internal/store/pg.go`         | 494             |
| `internal/metering/meter.go`   | 95              |
| `internal/metering/usage_*.go` | 3 个 extractor  |
| `internal/keys/keys.go`        | 153             |
| `internal/keys/crypto.go`      | 72              |
| `internal/config/config.go`    | 104             |
| `internal/store/store.go`      | 133（接口定义） |
| `cmd/migrate` + `cmd/dbclean`  | 维护工具，按需  |
