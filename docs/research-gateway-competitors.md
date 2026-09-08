# 竞品调研：LLM API 网关的上游凭据池与强化方向

> 调研日期：2026-09-02。方法：浅克隆源码本地逐文件阅读 + 官方文档交叉核对；每条关键结论标注 `[源码验证]` / `[仅文档]` / `[未验证·推断]`。
> 本轮只做调研与设计，不改业务代码。配套落地设计见 [design-upstream-account-pool.md](design-upstream-account-pool.md)。

---

## 1. 执行摘要（含必答 10 问）

调研了 11 个项目，其中 8 个深入到源码级（uni-api、gpt-load、aiproxy、one-api、new-api、Bifrost、Portkey、LiteLLM 的 router/cooldown 核心），3 个到配置/文档级（Envoy AI Gateway、APISIX、Higress）。**最重要的发现**：

- 所有成熟项目的账号池本质都是同一个三层结构：**健康过滤 → 排序选择 → 失败后换凭据重试**，没有一家用了复杂组件（Redis 除外，且都只为多实例准备）。
- 凭据失败处理收敛为一个共识分类：**429 = 短冷却（尊重 Retry-After）；401/403/402 = 停用（等待验证或人工）；5xx/网络 = 连续失败计数 + 熔断；400/404/413 = 客户端问题，不惩罚凭据**。
- 重试边界高度一致：**只要还没向客户端写出任何字节，就可以换凭据重试；写出第一字节（含 SSE 首事件）之后绝不透明切换**。
- 单机实现全部是内存计数器（原子变量/互斥锁），Redis/PG 只用于多实例协调和持久化审计，**不在热路径上**。

### 必答 10 问

**Q1：当前 Go 网关下一步最值得做的 5 项能力？**
1. **上游凭据池 MVP**（同名模型多渠道 + 每渠道并发槽 + 最少在途数选路 + 429 冷却 + 换渠道重试）——直接解决两个 OpenCode Go 订阅各限 2 并发、下游 4 并发会打爆单账号的问题。
2. **传输层加固**：`http.DefaultClient` 无超时、上游请求未绑定 `r.Context()`（客户端断开不取消上游）、SSE scanner 1MB 上限、无 body 大小上限、无优雅停机——这是任何失败重试/切换逻辑的前置条件，否则一个挂死的上游连接会拖垮整个切换链。
3. **配置热更新一致性**：`reloadChannels()` 全量替换 `cfg.Channels` 无锁，与读路径存在数据竞争（DECISIONS.md 待验证项 1）——换 `atomic.Pointer[Config]` 快照，S 级工作量。
4. **请求归因可观测**：`usage_logs` 加 `provider` 列 + 尝试次数 `attempts`，日志行带选路结果——没有它，账号池是否均衡、哪个账号在冷却完全不可见。
5. **管理操作审计日志**：渠道/上游 key/面板口令的变更目前无痕迹，加一张 `audit_logs` 表 + 写入钩子，S 级。

**Q2：并发账号池是否第一优先级？为什么？**
是（P0），但要和传输加固捆绑落地。理由：它是唯一同时解决「账号并发利用率」（两个 Go 订阅只能用到第一个渠道）和「稳定性」（429/5xx 无自动切换，客户端直接吃错误）的能力；其余能力（观测、审计）都依赖它产生归因数据。但它不能单独上线——没有超时和上下文传播，重试链会被挂死请求放大。

**Q3：第一版账号池是否需要 Redis？**
不需要。所有单机竞品（uni-api、gpt-load 内存模式、aiproxy 内存模式）热路径都是内存原子变量/互斥锁；Redis 只出现在多实例协调（gpt-load 的 active key 列表、aiproxy 的 Lua 原子计数）。我们单实例 systemd 进程，PG 只需存「人工禁用」这类跨重启状态，冷却/熔断/在途数全在内存，重启清零是可接受的（冷却状态丢失的代价只是多试错一次）。

**Q4：第一版应该选择哪种调度算法？**
**最少在途请求数（least-in-flight / least-connections）为主，优先级分层 + 权重为辅**。理由：coding agent 的请求时长极不均匀（流式长会话 vs 1 秒小请求），轮询会把长请求堆在同一账号上；uni-api 的 fixed_priority/round_robin、gpt-load 的 weighted RR 都没有 in-flight 感知，而 LiteLLM 的 least-busy 正是为此设计。2 账号 × 2 并发的场景下，least-in-flight 天然产生 2+2 均衡。权重作为平局打破与容量修正（`inflight/weight` 取最小）。

**Q5：并发槽、RPM、TPM、余额和健康状态应如何组合决策？**
分层过滤 + 排序，不做复合公式：
```
候选 = 同名模型的启用渠道
过滤①硬容量：inflight < max_concurrency（信号量，无槽不选）
过滤②健康：不在冷却期 / 熔断器未打开
排序③：优先级层（高优先级层有空槽绝不落到低层）→ inflight/weight 最小
排序④（P2 可选）：剩余额度多者优先、EWMA 延迟低者优先
重试⑤：失败后剔除该渠道再选，受重试预算约束
```
RPM/TPM 第一版不做：上游 429 + 冷却已经是反应式限流，足以自洽（LiteLLM 的 TPM 预检复杂度高，收益在几百 deployment 规模才体现）。

**Q6：是否应该做 sticky routing？粒度？**
应该做，但作为 **P1/P2 的软偏好**，粒度是 **prompt 前缀（≈会话）**，不是 virtual key 也不是单请求。gpt-load v2 的 affinity 包给了最贴切的形态：`HMAC(accessKeyID, protocol, prompt-prefix) → credential`，进程内缓存，选到偏好凭据就用、不在健康池里就放弃粘性（soft affinity）。理由：按 token 计费的订阅（OpenCode Go 的美元窗口制、DeepSeek 缓存计价）下，同会话粘同一账号能吃到 prompt cache，直接省额度；virtual key 粒度太粗（一个 key 可能跑多个会话），session 标识在 Anthropic/OpenAI 协议里没有统一字段，prompt 前缀哈希是唯一协议无关的锚点。故障切换优先于粘性：账号冷却时立即放弃粘性。

