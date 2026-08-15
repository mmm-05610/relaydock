# 架构设计 v2：自建 Go 透传网关（定稿）

> 替代 LiteLLM。本文件是 v2 的完整设计蓝图。落地后回填 DECISIONS.md（决策 12）。

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
2. **前后端两个服务**：一个 Go 单二进制（后端）+ 一个静态前端面板。不再有 litellm/spend-dashboard/navpage 三个子服务。
3. **复用 A 机 PG**：用量、key、spend 数据存 A 机 PostgreSQL（已是纯数据节点）。
4. **单二进制零依赖**：Go 编译成 ~20MB 二进制，无运行时依赖，B 机直接跑。

## 3. 总体架构

```
                    ┌─────────────────────────────────────────┐
                    │  B 机（2核2G · 公网入口 · 可备案）         │
  agent ──HTTPS──▶  │  Caddy（:443 TLS + 反代）                │
  Claude Code       │   ├─ /            → 前端面板（静态站）      │
  Codex             │   ├─ /v1/*        → Go 网关（透传）        │
  OpenCode          │   └─ /api/*       → Go 网关（管理 API）    │
  Hermes            │                                          │
                    │  Go 网关（单二进制 :8080）                  │
                    │   ├─ 透传 /v1/messages /v1/responses      │
                    │   ├─ 认证 + 额度硬挡                       │
                    │   ├─ 计量（读 usage → PG，旁路）            │
                    │   └─ 管理 API /api/keys                   │
                    └───────────────┬──────────────────────────┘
                                    │ 内网 <1ms
                    ┌───────────────▼──────────────────────────┐
                    │  A 机（2核2G · 纯数据节点 · 不对外）         │
                    │  PostgreSQL :5432（安全组只信任 B 机 IP）   │
                    │   ├─ upstream_keys（加密）                 │
                    │   ├─ keys（哈希 + 额度）                   │
                    │   └─ usage_logs                           │
                    └──────────────────────────────────────────┘
                                    │ 公网
                    ┌───────────────▼──────────────────────────┐
                    │  上游供应商（国内 API）                     │
                    │  DeepSeek  /anthropic  /responses         │
                    │  MiniMax   /anthropic  /responses         │
                    └──────────────────────────────────────────┘
```

**分层**（延续三层，服务层从 LiteLLM 换成 Go 网关）：

| 层     | 位置 | 内容                                             |
| ------ | ---- | ------------------------------------------------ |
| 数据层 | A 机 | PostgreSQL（3 张表）                             |
| 服务层 | B 机 | Go 网关（透传 + 认证 + 计量 + 管理 API）         |
| 展示层 | B 机 | 前端面板（静态站，导航 + 用量 + key 管理三合一） |
| 入口层 | B 机 | Caddy（TLS + 反代，唯一公网入口）                |

## 4. 组件设计

### 4.1 Go 网关（后端，单二进制）

语言 **Go**，框架标准库 `net/http`（Go 1.22+ `ServeMux` 方法路由），依赖仅 `pgx`。

包结构：

```
cmd/gateway/main.go        — 入口
internal/
  config/                  — config.yaml 解析（model 路由 + 价格）
  proxy/                   — 透传（含流式监听 usage）
  keys/                    — key 管理（虚拟 key + 上游 key + 额度）
    keys.go / upstream.go / quota.go / crypto.go
  metering/                — 计量（读 usage + 成本）
    meter.go / usage_anthropic.go / usage_responses.go / cost.go / fallback.go
  store/                   — PG 访问
```

四个职责：透传网关、认证、计量、管理 API。

### 4.2 前端面板（静态站）

整合 navpage（导航）+ spend-dashboard（用量）+ key 管理 UI，三合一。纯静态 HTML/CSS/JS，延续玻璃拟态 + Swiss 排版（navpage 已有 design.md）。Caddy 直接 serve，`fetch` 调 `/api/*`。

### 4.3 Caddy（入口）

