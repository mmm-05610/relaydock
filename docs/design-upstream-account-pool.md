# 多账号池设计

> 状态：第一版已实现（§14 实施拆分 1–6 完成；第 7 步真实客户端验证进行中）。
> 依赖 [Go 网关软件架构](software-architecture.md)。
> 范围：单实例、多账号、并发槽、基础选择、429 冷却和保守 failover。

## 1. 目标

同一个渠道可以配置多个合法持有的上游账号。请求到来时，网关选择一个健康且有容量的账号，并在响应结束后可靠归还并发槽。

第一版解决：

- 多个账号共同承载同一渠道的模型请求；
- 每账号独立设置最大并发；
- 长短不一的流式请求尽量均衡分布；
- 账号返回 429 后短暂退出候选集；
- 在客户端响应尚未提交时，对确定安全的错误换账号；
- 记录最终账号和尝试次数；
- 配置热更新不破坏正在执行的请求。

第一版不做权重、优先级、sticky、RPM/TPM、余额路由、跨实例同步和复杂熔断器。

## 2. 数据模型

不采用“一个 channel 伪装成一个账号”。Channel 继续表示供应商和路由配置，Account 表示该渠道下的一份独立凭据和容量。

```text
Channel（供应商/Endpoint/协议配置）
  ├─ Model A
  ├─ Model B
  └─ Accounts
       ├─ Account 1：credential + max_concurrency
       └─ Account 2：credential + max_concurrency
```

这样 `provider=deepseek` 的语义不会因为增加账号而变成 `deepseek-a/deepseek-b`，模型和协议路由也不需要复制。

### 2.1 新表

```sql
CREATE TABLE IF NOT EXISTS upstream_accounts (
  id               BIGSERIAL PRIMARY KEY,
  channel_id       BIGINT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  name             TEXT NOT NULL,
  encrypted_key    BYTEA NOT NULL,
  key_fingerprint  TEXT NOT NULL,
  max_concurrency  INTEGER NOT NULL DEFAULT 0,
  enabled          BOOLEAN NOT NULL DEFAULT TRUE,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(channel_id, name),
  UNIQUE(channel_id, key_fingerprint),
  CHECK (max_concurrency >= 0)
);

CREATE INDEX IF NOT EXISTS idx_upstream_accounts_channel
  ON upstream_accounts(channel_id) WHERE enabled = TRUE;
```

说明：

- `max_concurrency=0` 表示不限制；已知供应商存在硬并发限制时应显式填写；
- `key_fingerprint` 使用服务端密钥参与的 HMAC，只用于去重和日志关联，不保存可用于认证的 Key 前缀；
- 第一版不把 cooling/inflight 等运行状态写入 PostgreSQL；
- 人工启停持久化，自动冷却重启后清零。

### 2.2 兼容现有 upstream_keys

迁移采用“新增、复制、兼容读取、最后退役”，不直接删除旧表：

1. 为每条 `upstream_keys.provider` 找到对应 channel；
2. 解密后重新加密写入一个名为 `default` 的 upstream account；
3. 读取时优先使用 `upstream_accounts`；渠道没有账号时临时回退旧表；
4. 管理面和 CLI 完成账号管理后，再单独决定是否删除旧表。

迁移程序必须可重复执行，且不能在日志输出明文凭据。

### 2.3 usage_logs 归因

```sql
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS channel_id BIGINT;
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS account_id BIGINT;
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS attempts INTEGER NOT NULL DEFAULT 1;
```

这里记录最终承载响应的账号。中间失败尝试写结构化日志；第一版不新增 attempt_logs 表。

## 3. 配置态与运行态

每个账号分为不可变配置和共享运行状态：

```go
type AccountSpec struct {
    ID             int64
    ChannelID      int64
    Name           string
    Credential     string // 仅存在于进程内快照，不序列化、不打印
    MaxConcurrency int64
    Enabled        bool
}

type AccountState struct {
    mu            sync.Mutex
    inflight      int64
    coolingUntil  time.Time
    cooldownCause string
}

type AccountRef struct {
    Spec  AccountSpec
    State *AccountState
}
```

`AccountState` 以稳定的 `account_id` 注册。配置热更新时：

- 同 ID 账号复用 State；
- 新账号创建 State；
- 被删除账号不再进入新快照；
- 旧请求仍持有旧 AccountRef，完成后正常 Release；
- 修改凭据或并发上限通过新 AccountSpec 生效，不原地改旧 Spec。

## 4. 原子 Acquire 与 Lease

并发上限的检查和递增必须在同一把锁内完成，不能使用 `Load` 后再 `Add`。