**Q7：哪些错误允许换 Key 重试，哪些绝对不能透明重试？**
允许（在未向客户端写出任何字节前）：网络错误、TLS 错误、超时、5xx、408、**429（先给该凭据记冷却再换）**；以及 uni-api 识别的「伪 400」——上游网关自身配置问题（模型未配价、model_not_found 类）重映射为 503 后可切。
绝对不能透明重试：**已向客户端写出响应头或任何 SSE 字节之后**（HTTP 语义上响应已开始，换上游会产生拼接错乱的响应流；Bifrost 对 first-chunk 错误的流式 fallback 是特例，它 deliberately buffer 了首块才敢切，我们第一版不引入这个复杂度）；以及 400/413（客户端请求本身的问题，换凭据无用且多花一次上游额度）。401/403/404：可换凭据一次，但同请求内不再二次尝试同渠道，并对凭据做停用处理。

**Q8：在保持"纯透传"原则下，哪些能力可以安全加入？**
凡是**只动"连接到哪个上游、何时、试几次"而不动 body/协议**的能力：凭据池选路、并发槽、冷却/熔断、重试切换、超时、上下文取消传播、header 白名单修补（现有 `applyProtocolHeaders` 模式）、计量旁路增强、审计。竞品里所有"改 body 才能实现"的能力（参数覆盖、模型重定向、协议转换、响应改写）都默认排除——gpt-load 的 ParamOverrides 是它偏离透传的地方，我们不学。

**Q9：哪些竞品能力不适合 2核2G、自用型场景？**
- **多实例协调全套**（gpt-load 的 leader/follower + pub/sub、aiproxy 的 Redis Lua 计数）：单进程单机，无意义。
- **LiteLLM 的全量 Router**（pre-call TPM/RPM 预检、retry policy per-exception DSL、routing groups）：为百级 deployment 设计，配置面大于收益。
- **OpenTelemetry 全家桶 / ClickHouse 级观测**（Helicone、TensorZero）：Bifrost 有 OTel 插件但我们连 Prometheus 都可手写 `/metrics` 免依赖解决。
- **Envoy/APISIX 数据面**：一个网关进程做不了也不该做 Envoy 的 job；我们借概念不借架构。
- **new-api 整体引入**：AGPL-3.0 且是"计费分发"定位，与 DECISIONS 决策 1 冲突。
- **主动健康检查探测池**（gpt-load 每组定时真实验证）：我们渠道数个位数，请求驱动的被动健康 + 冷却已够，主动探测留作半开恢复的一种实现。

**Q10：推荐的三阶段实施路线和每阶段验收标准？**
- **M1（账号池 MVP + 稳定性底座，~1-2 周）**：凭据池调度 + 429 冷却 + 换渠道重试 + 传输加固 + 配置快照。验收：①2 账号各限 2 并发时 4 并发下游稳定 2+2 分配；②单账号人工注入 429/5xx，请求自动切到另一账号且 usage_logs 记录 attempts=2；③`go test -race` 通过；④kill -9 后重启无锁死；⑤客户端断开时上游连接被取消（netstat/日志验证）。
- **M2（可观测 + 运维体验，~1 周）**：usage_logs.provider/attempts、面板渠道健康快照、/metrics、审计日志、上游错误文本脱敏。验收：面板能看到每账号 in-flight/冷却状态/失败计数；重复一次渠道变更有审计记录。
- **M3（进阶调度，按需）**：prompt 前缀 sticky、per-virtual-key 并发上限（公平性）、余额感知、RPM/TPM 预算、upstream_accounts 多凭据表。验收：粘性会话的缓存命中率在面板可见并提升；单 key 后台任务洪峰不挤占其他 key 的配额。

---

## 2. 当前项目能力基线