| 路径     | 反代到              |
| -------- | ------------------- |
| `/`      | 前端静态站          |
| `/v1/*`  | Go 网关（透传）     |
| `/api/*` | Go 网关（管理 API） |

### 4.4 PostgreSQL（数据层）

保留 A 机 PG，Go 网关内网连接。schema 见 §7。

## 5. 协议路由（协议无关 + 渠道管理）

**转发层协议无关**：网关不认识任何协议，只做「认证 → 按 model 查路由 → 原样转发」。协议这个概念只存在于两处：渠道的模型路由条目、计量的 usage 提取器。

```
任意 /v1/* 路径 + body.model
→ 认证 → 查 model 的 routes，匹配 client_path
→ 拿到 upstream 完整 URL + upstream_model + usage_protocol
→ 原样转发（换 base_url + 上游 key + 改 model 名）
```

**渠道 = 上游供应商 → 模型（多协议路由 + 价格）**。配置存 **PG**（channels + models 表），面板增删改 + 热更新，config.yaml 退化为首次部署的种子数据。

**config 结构**（渠道为中心）：

```yaml
channels:
  - provider: deepseek # 渠道唯一标识
    name: DeepSeek
    balance_type: balance # 余额查询：balance(余额) | quota(余量百分比)
    balance_url: https://api.deepseek.com/user/balance
    models_url: https://api.deepseek.com/models # 拉取模型列表的接口
    models: # 该渠道的模型
      - name: deepseek-v4-pro # 客户端 model 名
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
        pricing: {
            input_per_m: 3,
            output_per_m: 6,
            cache_read_per_m: 0.025,
            cache_write_per_m: 3,
          } # 元/M
```

**关键设计**：

- `upstream` 存完整路径（不拼接，各供应商路径结构不同）。
- **一个模型多协议**：同客户端模型名配多条协议路由，纯透传无损。别的中转站靠格式转换（Anthropic↔OpenAI）实现多协议，转换有损（丢 thinking/reasoning）——这是自建网关的核心优势。
- **价格用人民币（元/M）**：DeepSeek 官方价（pro 3/6、flash 1/2），MiniMax 官方五折价（M3 2.1/8.4）。
- **缓存是独立单价**（cache_read_per_m / cache_write_per_m），不是写死倍数（DeepSeek 缓存读 0.025/0.02，不是输入的 10%）。
- 加新供应商 = 渠道表加一行 + 前端 `PROVIDER_BASE`/`PROVIDER_PRICING` 映射加一条。

## 6. 核心流程

### 6.1 透传链路

```
1. 读请求 body（JSON，含 model 字段）
2. 认证：Bearer → SHA-256 → 查 keys 表（无效 401）
3. 硬挡：quota_used >= quota_limit → 403
4. 路由：body.model → 查 config；path → 匹配 routes 的 client_path，得上游 URL + upstream_model + usage_protocol
5. 解密：上游 key（启动时已解密缓存内存）
6. 改 body.model = upstream_model（若不同）
7. 构造上游请求：替换 base_url + Authorization，其余 header/body 原样
8. 转发，流式（SSE）逐块吐回客户端，边转发边监听 usage 事件
9. 计量：defer 落库（旁路，不阻断）
```

### 6.2 计量（读 usage + fallback，旁路）

**总纲：计量是旁路，不阻断、不延迟主链路；计量失败 = 少记一笔，请求照常。**

上游每次返回都自带 token 用量（字段 `usage`），网关不用自己数 token：

```json
{ "usage": { "input_tokens": 1500, "output_tokens": 800 } }
```

**归一化 Usage 结构**（学自 litellm）：每协议一个 extractor，把原始 usage 拍平成统一结构，成本只看统一结构。

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

**字段语义坑**（必须每协议一个 extractor 的原因）：不同协议 usage 语义不同——

```json
// Anthropic：input_tokens 是"纯输入"，缓存是顶层独立字段
{ "input_tokens": 500, "cache_creation_input_tokens": 1000, "cache_read_input_tokens": 500 }

// Responses：input_tokens 是"总输入"，缓存藏在 details 里，字段名也不同
{ "input_tokens": 1500, "input_tokens_details": { "cached_tokens": 500, "cache_write_tokens": 0 } }
```