```go
func (s *AccountState) tryAcquire(spec AccountSpec, now time.Time) bool {
    s.mu.Lock()
    defer s.mu.Unlock()

    if !spec.Enabled || now.Before(s.coolingUntil) {
        return false
    }
    if spec.MaxConcurrency > 0 && s.inflight >= spec.MaxConcurrency {
        return false
    }
    s.inflight++
    return true
}

type lease struct {
    account AccountRef
    once    sync.Once
}

func (l *lease) Release() {
    l.once.Do(func() {
        l.account.State.mu.Lock()
        l.account.State.inflight--
        l.account.State.mu.Unlock()
    })
}
```

测试中必须断言 `inflight` 永不为负，也永不超过非零 `max_concurrency`。

## 5. 选择算法

第一版账号同质，不引入 weight 和 priority。选择目标是降低账号占用比例：

```text
有限并发账号：score = inflight / max_concurrency
无限并发账号：score = inflight
```

选择过程：

1. 从快照取得该 RouteCandidate 下的账号列表；
2. 排除本请求已经尝试的账号；
3. 排除人工禁用、冷却中和没有并发槽的账号；
4. 按占用比例从低到高尝试原子 Acquire；
5. 同分时使用进程内递增游标轮换，避免总偏向配置中的第一个账号。

排序结果只能作为尝试顺序，最终容量判断必须由 `tryAcquire` 完成，因为排序与 Acquire 之间状态可能变化。

### 全部满槽

第一版默认快速失败，不建立无界等待队列：

- 全部账号满槽：返回本地 429，并携带简短 `Retry-After`；
- 全部账号冷却或禁用：返回 503；
- 错误 body 使用网关自己的稳定 JSON 格式。

保留可配置的短等待接口，但默认关闭。只有实测 Claude Code/Codex/OpenCode 的重试行为后，才决定是否启用有界等待；启用时必须同时限制等待时间和全局等待者数量。

## 6. Attempt 生命周期

一次尝试必须在独立函数作用域内执行：

```go
func runAttempt(ctx context.Context, original GatewayRequest, c RouteCandidate, a AccountRef) AttemptResult {
    lease, err := pool.AcquireOne(a)
    if err != nil { return capacityResult(err) }
    defer lease.Release()

    req, err := proxy.BuildRequest(ctx, original, c, a)
    if err != nil { return buildResult(err) }

    resp, err := transport.Do(req)
    if err != nil { return transportResult(err) }
    defer resp.Body.Close()

    return classifyAndRelay(resp)
}
```

重要约束：

- 每个 attempt 都从原始 body 替换自己的 upstream model；
- A 账号失败后必须先 Release，才能尝试 B；
- 失败响应在切换前关闭；在大小和时间受控的前提下 drain，以便复用连接；
- 请求总尝试次数不超过可用账号数，并受总时间预算约束；
- 客户端 context 取消后立即停止，不再尝试其他账号。

## 7. Failover 边界

第一版采用保守策略。

| 结果 | 账号状态 | 是否换账号 |
| --- | --- | --- |
| 明确的账号级 429 | 按 Retry-After 冷却 | 是，响应未提交时 |
| 明确的 credential invalid | 较长内存冷却并告警；暂不永久自动禁用 | 是，响应未提交时 |
| 明确的 quota exhausted | 较长冷却 | 是，响应未提交时 |
| 400/404/413 | 不惩罚 | 否 |
| 5xx | 记录失败 | 默认否 |
| 网络错误/超时/TLS 错误 | 结果未知 | 默认否 |
| 客户端 context 取消 | 不惩罚 | 否 |
| 响应已经提交 | 按实际结果记录 | 绝不切换 |

网络错误和 5xx 默认不透明重试，因为上游可能已经执行并计费，只是结果没有送达。未来如真实数据证明某个 Provider 的特定错误可安全切换，再通过 ProviderClassifier 精确放开，不能只按状态码全局放开。

```go
type ErrorClassifier interface {
    Classify(provider string, resp *http.Response, err error) Classification
}

type Classification struct {
    Class        ErrorClass
    Retryable    bool
    OutcomeKnown bool
    Cooldown     time.Duration
}
```

## 8. 429 冷却

第一版只实现冷却，不实现完整 open/half-open 熔断器：

```text
healthy ──账号级429──▶ cooling ──到期──▶ healthy
```

冷却时间：

1. 优先解析标准 `Retry-After` 秒数或 HTTP 日期；
2. 没有有效值时使用可配置默认值；
3. 设置合理上限，异常 Retry-After 不能让账号永久消失；
4. 加少量 jitter，避免多个账号同时恢复造成瞬时拥塞。

