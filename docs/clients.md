# 客户端接入

> 网关：**`http://121.40.184.111`**（Caddy 入口，唯一公网）
> 协议路径：Anthropic `/v1/messages`、Responses `/v1/responses`、OpenAI `/v1/chat/completions`
> 模型名：「直白」，不再用 LiteLLM 时代的 `供应商:模型名(协议)` 三段式——协议由 URL path 决定（决策 14）
> 当前可用模型来自 PG `channels` + `models` 表（或 `gateway/config.yaml`），见 §「可用模型清单」

## 认证：用带预算的 virtual key（不用 master key）

每个客户端一个 virtual key，月度预算 100 元，泄露损失封顶 + 可单独吊销。生成方式见 [setup.md](setup.md) 或 UI「Virtual Keys」（`POST /api/keys`）。

> master key (`GATEWAY_MASTER_KEY`) 是上游 key 的 AES 解密密钥，**绝不**用于客户端认证。客户端认证走 virtual key（SHA-256 哈希）或管理 API 的 `PANEL_PASSWORD`（env 口令）。

## Claude Code（Anthropic 协议）

主对话 → DeepSeek，后台杂活 → MiniMax。**这是角色分配的核心。**

```bash
export ANTHROPIC_BASE_URL="http://121.40.184.111"
export ANTHROPIC_AUTH_TOKEN="sk-你的virtual-key"     # 或 "gw-" 前缀的新格式
export ANTHROPIC_DEFAULT_MODEL="deepseek-v4-pro"               # 主对话 → DeepSeek
export ANTHROPIC_DEFAULT_HAIKU_MODEL="minimax-m3"              # 后台杂活 → MiniMax（haiku 角色）
export CLAUDE_CODE_SUBAGENT_MODEL="minimax-m3"                 # Task 工具子代理 → MiniMax
```

> cc-switch（agent-box 里 ACS 风格的 profile 切换器）里可直接加一个「自定义供应商」：
>
> - 默认 / Sonnet / Opus / Fable 兜底 → `deepseek-v4-pro`
> - Haiku / Subagent → `minimax-m3`
> - `[1m]` 后缀（声明上下文）会被 Claude Code 客户端自己剥离后才发到网关，可放心加（网关 `cfg.FindModel(name)` 按名查表，剥离后能命中）。

### 为什么主对话整段留 DeepSeek

不要按上下文长度把长请求切给 MiniMax——那样主模型丢后段上下文、MiniMax 缺前段脉络，连贯性被切断。见 [DECISIONS.md](DECISIONS.md) 决策 4。

## OpenCode / Codex（OpenAI 协议）

**Codex 走 OpenAI Responses / Chat Completions（`/v1/responses` 或 `/v1/chat/completions`），OpenCode 一样。**

```bash
export OPENAI_BASE_URL="http://121.40.184.111/v1"
export OPENAI_API_KEY="sk-你的virtual-key"
# 模型名用直白的（path 决定走哪个 upstream + protocol）
# 例如：deepseek-v4-pro / deepseek-v4-flash / minimax-m3
```

| 客户端   | 模型名建议                                                |
| -------- | --------------------------------------------------------- |
| OpenCode | `deepseek-v4-pro` / `minimax-m3`                          |
| Codex    | `deepseek-v4-flash`（Codex 默认 responses，pro 暂未走通） |

## 可用模型清单（当前 PG / config.yaml 种子）

来自 `gateway/config.yaml`（首次启动种子导入到 PG 的 `channels` + `models` 表）：

| 客户端模型名        | 协议                         | 上游 model 名       | 渠道     |
| ------------------- | ---------------------------- | ------------------- | -------- |
| `deepseek-v4-pro`   | anthropic / responses / chat | `deepseek-v4-pro`   | deepseek |
| `deepseek-v4-flash` | anthropic / responses / chat | `deepseek-v4-flash` | deepseek |
| `minimax-m3`        | anthropic / responses / chat | `MiniMax-M3`        | minimax  |

> 每个模型配三条 `routes`（`/v1/messages` + `/v1/responses` + `/v1/chat/completions`），分别指向对应上游端点，纯透传。同客户端模型名可在三种协议下通用，**同协议透传避免协议转换损耗**。

> **模型名大小写注意**：客户端用 `minimax-m3`（小写，直白），网关自动替换为上游 `MiniMax-M3`（`replaceModel`）。DeepSeek 模型客户端/上游同名。

## 验证

```bash
# 1. 列模型（OpenAI 兼容格式，按 virtual key 权限过滤）
curl http://121.40.184.111/v1/models -H "Authorization: Bearer sk-你的virtual-key"

# 2. 测一条 Anthropic 协议请求
curl http://121.40.184.111/v1/messages \
  -H "Authorization: Bearer sk-你的virtual-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-pro","max_tokens":32,"messages":[{"role":"user","content":"你好"}]}'

# 3. 测一条 Responses 协议请求
curl http://121.40.184.111/v1/responses \
  -H "Authorization: Bearer sk-你的virtual-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-pro","input":"hi","max_output_tokens":32}'

# 4. 看用量面板（PANEL_PASSWORD 登录）
open http://121.40.184.111/panel
```

## 关键差异（vs LiteLLM 时代）

| 维度          | LiteLLM 时代（已废）         | 当前 Go 网关                                   |
| ------------- | ---------------------------- | ---------------------------------------------- |
| 网关 base URL | `http://115.29.241.36:4000`  | `http://121.40.184.111`（Caddy 入口）          |
| 端口          | 4000（直连 LiteLLM）         | 80/443（Caddy 反代到网关 :8080）               |
| 模型名格式    | `供应商:模型名(协议)` 三段式 | 直白（`deepseek-v4-pro` / `minimax-m3`）       |
| 协议选择      | 编码在 model 名后缀          | 由请求 URL path 决定（`/v1/messages` 等）      |
| 模型列表      | 启动从 `config.yaml` 读      | 启动从 PG 读（`config.yaml` 仅作种子）         |
| 改模型/渠道   | 改 YAML + 重启               | 面板「渠道」页改，热更新（`reloadChannels()`） |