所以 anthropic 的 extractor：`InputTokens = input_tokens`（已不含缓存）；responses 的 extractor：`InputTokens = input_tokens - cached - cache_write`。归一化后 cost.go 统一算，不碰协议。所有字段 `or 0` 兜 null（litellm 同款）。

**三段编排**：

- **① pre-call 估 input**：`EstimateInput(body)` 字符/3，兜底流式中断也不丢 input 账。
- **② 转发中监听**：`bufio.Scanner` 逐行 `Write`+`Flush` 透传，同时 `parseUsageEvent` 识别 usage 事件：

  | 协议             | 流式 usage 事件                                      | 内容                                    |
  | ---------------- | ---------------------------------------------------- | --------------------------------------- |
  | Anthropic        | `message_start`                                      | input_tokens + cache                    |
  | Anthropic        | `message_delta`（最后）                              | output_tokens                           |
  | Responses        | `response.completed` / `response.incomplete`（最后） | 完整 usage（实测截断时是 `incomplete`） |
  | chat_completions | 默认不带，需 `stream_options.include_usage`          | 流式只记 input 估算                     |

  （anthropic 拆两个事件拼；responses 读最后一个事件，`completed` 和 `incomplete` 都带 usage。）

- **③ post-call 落库**：`defer Record(...)`，算 cost + 写 usage_logs + 累加 quota_used。

**成本计算**（cost.go）：

```
cost = input/1M × 输入价
     + cache_read/1M  × 输入价 × 0.1    // 缓存命中
     + cache_write/1M × 输入价 × 1.25   // 缓存创建
     + output/1M × 输出价
```

**优先级铁律**：有 usage 用 usage（100% 准），没 usage 才用估算（±10-20%）。

**边界**：流式中断（input 估算保底 + 打 unmetered）；上游 4xx/5xx（有 usage 就读，没有记 0）；计量/写库失败（只 log，不阻断）。

## 7. 数据模型（PG schema，5 张表）

```sql
-- 渠道（上游供应商）
CREATE TABLE channels (
  id           BIGSERIAL PRIMARY KEY,
  provider     TEXT UNIQUE NOT NULL,
  name         TEXT NOT NULL,
  balance_type TEXT NOT NULL DEFAULT 'balance',  -- balance(余额) | quota(余量百分比)
  balance_url  TEXT NOT NULL DEFAULT '',
  models_url   TEXT NOT NULL DEFAULT '',          -- 拉取模型列表的接口
  enabled      BOOLEAN NOT NULL DEFAULT TRUE,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 模型（客户端模型 → 多协议路由 + 价格），挂在渠道下
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

-- 上游真 key：AES-256-GCM 密文，master key 解密
CREATE TABLE upstream_keys (
  id              BIGSERIAL PRIMARY KEY,
  provider        TEXT NOT NULL UNIQUE,
  encrypted_key   BYTEA NOT NULL,         -- nonce||ciphertext(GCM tag)
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 虚拟 key（发给 agent）：只存 SHA-256 哈希
CREATE TABLE keys (
  id             BIGSERIAL PRIMARY KEY,
  key_hash       TEXT UNIQUE NOT NULL,      -- SHA-256，不存明文
  name           TEXT NOT NULL,
  owner          TEXT,                      -- 多用户归属
  agent_type     TEXT,
  quota_limit    NUMERIC(12,6),             -- 额度（元），NULL = 不限
  quota_used     NUMERIC(12,6) NOT NULL DEFAULT 0,
  enabled        BOOLEAN NOT NULL DEFAULT TRUE,
  allowed_models TEXT NOT NULL DEFAULT '',  -- 允许访问的模型（逗号分隔，空=不限）
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 用量日志
CREATE TABLE usage_logs (
  id                BIGSERIAL PRIMARY KEY,
  key_id            BIGINT REFERENCES keys(id),
  model             TEXT NOT NULL,
  upstream_model    TEXT,
  protocol          TEXT NOT NULL,         -- anthropic | responses | chat_completions
  input_tokens      INTEGER,
  output_tokens     INTEGER,
  cache_read_tokens  INTEGER,
  cache_write_tokens INTEGER,
  cost              NUMERIC(12,6),         -- 元
  latency_ms        INTEGER,
  status            INTEGER,               -- HTTP 状态码（200/401/404/...）
  error             TEXT,                  -- 失败原因（空=成功）
  request_id        TEXT,                  -- 客户端请求 ID
  unmetered         BOOLEAN NOT NULL DEFAULT FALSE,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON usage_logs (key_id, created_at DESC);
```

