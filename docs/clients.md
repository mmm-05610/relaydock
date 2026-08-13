# 客户端接入

> 网关：`http://115.29.241.36:4000`
> Anthropic 协议走 `/v1/messages`，OpenAI 协议走 `/v1/chat/completions`。
> 模型命名三段式 `供应商:模型名(协议)`，见 [DECISIONS.md 决策 8](DECISIONS.md)。

## 认证：用带预算的 virtual key（不用 master key）

每个客户端一个 virtual key，月度预算 100 元，泄露损失封顶 + 可单独吊销。生成方式见 `docs/setup.md` 或 UI「Virtual Keys」。

## Claude Code（Anthropic 协议）

主对话 → DeepSeek，后台杂活 → MiniMax。**这是角色分配的核心。**

```bash
export ANTHROPIC_BASE_URL="http://115.29.241.36:4000"
export ANTHROPIC_AUTH_TOKEN="sk-你的virtual-key"
export ANTHROPIC_DEFAULT_MODEL="DeepSeek:deepseek-v4-pro(anthropic)"   # 主对话 → DeepSeek
export ANTHROPIC_SMALL_FAST_MODEL="MiniMax:MiniMax-M3(anthropic)"      # 后台杂活 → MiniMax
```

> cc-switch 里可直接加一个「自定义供应商」，模型映射：
>
> - Sonnet/Opus/Fable/默认兜底 → `DeepSeek:deepseek-v4-pro(anthropic)`
> - Haiku/Subagent → `MiniMax:MiniMax-M3(anthropic)`
> - `[1M]` 后缀（声明上下文）会被 Claude Code 剥离后才发到网关，可放心加。

### 为什么主对话整段留 DeepSeek

不要按上下文长度把长请求切给 MiniMax——那样主模型丢后段上下文、MiniMax 缺前段脉络，连贯性被切断。见 [DECISIONS.md](DECISIONS.md) 决策 4。

## OpenCode / Codex（OpenAI 协议）

```bash
export OPENAI_BASE_URL="http://115.29.241.36:4000/v1"
export OPENAI_API_KEY="sk-你的virtual-key"
# 模型名用 (openai) 套的，如 DeepSeek:deepseek-v4-flash(openai)
```

| 客户端   | 模型名建议                                                               |
| -------- | ------------------------------------------------------------------------ |
| OpenCode | `DeepSeek:deepseek-v4-pro(openai)` / `MiniMax:MiniMax-M3(openai)`        |
| Codex    | `DeepSeek:deepseek-v4-flash(openai)`（Codex 走 responses，pro 暂不支持） |

## 可用模型清单（8 个）

```
DeepSeek:deepseek-v4-flash(openai)      DeepSeek:deepseek-v4-flash(anthropic)
DeepSeek:deepseek-v4-pro(openai)        DeepSeek:deepseek-v4-pro(anthropic)
MiniMax:MiniMax-M3(openai)              MiniMax:MiniMax-M3(anthropic)
MiniMax:MiniMax-M2.7-highspeed(openai)  MiniMax:MiniMax-M2.7-highspeed(anthropic)
```

`(anthropic)` 给 Claude Code 等 Anthropic 客户端；`(openai)` 给 OpenCode/Codex 等 OpenAI 客户端。**同协议透传，避免协议转换损耗**。

## 验证

```bash
curl http://115.29.241.36:4000/v1/models -H "Authorization: Bearer sk-你的virtual-key"
# 测一条 Anthropic 协议请求
curl http://115.29.241.36:4000/v1/messages \
  -H "Authorization: Bearer sk-你的virtual-key" -H "Content-Type: application/json" \
  -d '{"model":"DeepSeek:deepseek-v4-pro(anthropic)","max_tokens":32,"messages":[{"role":"user","content":"你好"}]}'
```
