# 实测：DeepSeek 涨价前，我把 39% 的 token 路由到了订阅制模型

> 一次长任务实验，验证「LiteLLM 网关 + Claude Code 自带的 subagent 配置」能不能把杂活从按量付费的模型，转移到订阅制的便宜模型。

---

## 起因

8 月 13 日，DeepSeek 宣布 8 月 17 日起调价，采用峰谷定价，**高峰时段（9–12 点、14–18 点）V4-Pro 缓存命中输入涨 12 倍，输出涨 4.5 倍**，综合下来高峰时段价格是原来的 4–5 倍，个别档位最高 11 倍。

我一个月 DeepSeek API 消费 200 出头，涨价后估计要 600 以上，扛不住。

手里另有一样东西：MiniMax 的 Coding Plan 订阅，119 元/月，专门用来跑杂活，量对我基本用不完。

我想测试这样一个场景：在一个 Claude Code 会话里，跑一个连续完整的长任务，包含搜索、决策、编码、测试等一系列工作。这个过程里，把后台杂活路由到 MiniMax，主对话留在 DeepSeek，看能省下多少 DeepSeek 的 token。

具体做法：搭了个 LiteLLM 网关，把 DeepSeek 和 MiniMax 两个模型接进去，给 Claude Code 设了两个模型参数——后台小任务和子代理走 MiniMax，主对话走 DeepSeek。

---

## 实验步骤

实验任务：从零做一个「agent 框架元数据 registry」的开源包（`agentframe-sdk`）——一个可 `pip install`、能通过 `frames["hermes"].prompt_file` 拿到 agent 结构信息的 Python 包。下面三个阶段就是做这个包的过程。

**1. 配置 LiteLLM 网关**。服务器上自建 LiteLLM，配两个上游模型，用三段式命名区分：

- `DeepSeek:deepseek-v4-pro(anthropic)` → 指向 DeepSeek 官方 Anthropic 端点
- `MiniMax:MiniMax-M3(anthropic)` → 指向 MiniMax 官方 Anthropic 端点

**2. Claude Code 接入**。设 `ANTHROPIC_BASE_URL` 指向网关，`ANTHROPIC_AUTH_TOKEN` 用网关发的 virtual key。

**3. 设三个模型参数**：

```
ANTHROPIC_MODEL               = DeepSeek:deepseek-v4-pro(anthropic)   # 主模型
ANTHROPIC_DEFAULT_HAIKU_MODEL = MiniMax:MiniMax-M3(anthropic)         # 后台小任务
CLAUDE_CODE_SUBAGENT_MODEL    = MiniMax:MiniMax-M3(anthropic)         # 子代理
```

**4. `/goal` 跑第一阶段（决策）**。任务是「调研 8 个 agent 的真实结构，产出 5-10 种产品方案」。跑完产出 7 种方案，我从中挑了 B、C、E 三个。

**5. `/goal` 跑第二阶段（聚合体设计）**。任务是「把字段调研扩充到 15 个 agent，把 B/C/E 三方案融合成一个聚合体架构」。跑完产出字段调研、聚合体方案、一个可跑原型。

**6. `/goal` 跑第三阶段（实现）**。任务是「把聚合体方案实现成一个可安装的 Python 包」。跑完 16 个测试全绿。

三个阶段跑完，从 LiteLLM 拉消耗数据，得到下面的 38.9%。

---

## 消耗数据

本次实验的 token 增量（结束减开始），只列实验用到的两个模型：

| 模型            | 缓存读     | 未命中输入 | 缓存命中率 | 输出    | 总 token   | 占比      |
| --------------- | ---------- | ---------- | ---------- | ------- | ---------- | --------- |
| DeepSeek v4-pro | 36,450,688 | 857,064    | 97.7%      | 352,473 | 37,660,225 | 61.1%     |
| MiniMax M3      | 22,200,181 | 1,549,460  | 93.5%      | 243,270 | 23,992,911 | **38.9%** |
| **合计**        | 58,650,869 | 2,406,524  | 96.1%      | 595,743 | 61,653,136 | 100%      |

MiniMax 吃掉了 **38.9%** 的 token，说明这两个模型参数确实生效了——token 不是全走主模型 DeepSeek。

## 花费对比

按 DeepSeek 8-17 涨价后的**高峰价**重算同一批 token，三个场景：

| 场景                                   | 花费                                  |
| -------------------------------------- | ------------------------------------- |
| **全用 DeepSeek v4-pro**               | **55.35 元**                          |
| **MiniMax 部分改用 DeepSeek v4-flash** | **37.23 元**                          |
| **现状（pro 按量 + MiniMax 订阅）**    | **28.17 元**（仅 pro 按量，订阅另计） |

计算口径（高峰价，元/百万 token）：

| 模型              | 缓存读 | 未命中输入 | 输出 |
| ----------------- | ------ | ---------- | ---- |
| DeepSeek v4-pro   | 0.30   | 9.0        | 27.0 |
| DeepSeek v4-flash | 0.10   | 3.0        | 9.0  |

- **全用 pro**：58.65M×0.30 + 2.41M×9 + 0.60M×27 ≈ 55.35 元
- **MiniMax 改 flash**：pro 那 37.66M 按 pro 价（≈28.17）+ MiniMax 那 23.99M 按 flash 价（≈9.06）= 37.23 元
- **现状**：pro 那 37.66M 按 pro 价 ≈ 28.17 元；MiniMax 那 23.99M 走订阅，不按量计费（订阅费 119 元/月固定）

三个场景里，全用 pro 最贵。「MiniMax 改 flash」和「现状」的差别只在计费方式：flash 是按量付费（这批杂活量按 flash 价约 9 元），MiniMax 订阅是每月 119 元固定。所以不能说订阅一定比 flash 便宜——只有当日常后台杂活的消耗量足够大、把订阅费摊薄到比 flash 按量还低时，用 MiniMax 订阅替代 flash 处理后台任务才更划算。

> 说明：28.17 元是「现状」里 DeepSeek pro 那部分的涨价后高峰价。实际我现在跑的是涨价前旧价（pro 缓存读 0.025 元/百万），所以当下这一部分还没这么贵，但 8-17 之后就是这个数。