## 8. key 管理（全对称加密）

两类 key，两条生命周期：

|          | 虚拟 key（发给 agent）  | 上游 key（真钱）       |
| -------- | ----------------------- | ---------------------- |
| 数量     | 每 agent 一个，增删频繁 | 2 个，极少动           |
| 存储     | PG 存 SHA-256 哈希      | PG 存 AES-256-GCM 密文 |
| 泄露后果 | 只能访问网关，可吊销    | 别人直接花你的钱       |

**虚拟 key 生命周期**：

1. **签发**：`"gw-" + rand(32B)` → 存 SHA-256 哈希，明文只返回一次（GitHub token 式）。
2. **校验**：每次请求 Bearer → SHA-256 → 查表（O(1)）。
3. **额度**：pre-call 硬挡（quota_used >= quota_limit → 403），post-call 累加（cost 加到 quota_used）。
4. **吊销/轮换**：吊销 = enabled=false（保留历史）；轮换 = 禁用旧 + 签发新。

**上游 key 生命周期**：

1. **录入**：`gateway keys set-upstream --provider deepseek` 交互输入（终端不回显），AES-GCM 加密写 PG。
2. **解密**：启动时解密一次缓存内存，运行时直接用。

**管理 API**（`/api/*`，master key 口令认证）：

```
POST /api/keys              创建（返回明文一次）
GET  /api/keys              列出（JOIN usage_logs 汇总用量）
POST /api/keys/:id/revoke   吊销
POST /api/keys/:id/rotate   轮换
POST /api/upstream-keys     录入上游 key
```

**安全模型（全对称，不引入公私钥）**：

| 目标          | 方案                                                                   |
| ------------- | ---------------------------------------------------------------------- |
| 上游 key 保密 | AES-256-GCM 加密存 A 机 PG                                             |
| master key    | 32 字节对称 key（`openssl rand -hex 32`），B 机 env，唯一落 env 的秘密 |
| 虚拟 key      | SHA-256 哈希，明文签发露一次                                           |
| 管理 API 认证 | master key 口令（对称）                                                |
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

- **Go 网关**：`CGO_ENABLED=0 go build` → ~20MB 单二进制，systemd/Docker 跑 B 机。
- **前端面板**：静态文件，Caddy 托管（B 机）。
- **配置**：`config.yaml`（路由表 + 价格）+ 环境变量（`GATEWAY_MASTER_KEY` / `DATABASE_URL`）。
- **迁移**：LiteLLM 下线，Go 网关接 A 机 PG（新建独立库 `gateway`，不动 litellm 旧库）。

---

## 附：工作量估算

| 模块                                | 估计               |
| ----------------------------------- | ------------------ |
| 透传反代（含流式 SSE + usage 监听） | ~150 行            |
| 认证 + 额度 + key 管理              | ~100 行            |
| 计量（读 usage + 成本 + 缓存计价）  | ~100 行            |
| 管理 API                            | ~100 行            |
| AES-GCM + SHA-256 加密              | ~30 行             |
| config 解析 + 路由                  | ~80 行             |
| 前端面板（玻璃拟态三合一）          | 静态站，看设计投入 |
| **合计**                            | Go 后端 ~560 行    |