代码事实（`gateway/`，2026-09-02 阅读 main.go 1261 行 + internal/* 全部）：

| 能力域 | 现状 | 缺口 |
| --- | --- | --- |
| 路由 | `FindModel` 返回**第一个**启用渠道的同名模型（config.go:73），同名多渠道是「启停控制」语义 | 多候选调度、权重、优先级、failover 全无 |
| 凭据 | `upstream_keys.provider UNIQUE`，一渠道一 key，AES-256-GCM | 无多凭据；无状态（健康/冷却/熔断）字段 |
| 并发控制 | 无 | 无信号量、无 RPM/TPM、无排队 |
| 失败处理 | 上游错误直接透传给客户端（main.go:282 后无重试） | 无错误分类、无重试、无熔断 |
| 传输 | `http.DefaultClient`（**无任何超时**）；`http.NewRequest`（**未绑 r.Context()**）；scanner 1MB | 流式专用 client、取消传播、body 上限 |
| 热更新 | `reloadChannels()` 全量替换 `cfg.Channels`，**读写无锁** | 快照一致性 |
| 计量 | 旁路、usage 提取三协议、估算兜底——成熟 | `usage_logs` 无 provider/attempts 归因 |
| 凭据安全 | AES-GCM + master key；日志 `maskKey` 前 8 位 | 上游错误文本可能回传含 key 的 URL（gpt-load 有 RedactSecret 先例） |
| key 管理 | 额度/权限/轮换齐备 | 无预算周期重置、无 per-key 并发上限 |
| 审计 | 无 | 管理操作无痕迹 |

## 3. 候选项目筛选过程

**广泛扫描**（深度=README/文档/粗读，用于筛除）：

| 项目 | 结论 |
| --- | --- |
| one-hub（MartialBE，one-api fork） | 功能与 one-api 重叠，未深入 [仅文档] |
| Kong AI Gateway / MLflow AI Gateway | 前者企业定位、后者实验性，机制可被 APISIX/Envoy 结论覆盖 [仅文档] |
| Cloudflare AI Gateway / OpenRouter / AIHubMix | 闭源 SaaS，只能当功能参照 [标注：闭源] |
| openai-forward | Python 转发器，无池化调度，筛除 [仅文档] |
| LiteLLM enterprise 目录 | 闭源，涉及的 SSO/SPIFFE 能力不在调研范围 [标注：闭源] |

**深入源码**（8 个）+ **数据面参照**（3 个）见 §4/§5。选择标准：①真的开源了 channel/key pool 实现；②与我们同为"透传型"或有可借鉴的调度内核；③活跃。

## 4. 竞品功能矩阵

| 能力 | uni-api | gpt-load | aiproxy | one-api | new-api | Bifrost | Portkey | LiteLLM |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 多凭据/渠道 | ✅ provider 多 + 每 provider 多 key 环 | ✅ 组多 key + 组多上游端点 | ✅ 每 (model,channel) 池 | ✅ abilities 表 | ✅ 渠道多 key | ✅ provider 多 key | ✅ targets 树 | ✅ deployment 列表 |
| 选择算法 | fixed_priority/RR/随机/平滑WRR/lottery | 平滑WRR(端点)+轮换(key)+加权随机(凭据) | 时间窗错误率+封禁过滤，权重排序 | 最高优先级内随机 | random/polling | 加权随机+keyPoolFilter | 加权随机/顺序fallback | simple-shuffle默认+least-busy等5种 |
| in-flight 感知 | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ✅ least-busy |
| 并发上限/槽 | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ |
| 429 冷却+Retry-After | ✅ 30min 默认+解析 | ⚠️ 计入通用失败 | ⚠️ 计入错误率 | ⚠️ 仅重试不冷却 | ⚠️ 仅重试 | ✅ per-request 排除+池级重置 | ❌ | ✅ 5s 默认冷却 |
| 熔断器 | ✅ (provider,model) 级 | ❌（黑名单替代） | ✅ 错误率阈值+5min±抖动封禁 | ❌（禁用替代） | ❌ | ✅ keyPoolFilter/半开语义 | ✅ in-memory | ✅ allowed_fails+错误率 |
| 自动停用凭据 | ✅ 配额/永久auth分类 | ✅ 阈值3拉黑+定时验证恢复 | ✅ MaxErrorRate+NoPermissionBan | ✅ 401/配额/文本特征 | ✅ per-key 状态表 | ✅ deadKey 永久+池过滤 | ❌ | ✅ cooldown/allowed_fails |
| 流式后切换 | ❌（不可） | ❌（显式仅首字节前） | ❌ | ❌ | ❌ | ⚠️ first-chunk 特例 | ❌ | ❌ |
| sticky | ❌ | ✅ prompt 前缀 HMAC 软亲和 | ❌ | ❌ | ❌ | ❌（一致性哈希用于缓存） | ❌ | ❌ |
| 排队/背压 | ⚠️ admission(内存租约) | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ |
| TPM/RPM 预检 | ✅ TPR chars/4 估算 | ❌ | ✅ per-model 限额 | ✅ RPM 令牌桶 | ✅ | ✅ governance | ✅ per-VK | ✅ pre-call check |
| 多实例 | ❌ | ✅ Redis+leader/follower | ✅ Redis Lua | ✅ Redis SYNC_FREQUENCY | ✅ | ⚠️ | ❌ 单机 | ✅ Redis |
| 值得抄的内核 | 错误分类+请求局部环 | 状态机+failover匹配器+affinity | 错误率封禁+抖动 | abilities 索引+禁用特征 | per-key 状态表 | dead/used 错误三分法 | config DSL | 冷却参数表 |

> 惊讶点：**没有任何竞品实现 per-credential 并发上限**。这正是我们场景（订阅账号限并发数而非限 RPM）最需要的能力，必须自建。

## 5. 各项目关键实现分析

### 5.1 uni-api（yym68686/uni-api，Python，Apache-2.0，1.26k★）

结构演进最激进的小项目，路由内核完整。[源码验证：`uni_api/routing/core.py`、`routing/health.py`、`upstream/policies.py`、`rate_limit/key_pool.py`，commit main@2026-09-02]

- **建模**：`RoutingIndex.providers_by_model` —— 一个模型名 → 多 provider 的倒排索引；每个 provider 一个 `ProviderKeyPool`（多 key）。
- **调度**：`get_right_order_providers()` 产出**有序候选列表**；算法支持 `fixed_priority`（默认！按配置顺序）/`random`/`weighted_round_robin`（nginx 平滑 WRR：`current += weight; 选 ratio=current/weight 最大者; 选中者 -= total`）/`lottery`（累计权重随机）/`smart_round_robin`。
- **请求局部环**（最有价值的模式）：`RoutingPlan` 在请求开始时快照 `matching_providers` 元组 + 重试预算（`retry_count = min(Σ每provider key数×2, 10)`），`next_provider()` 按 `(start_index + index) % n` 单调前进；配置热更新只刷新 `available_provider_names`（计划∩现况），**绝不重写环**——避免执行中池子变化导致的重复/跳过。
- **key 环**：`ProviderKeyPool.next(model)` 轮询跳过 rate-limited key，全限则 429；`set_cooling(item, seconds)`；`is_tpr_exceeded(model, tokens)` 请求前用 `字符数/4` 估算做 TPM 预检；`rollback_rate_limit_record()` 响应后校正。
- **熔断**：`ProviderModelCircuitBreaker` 按 (provider, model) 记 **400/403/404**（确定性路由失败）滑动窗口，3 次/120s → 打开 300s；成功清零。429 不进熔断（由 key 冷却负责）。
- **错误分类**（`ProviderErrorClassifier`）：`remap_status_code()` 把上游伪错误重映射（上下文超长→413、无效key→401、上游配置缺价→502、model_not_found→503）——**重映射成 5xx/503 是让"可切换"语义成立的手段**；`is_retryable_rate_limit_error()` 429+文本特征；`retry_after_seconds()` 解析 "try again in X ms/s/m"；`is_quota_exhausted_error()`（insufficient_quota/billing_hard_limit…）→ 长冷却；`is_codex_permanent_auth_error()`（account_deactivated…）→ 永久停用。
- **重试策略**：`should_retry` 默认全重试，仅排除 400/413（+窄例外）；冷却默认 30min 且 `max(配置, Retry-After)`。

### 5.2 gpt-load（tbphp/gpt-load，Go，MIT，6.3k★，v2.0.0-rc）

与我们最同构的透传型网关（官方明言不做协议转换）。[源码验证：`internal/keypool/provider.go`、`channel/base_channel.go`、`proxy/server.go`、`scheduler/scheduler.go`、`affinity/`，commit 0b8ee14@2026-09-01；另有代理深度报告佐证]

- **三层选择**：上游端点（平滑 WRR，`getUpstreamURL()`）→ 凭据（Redis 列表原子轮换/内存切片轮换，`SelectKey()`）→ 聚合组（加权轮询+空池跳过）。**没有 in-flight 感知、没有 per-key 并发上限**。
- **key 状态机**：只有 `active/invalid` 两态。失败计数 ≥ `BlacklistThreshold`(3) 拉黑；**计数只增不减**（成功不重置，是它明确的设计妥协）；恢复靠 cron 每 5min tick、每组默认 60min 间隔用真实最小请求（`ValidationEndpoint` + `max_tokens=100`）并发探测 invalid key，2xx 归队。无 half-open、无冷却时长。
- **failover 匹配器**：`FailoverStatusCodes="400-403,405-999"` 区间语法+二分查找——默认**重试 400/401/402/403/429/5xx，排除 404**；无 backoff，立即换 key。
- **流式边界**：`IsStreamRequest` 在请求前判定；重试只发生在「上游返回可 failover 状态码、尚未向客户端写字节」阶段；成功开始响应后断流只记日志。流式专用 client：`RequestTimeout=0` + `DisableCompression=true`（防 transport 解压破坏 SSE 分块）+ 独立连接池；4KB read+flush 字节透传。
- **affinity**（v2 新增，`internal/affinity/key.go`）：`DeriveKey = HMAC(accessKeyID, clientProtocol, prompt-prefix)` → 进程内缓存映射到凭据 ID，调度时 `preferredCredential()` 命中且在健康池内则优先（软亲和）。
- **热更新**：三层配置合并 + `CacheSyncer[T]` 整体换指针 + Redis pub/sub 失效传播；凭据增删在**同一 DB 事务内同步写 store**，失败回滚（DB 为准）。
- **凭据加密**：PBKDF2(passphrase,固定盐,10万轮)→AES-256-GCM；`key_hash = HMAC-SHA256` 用于日志关联；错误文本回传前 `RedactSecret` 脱敏。

### 5.3 aiproxy（labring/aiproxy，Go+PG，MIT，538★）

监控驱动的渠道池。[源码验证：`core/model/channel.go`、`core/monitor/memmodel.go`、`core/monitor/model.go`，main@2026-09-02]

- **Channel 建模**：一渠道一 key，字段含 `Priority`(默认10)、`Status`(自动管理)、`UsedAmount/RequestCount/RetryCount`、`WarnErrorRate/MaxErrorRate`、`EnabledNoPermissionBan`、`BalanceThreshold`(自动余额下限停用)、`SkipTLSVerify`、`ModelMapping`。
- **监控内核**：`MemModelMonitor` 按 (model, channel) 维护时间窗统计；错误率 ≥ `MaxErrorRate` → 封禁 `banDuration = 5min ± 10% 抖动`（抖动防恢复踩踏——我们采纳）；`maxErrorRate<=0` 关闭自动封禁。
- **多实例**：Redis 版用 Lua 脚本原子 AddRequest + `:banned` 后缀 key + 本地缓存；内存版单机。管理端可查/清 banned channels。

### 5.4 one-api（songquanpeng/one-api，Go，MIT）与 new-api（QuantumNous/new-api，Go，**AGPL-3.0**）

[源码验证：one-api `model/ability.go`、`middleware/distributor.go`、`controller/relay.go`、`monitor/manage.go`；new-api `model/channel.go`、`controller/relay.go`、`setting/operation_setting/`]

- **abilities 表**是"同名模型多渠道"的标准解：`(group, model, channel_id, priority, enabled)`，`GetRandomSatisfiedChannel` 取**最高优先级层内 `ORDER BY RANDOM()`**，重试时 `ignoreFirstPriority=true` 降到次层。
- **重试**：`shouldRetry` = 429/5xx/其余4xx 重试，400/2xx 不重试。
- **自动禁用**（`ShouldDisableChannel`）：401；type ∈ {insufficient_quota, authentication_error, permission_error, forbidden}；code ∈ {invalid_api_key, account_deactivated}；消息子串（credit/balance/已欠费/api key not valid…）。429 **不禁用**。
- **new-api 增量**：渠道内多 key——`ChannelInfo.MultiKeyStatusList/DisabledReason/DisabledTime`（key 下标→状态），模式 random/polling（持久化轮询下标，跳过禁用 key）；重试状态码范围运营可配（`ShouldRetryByStatusCode`）。
- **许可警示**：new-api 为 **AGPL-3.0**——只可借鉴设计，禁止移植代码。

### 5.5 LiteLLM Router（BerriAI/litellm，Python，MIT 核心/enterprise 目录闭源）

参数表最完整的设计参考。[源码验证：`litellm/router.py`、`router_utils/cooldown_handlers.py`、`constants.py`，main@2026-09-02]

- **deployment 建模**：同 `model_name` 多 deployment（各自 api_base/api_key/rpm/tpm）。
- **策略**：默认 `simple-shuffle`；可选 `least-busy`（in-flight 最少）、`usage-based-routing-v2`（剩余 TPM/RPM 多者优先）、`latency-based-routing`、`cost-based-routing`；`enable_weighted_failover`（失败后加权重选）。
- **冷却参数**：`DEFAULT_ALLOWED_FAILS=3`、`DEFAULT_COOLDOWN_TIME_SECONDS=5`；`_is_cooldown_required`：**429/401/408/404 与全部 5xx 冷却，其余 4xx 不冷却**；v2 逻辑加「1 分钟失败率 > 50%」熔断（`ALLOWED_FAILURE_RATE_PER_MINUTE`）；`cooldown_cache` 内存 TTL 缓存，Redis 版供多实例。
- **TPM/RPM 预检**在 `router_utils/pre_call_checks/`，依赖 tokenizer 与 deployment 级计数——复杂度最高的部分，明确不学。

### 5.6 Bifrost（maximhq/bifrost，Go，Apache-2.0）

[源码验证：`core/bifrost.go`（executeRequestWithRetries 及注释）、`core/keyselectors/weightedrandom.go`、`core/streamfallback_test.go`，main@2026-09-02]

- **错误三分法**（注释明文）：429 → `usedKeyIDs`（本请求内排除，池耗尽时重置开新一轮）；**401/402/403 → `deadKeyIDs`（本请求内永不重置，"坏凭据不会因为等待变好"）**；网络/5xx → **复用同一凭据**（服务端瞬时问题，非凭据问题）。
- 池耗尽语义：全 dead → 合成 **502 `upstream_credentials_exhausted`**（避免把上游 401 误报为调用方 key 错误）；全被过滤器抑制 → **503**（瞬态，可自愈）。
- `WeightedRandom`：权重×100 整数化 + 全零退化均匀随机；`keyPoolFilter` hook 是治理插件的熔断/配额抑制点。
- `TestStreamFallbackAfterFirstChunkError` 证明其支持首 chunk 错误后的流式 fallback——依赖 buffer 首块，我们第一版不引入。

### 5.7 Portkey AI Gateway（Portkey-AI/gateway，TypeScript，MIT，v1.15.2）

[源码验证：`src/handlers/handlerUtils.ts` tryTargetsRecursively、`src/middlewares/requestValidator/schema/config.ts`]

- **config DSL**：`strategy: {mode: fallback|loadbalance|conditional|single, targets: [...]}` 递归树；fallback 按 `onStatusCodes` 判断是否继续（默认 2xx 即停）；loadbalance = **每次请求单次加权随机**；熔断状态 `isOpen` 过滤 + `handleCircuitBreakerResponse` 更新（in-memory per node）。
- 启发：策略表达力来自"递归 targets 树"，对我们等价物是「同名模型渠道列表 + 两级参数」，不需要 DSL。

### 5.8 数据面三参照（Envoy AI Gateway / APISIX / Higress）

- **Envoy AI Gateway**（envoyproxy/ai-gateway，Go，Apache-2.0）[源码验证：`api/v1alpha1/ai_gateway_route.go`、`quota_policy.go`]：`AIGatewayRoute.Timeouts` **默认 60s**（注释明言：相对 Envoy Gateway 默认 15s，LLM 首字节慢，必须放宽）——我们设 ResponseHeaderTimeout 的直接依据；重试/熔断/异常点剔除全部复用 Envoy 原语（BackendTrafficPolicy/outlier detection）。
- **APISIX** ai-proxy 插件族（Lua）[仅文档为主，源文件抓取不完整标注未验证]：多上游 failover 靠通用 upstream 健康检查 + retry 原语。
- **Higress** ai-token-rate-limit 插件 [仅文档，源码目录未验证]：流式响应按 token 计数限流。
- **共同结论**：数据面把"每凭据错误计数+冷却、半开探测、有界重试"做成**通用原语**而非业务逻辑——我们在单进程 Go 里的等价物就是 §6 的三个小组件。

## 6. 并发账号池专题（六个问题的跨项目结论）

### 6.1 数据模型

三种流派：
1. **渠道=凭据**（one-api abilities、aiproxy、我们的现状）：最简单，天然支持"同模型多账号"，靠渠道表自身字段承载状态。适合凭据数少、每凭据一 key（订阅制账号）。
2. **渠道内多 key**（new-api `MultiKeyStatusList`、uni-api ProviderKeyPool、gpt-load keys 表）：适合"一个账号一把 key、账号有多个"的 API 计费供应商；需要 key 级状态存储。
3. **deployment/凭据对象**（LiteLLM deployment、gpt-load v2 credential + state 包）：配置与状态分离（配置落 DB/文件，状态进内存注册表），最干净但工程量最大。

关键共识：**配置（哪些凭据、什么权重）与运行状态（在途数、冷却、失败计数）分离**——配置热更新不碰状态，状态重启可丢。凭据明文只存加密字段 + HMAC hash 用于日志/去重（gpt-load 双轨制）；热更新期间"DB 为准、内存为快照、异步落库"（gpt-load）。

### 6.2 调度策略

- 轮询/随机在**凭据同质**时足够（gpt-load/one-api 全线如此）；一旦凭据异质（限额不同/价格不同）需要权重。
- **平滑 WRR vs 普通加权**：平滑 WRR（nginx 算法）在长短请求混合时比"普通加权轮询"产出更均匀的**时间维度**分布；lottery（随机加权）实现最简但方差大。我们场景 2-4 个凭据，差异不显著，**weighted least-in-flight 优于一切轮询**（唯一有 in-flight 感知的是 LiteLLM least-busy）。
- **优先级分层**：one-api 的"最高优先级层内随机、重试才降层"最简且足够；避免"低优先级永远饿死"可用权重而非绝对分层表达（gpt-load effectiveWeight 手动×自动）。
- **sticky 对缓存/连贯性/切换的影响**：见 Q6。补充：粘性会放大"刚恢复账号被打爆"——解法是恢复期限流（half-open 只放行探测请求，aiproxy 的 ±10% 抖动 + LiteLLM cooldown 到期自然分摊）。
- **单机 vs 分布式**：单机互斥锁/原子变量即可；跨实例要么 Redis 原子操作（gpt-load RPOPLPUSH、aiproxy Lua）要么 PG `UPDATE...RETURNING` 抢占——都只在多实例时需要。

### 6.3 并发与限流

- **in-flight 计数**：acquire/release 成对出现于"发出上游请求前/响应完全结束后"。uni-api/gpt-load 都没有做（它们靠上游 429 反馈限流），Bifrost 有 `keyPoolFilter` 但无并发槽——**共识：并发槽是可选能力，主流靠"重试+冷却"兜底**。但订阅账号的并发限制表现为"超额直接挂连接/长阻塞"，反馈周期长，我们必须前置信号量。
- **流式释放时机**：所有项目都在「响应体读取结束」时归还——即流式场景= SSE 读循环退出（含错误）。客户端断开：请求 ctx 取消传导到上游连接（Bifrost 有专门注释防 channel 池复用泄漏；gpt-load ctx 继承自动取消）。panic：Go 里靠 `defer release()`（gpt-load 异步失败处理 goroutine 同样 defer）。
- **RPM/TPM/日/月额度组合**：LiteLLM 全做（复杂）；uni-api 只做 TPR 预检+冷却；实际共识是 **令牌桶做 RPM、计数器做并发、预算表做日/月**，TPM 用请求前估算+响应后校正（uni-api rollback 模式）。
- **单实例一致性**：互斥锁保护 (计数+状态) 原子读写即可；先取 key 再检查 vs 先检查再取的差异用「检查+递增在同一临界区」解决。

### 6.4 健康检查与熔断

分类收敛表（综合 8 个项目的实现）：

| 错误 | 处理 | 依据 |
| --- | --- | --- |
| 429 | 冷却（Retry-After 优先，默认 30s~30min） | LiteLLM 5s/uni-api 30min/gpt-load 计数 |
| 401/403 | 停用凭据（等验证/人工），本请求换下一个 | one-api ShouldDisableChannel、Bifrost deadKey、uni-api permanent auth |
| 402/配额耗尽 | 长冷却（小时级）+ 告警 | uni-api is_quota_exhausted、one-api 文本特征 |
| 404/400/413 | 不惩罚凭据；400 中的"上游配置错"重映射 503 后可切换 | uni-api remap、gpt-load 排除 404、LiteLLM 不冷却其余 4xx |
| 408/超时 | 冷却（短）或计入连续失败 | LiteLLM 408 冷却 |
| 5xx/网络 | 连续失败计数 → 阈值熔断；本请求可切换 | 全体 |

状态机共识：`healthy → cooling(定时自愈) → healthy` 管瞬时；`healthy → open(连续N失败/错误率超阈) → half-open(单探测) → healthy/open` 管持续性；**人工禁用（enabled=false）与自动状态分开存储**（aiproxy Status 字段 + monitor 分离；gpt-load enabled 与 failure_count 分离）——恢复探测可选：cron 真实验证（gpt-load，慢但真实）或 half-open 放行单请求（Envoy 式，快且零成本，推荐）。

### 6.5 重试与故障切换

- **可重试窗口 = 首字节前**。各家实现都在 Do() 返回错误或读到 failover 状态码阶段决策（gpt-load executeRequestWithRetry、one-api relay 循环、uni-api RoutingPlan.next_provider）；此时 body 可重放（我们都已全量读入内存）、响应头未写。
- **非幂等风险**：LLM 请求"看起来非幂等"（消耗额度），但「未产生任何输出的失败」不会计费输出 token，重试安全；**流式中途失败**重试会导致重复计费 + 重复输出，禁止。
- **预算**：uni-api `min(2×Σkey数, 10)`、gpt-load `MaxRetries=3`（共 4 尝试）、one-api 固定档。建议：`attempts ≤ 候选数`（每渠道至多一次）+ 无 backoff（gpt-load 证明无 backoff + 换凭据即可，backoff 只在"对同一凭据重试"时需要，而我们不这么做）。
- **尝试链记录**：Bifrost 的 tracer 条目、gpt-load request_logs(retry/final)。最小实现：usage_logs.attempts + error 记最终原因，链路进日志行。

### 6.6 公平性与背压

- 竞品几乎全空：无队列、无优先级、无 bulkhead。唯一接近的是 uni-api 的 admission（大 body 内存租约/disk spool）——解决内存安全而非公平。
- 我们的公平问题真实存在但规模小：多个 agent 的后台任务可能挤占账号池。**per-virtual-key 并发上限**（每 key 一个小信号量）即可，不需要调度器级公平队列；bulkhead（按客户端隔离渠道池）在我们渠道个位数的场景是过度设计。

## 7. 账号池关键时序和状态机

**正常请求（无失败）**：
```
客户端 → 认证 → FindModels(同名候选)
  → 取候选快照（配置 atomic snapshot）
  → 过滤（容量/健康）→ 选定账号 A（inflight 最小）
  → A.inflight++（临界区）→ 构造上游请求（绑 ctx）→ Do
  → 2xx：写响应头 → 透传(流式逐行 Flush + 计量) → defer A.inflight-- → 落库(provider/attempts=1)
```

**429 failover**：
```
→ A 返回 429（未写字节）
  → A: cooling_until = now + max(30s, Retry-After)，inflight--
  → 从候选剔除 A 重选 B（B.inflight++）→ 重发
  → 成功 → 落库 attempts=2, provider=B
  → 全候选失败 → 把最后一个上游错误按协议原样回传（gpt-load：错误 JSON 保真）
```

**凭据健康状态机**：
```
            成功(计一次健康样本)
   ┌──────────────────────────────┐
   ▼                              │
healthy ──429──▶ cooling(until t)──┘ (到期自动)
   │  │
   │  └─401/403/配额──▶ disabled_auto（人工或验证后才回）
   │
   └─连续 5xx/网络 ≥3──▶ open ──冷却期满──▶ half-open(放行1请求)
                             ▲                  │成功→healthy
                             └────失败────◀─────┘（重新 open，退避加长）
disabled_manual（面板 enabled=false）独立于此，任何状态不自动改它
```

## 8. 其他值得建设的方向（账号池之外）

| 方向 | 解决的真实问题 | 启发来源 | 建议 |
| --- | --- | --- | --- |
| 配置快照热更新 | reloadChannels 与读路径数据竞争 | gpt-load CacheSyncer 换指针 | P0，atomic.Pointer[Config] |
| 传输加固 | 无超时/无取消传播 → 挂死连接堆积 | Envoy AI GW 60s 默认、gpt-load 流式双 client | P0 |
| usage 归因 | 账号池不可观测 | Bifrost tracer/gpt-load request_logs | P0（字段）/P1（面板） |
| /metrics | 无外部监控入口 | Bifrost OTel 插件的简化面 | P1，手写 Prometheus 文本端点 |
| 审计日志 | 管理操作无痕 | aiproxy web/audit、new-api controller/audit | P1 |
| 错误脱敏 | 上游错误可能带出 key | gpt-load RedactSecret | P1 |
| prompt 粘性 | 订阅额度下的缓存经济性 | gpt-load affinity | P1/P2 |
| per-key 并发/公平 | agent 后台洪峰互扰 | 无竞品先例，需求自生 | P2 |
| 余额/窗口余量感知 | 订阅窗口耗尽前主动切换 | aiproxy BalanceThreshold、我们已有的 MiniMax quota 解析 | P2 |
| 模型别名/组 | 客户端换名成本 | LiteLLM routing_groups | P2 |
| 预算周期重置 | virtual key 月度预算 | Helicone virtual key reset（仅文档） | P2 |
| 异常用量告警 | key 泄露/失控成本 | aiproxy 消费排名/Helicone | P2 |

## 9. 与当前代码的差距分析

| # | 差距 | 现状证据 | 目标 | 复杂度 |
| --- | --- | --- | --- | --- |
| 1 | 单候选路由 | `FindModel` first-match | `FindModels` 全候选 + 调度器 | S |
| 2 | 无并发槽 | 无 | `dispatch.slot`（mutex+notify） | S |
| 3 | 无健康状态 | 无 | cooling/open/half-open/disabled 三层 | M |
| 4 | 无重试 | Do 后直接透传 | 首字节前 attempt 循环 | M |
| 5 | 无超时/取消 | `http.DefaultClient`+`http.NewRequest` | 双 client + `NewRequestWithContext` | S |
| 6 | 热更新竞态 | `cfg.Channels = channels` 裸赋值 | atomic 快照 | S |
| 7 | 无归因 | usage_logs 无 provider | +provider/+attempts | S |
| 8 | 无审计 | 无 | audit_logs 表 | S |
| 9 | SSE 1MB 行上限 | scanner.Buffer(1<<20) | 4MB + 保持旁路计量 | S |
| 10 | 无 body 上限 | io.ReadAll 无界 | MaxBytesReader | S |

## 10. 推荐路线图

见 §1 Q10 三阶段。每项建议的「问题/启发/实现/改动文件/表/复杂度/成本/风险/验收」明细：

### P0-1 上游凭据池调度（含并发槽、优先级、权重）
- 问题：单账号并发上限卡容量；单点无 failover。启发：one-api abilities + uni-api 请求局部环 + LiteLLM least-busy + gpt-load failover 匹配器。
- 简化实现：渠道即账号 + `FindModels` + `internal/dispatch`（slot+cooldown+attempt 循环）；每渠道至多尝试一次。
- 改动：`internal/config/config.go`（FindModels/weight/priority 字段）、`cmd/gateway/main.go`（handleProxy）、新 `internal/dispatch/`、`internal/store/pg.go`+`schema.sql`（channels 加 3 列）。
- 新表：否（仅加列）。复杂度 M。运行时成本：每请求一次内存临界区，可忽略。
- 风险：流式释放遗漏导致槽泄漏（defer+race 测试覆盖）；全候选冷却时的快速失败语义。
- 验收：M1-①②③。

### P0-2 传输加固（超时/取消/上限/优雅停机）
- 问题：挂死连接、泄漏、无界内存。启发：gpt-load 双 client、Envoy 60s、uni-api body 限制。
- 实现：`upstreamHTTP`（dial 5s/TLS 5s/response-header 120s/idle 120s/无整体超时）与流式专用禁压缩 client；`NewRequestWithContext`；`http.MaxBytesReader` 20MB；scanner 4MB；`http.Server.Shutdown`。
- 改动：`cmd/gateway/main.go`、新 `internal/transport/`。新表：否。复杂度 S。
- 风险：ResponseHeaderTimeout 过紧误杀慢模型（可配置）。
- 验收：M1-④⑤。

### P0-3 健康状态机与冷却（P0-1 内核的一部分，独立成包便于测试）
- 问题：429 打到已限流账号；坏 key 反复试。启发：LiteLLM `_is_cooldown_required` 表 + aiproxy 抖动 + Envoy half-open。
- 实现：`dispatch.health`（cooling map / 连续失败计数 / half-open 单探测）；429 冷却 `max(30s, Retry-After)`；401/403 → disabled_auto（面板可见，可手动恢复）。
- 复杂度 M。验收：注入 429×N 后 N 秒内不再选中该渠道；恢复后自动回归。

### P0-4 配置快照热更新
- 问题：DECISIONS 待验证项 1 成真——读路径与 reload 并发。启发：gpt-load CacheSyncer。
- 实现：`atomic.Pointer[config.Config]`；reload 构建新 Config 后原子替换；请求开始时 `Load()` 一次快照贯穿全程（天然与 uni-api"请求局部环"一致）。
- 改动：config.go/main.go。复杂度 S。验收：`-race` 下压测 + 面板连续改渠道无报错。

### P0-5 usage 归因字段
- 问题：无法验证调度正确性。改动：schema + `UsageLog` + 落库 SQL + 日志行。复杂度 S。验收：failover 请求 records attempts≥2 且 provider=实际账号。

### P1
- **P1-1 面板渠道健康页 + /metrics**：调度快照（inflight/cooling/open）暴露 `/api/channels/health`；`/metrics` 手写文本（请求计数/延迟直方图/凭据状态 gauge）。启发 Bifrost/AI GW 观测面。复杂度 M。
- **P1-2 审计日志**：`audit_logs(action, actor, target, before, after, at)` + 管理 API 钩子。复杂度 S。
- **P1-3 错误文本脱敏**：回传/落库前 RedactSecret（正则匹配 key 明文与 URL query）。复杂度 S。

### P2
- prompt 前缀粘性（gpt-load affinity 式）；per-virtual-key 并发上限；余额/窗口感知排序；RPM 令牌桶；TPM 预检+rollback 校正；模型别名；预算周期重置；异常用量告警。

### 不建议
- Redis/Kafka/K8s 多实例协调；OpenTelemetry SDK 全量接入；语义路由（决策 2 维持）；协议转换（决策 12 维持）；gpt-load 式参数覆盖/模型重定向（破坏纯透传）；主动健康探测池（渠道数太小，half-open 足够）；引入 new-api（AGPL + 定位冲突）。

## 11. 风险、边界及不建议建设的能力

1. **许可边界**：MIT/Apache 项目（one-api、gpt-load、aiproxy、uni-api、Bifrost、Portkey、LiteLLM 核心）可移植代码（保留版权声明）；**new-api AGPL-3.0 禁止移植任何代码**，只读设计。
2. **单维护者依赖**：gpt-load 1179/1179 提交来自一人——借鉴设计要接受其 API 会漂移，锁定 commit（本调研锁定 0b8ee14）。
3. **信号量泄漏**是账号池最大工程风险：所有失败路径必须 defer release，race + goleak 测试强制。
4. **冷却误伤**：429 冷却过长会在上游恢复后闲置容量 → 冷却上限封顶（如 10min）+ half-open 兜底。
5. **重复计费**：failover 只允许发生在"上游未产出任何 token"的错误上；流式开始后绝不重试（§6.5）。
6. **可观测缺失即盲飞**：P0-5 必须与 P0-1 同批上线，否则无法验收。

## 12. 来源链接和源码证据

| 项目 | 仓库 | License | 证据深度 | 关键源码位置 | 调研日期/commit |
| --- | --- | --- | --- | --- | --- |
| uni-api | github.com/yym68686/uni-api | Apache-2.0 | 源码级 | uni_api/routing/core.py, routing/health.py, upstream/policies.py, rate_limit/key_pool.py | main@2026-09-02 |
| gpt-load | github.com/tbphp/gpt-load | MIT | 源码级（最深） | internal/keypool/provider.go, proxy/server.go, scheduler/scheduler.go, affinity/key.go, channel/base_channel.go, state/ | 0b8ee14@2026-09-01 |
| aiproxy | github.com/labring/aiproxy | MIT | 源码级 | core/model/channel.go, core/monitor/memmodel.go, core/monitor/model.go | main@2026-09-02 |
| one-api | github.com/songquanpeng/one-api | MIT | 源码级 | model/ability.go, controller/relay.go, monitor/manage.go | main@2026-09-02 |
| new-api | github.com/QuantumNous/new-api | **AGPL-3.0** | 源码级 | model/channel.go:230-300（多 key 选择）, controller/relay.go:365 | main@2026-09-02 |
| LiteLLM | github.com/BerriAI/litellm | MIT(核心) | 源码级 | litellm/router.py:619-677, router_utils/cooldown_handlers.py:205-340, constants.py:36-94 | main@2026-09-02 |
| Bifrost | github.com/maximhq/bifrost | Apache-2.0 | 源码级 | core/bifrost.go:6040-7025(executeRequestWithRetries), core/keyselectors/weightedrandom.go, core/streamfallback_test.go | main@2026-09-02 |
| Portkey gateway | github.com/Portkey-AI/gateway | MIT | 源码级 | src/handlers/handlerUtils.ts:640-860, package.json v1.15.2 | main@2026-09-02 |
| Envoy AI Gateway | github.com/envoyproxy/ai-gateway | Apache-2.0 | 源码(API 层) | api/v1alpha1/ai_gateway_route.go:255-266（60s 默认超时） | main@2026-09-02 |
| APISIX | github.com/apache/apisix | Apache-2.0 | 文档+部分源码 | apisix/plugins/ai-*（源文件抓取不完整，标未验证） | 2026-09-02 |
| Higress | github.com/alibaba/higress | Apache-2.0 | 文档级 | ai-token-rate-limit 插件（目录未定位成功，标未验证） | 2026-09-02 |
| TensorZero | github.com/tensorzero/tensorzero | Apache-2.0 | 部分源码 | crates/tensorzero-core/src/model.rs/config（weights/shuffle 逻辑未定位，标未验证） | main@2026-09-02 |
| Helicone | github.com/Helicone/helicone | Apache-2.0 | 文档级（virtual key 预算/重置） | — | 2026-09-02 |

> 本地克隆缓存：/tmp/comp/（uni-api、gpt-load、aiproxy、one-api、new-api、bifrost、gateway(Portkey)、ai-gateway、litellm-src、apisix、tensorzero）。