是否属于“账号级 429”由 ProviderClassifier 判断。无法判断时可以短冷却，但不能自动永久停用账号。

## 9. 流式请求

并发槽持有窗口：

```text
Acquire
  → 发起上游请求
  → 收到响应头
  → 持续转发 SSE
  → 正常 EOF / 上游断流 / 客户端断开
  → Release
```

要求：

- 上游请求绑定客户端 context；
- 检查写客户端的错误，写失败后停止读取上游；
- 读取循环退出的所有路径都由 defer 归还 Lease；
- 收到 2xx 后若首个 SSE event 是协议级错误，第一版仍直接转发，不为 failover 缓冲首事件；
- SSE 已提交后断流不换账号；
- 没拿到 usage 时沿用现有 fallback，但要记录流是否完整。

## 10. 快照与账号池一致性

Reload 顺序固定为：

```text
Load DB
  → validate channels/models/accounts
  → reconcile AccountState registry by account_id
  → build RouteCandidate + AccountRef
  → atomic publish Snapshot
```

请求在开始时加载一次 Snapshot。重试只能使用该请求最初得到的候选集合，避免热更新后重复或跳过账号。人工紧急禁用若需要立即影响旧快照，可额外在 AccountState 中设置 runtime disabled 标志；第一版先接受“只影响新请求”的快照语义。

## 11. 管理 API 最小集合

```text
GET    /api/channels/{provider}/accounts
POST   /api/channels/{provider}/accounts
PUT    /api/channels/{provider}/accounts/{id}
DELETE /api/channels/{provider}/accounts/{id}
POST   /api/channels/{provider}/accounts/{id}/test
```

返回值只能包含账号 ID、名称、启用状态、并发限制、Key 是否已配置以及运行时健康摘要，不能返回明文 Key、密文或可识别的长 Key 前缀。

运行时摘要：

```json
{
  "id": 12,
  "name": "account-b",
  "enabled": true,
  "inflight": 1,
  "max_concurrency": 2,
  "state": "healthy",
  "cooling_until": null
}
```

## 12. 可观测性

第一版至少记录：

- 请求最终 channel/account；
- attempts 总数；
- 每账号当前 inflight；
- 本地 capacity rejection 次数；
- 账号级 429 次数和冷却次数；
- 客户端取消、上游断流和未知结果网络错误；
- 每次失败 attempt 的 request_id、account_id、错误类别和耗时。

不得记录原始请求 body、Authorization、上游凭据和未经脱敏的上游错误文本。

## 13. 测试与验收

### 单元测试

- N 个 goroutine 抢 M 个槽，峰值永不超过 M；
- Release 重复调用不会把 inflight 减成负数；
- 同容量账号在持续并发下无长期固定偏置；
- 冷却账号不被选择，到期后重新进入候选；
- 配置 reload 保留相同 account_id 的运行状态；
- 删除账号时已有 Lease 仍可正常 Release。

### HTTP 故障注入

- 两账号各限制 2 并发，4 个长流请求能同时运行，第 5 个按设计快速失败；
- A 返回带 Retry-After 的 429，未提交响应时切到 B；
- 429 响应 body 被关闭，连接和槽均不泄漏；
- 网络超时默认不切换，并标记 outcome unknown；
- 客户端断开后上游 context 被取消，槽最终归零；
- SSE 中途断开不切换，不拼接第二个响应；
- 两账号使用不同 upstream model 时，每次 attempt 的 body 都正确；
- 热更新与持续请求并发运行，`go test -race ./...` 无告警。

### 回归红线

- 三种协议的正常请求和 usage 提取行为不退化；
- 未配置多账号时行为与当前单 Key 渠道兼容；
- 计量或 PostgreSQL 写入失败不改变客户端已收到的成功响应；
- `go test ./...`、`go test -race ./...`、`go vet ./...` 通过。

## 14. 实施拆分

1. 先完成软件架构文档中的 Snapshot、RouteCandidate、Executor 和 Proxy 边界；
2. 添加 upstream_accounts 表及兼容读取，不启用调度；
3. 实现 AccountState、原子 Acquire 和幂等 Lease；
4. 开启基础选择和容量拒绝；
5. 加 ProviderClassifier 与 429 冷却；
6. 加账号归因、管理 API、并发与故障注入测试；
7. 用真实 Claude Code/Codex/OpenCode 验证取消、429 和客户端重试行为。

完成以上七步即视为第一版结束，不顺带加入后续高级能力。
