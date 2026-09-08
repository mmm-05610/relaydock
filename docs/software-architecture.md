# Go 网关软件架构

> 状态：目标架构设计稿。本文描述单二进制内部的软件边界，不改变现有双机部署方式。
> 当前阶段只服务两个目标：建立可靠的网关内核；在该内核上实现多账号池。

## 1. 目标与边界

网关继续保持以下定位：

- Go 单二进制、低内存、少依赖；
- 面向 Claude Code、Codex、OpenCode 等 coding agent；
- 原生协议路由，不做 Anthropic/OpenAI 之间的格式转换；
- PostgreSQL 保存配置、凭据和用量；
- 单实例运行，不引入 Redis、Kafka、Kubernetes；
- 计量和观测是旁路能力，失败不能破坏已经成功的代理响应。

本阶段不建设语义路由、复杂策略 DSL、跨实例协调、完整企业 RBAC、Guardrail 平台和大而全的 Provider 适配层。

## 2. 总体结构

单个进程内部划分为控制面和数据面。二者共享类型和存储，但不共享可变配置对象。

```text
                         Go Gateway Process
┌──────────────────────────────────────────────────────────────┐
│ Control Plane                                                │
│                                                              │
│ Admin HTTP → 配置校验 → PostgreSQL → 构建新 Snapshot          │
│                                         │                    │
│                                         └── atomic publish ─┐│
├─────────────────────────────────────────────────────────────┤│
│ Data Plane                                                  ││
│                                                            ▼│
│ Client HTTP                                                 │
│    → Authenticate                                           │
│    → Parse Request                                          │
│    → Resolve Route                                          │
│    → Select Account / Acquire Lease                         │
│    → Execute Attempt                                        │
│    → Relay Response                                         │
│    → Finalize Metering                                      │
│                                                              │
│ Logging / Metrics / Usage Store 观察链路，不参与成功判定       │
└──────────────────────────────────────────────────────────────┘
```

控制面发布完整、不可变的路由快照。每个数据面请求只加载一次快照，并在整个请求生命周期中使用该版本，避免请求执行到一半时渠道、路由或凭据发生变化。

## 3. 包结构

不追求过度拆包，目标结构如下：

```text
gateway/
  cmd/gateway/
    main.go                 依赖装配、HTTP Server、信号与优雅停机

  internal/
    gateway/
      handler.go            数据面 HTTP 入口与请求编排
      admin.go              管理 API 入口
      request.go            GatewayRequest/RequestResult

    routing/
      snapshot.go           不可变配置快照及原子发布
      resolver.go           model + path → RouteCandidate 列表

    pool/
      pool.go               账号选择、Lease、并发槽
      state.go              账号运行时状态和 429 冷却
      classify.go           保守错误分类及 Provider 扩展点

    proxy/
      request.go            每次 attempt 构造独立上游请求
      transport.go          HTTP Transport、超时、连接池、取消传播
      relay.go              非流式/SSE 转发与响应提交状态

    config/                 配置类型和 YAML 种子（保留）
    keys/                   Virtual Key 和凭据加解密（保留）
    metering/               usage 提取与成本计算（保留）
    store/                  PostgreSQL/内存存储（保留）
```

`cmd/gateway/main.go` 最终只负责创建依赖、注册路由和启动/关闭服务，不再承载完整代理逻辑。

## 4. 核心领域对象

### 4.1 GatewayRequest

表示一个客户端请求。原始请求体必须保留，不能被某次上游尝试永久修改。

```go
type GatewayRequest struct {
    ID        string
    Method    string
    Path      string
    Protocol  string
    Model     string
    Header    http.Header
    Body      []byte // 客户端原始 body，只读
    Principal *keys.Key
}
```

### 4.2 RouteCandidate

表示“这个请求可以通过哪个渠道、模型路由和账号执行”。候选必须是完整值，不能只返回 `Channel` 或 `Model`。

```go
type RouteCandidate struct {
    ID            string
    ChannelID     int64
    Provider      string
    Model         string
    UpstreamModel string
    Protocol      string
    UpstreamURL   string
    AuthMode      string
    Pricing       config.Pricing
    Accounts      []*pool.AccountRef
}
```

Resolver 必须先按请求 path 过滤路由。一个渠道存在同名模型，但没有当前协议路由时，不能进入候选集。

### 4.3 Attempt

一次客户端请求可以包含多次上游尝试。Attempt 是观测和错误判断的最小单位。

```go
type Attempt struct {
    Number      int
    CandidateID string
    AccountID   int64
    StartedAt   time.Time
    FirstByteAt time.Time
    FinishedAt  time.Time
    Status      int
    ErrorClass  ErrorClass
    Err         error
}
```

### 4.4 Lease

账号池不把裸账号状态暴露给 Handler，而是返回一个 Lease：

```go
type Lease interface {
    Account() AccountRef
    Release()
}
```

`Release` 必须幂等，内部使用 `sync.Once`。每次 attempt 在独立函数作用域内获取并释放 Lease，不能把多个 attempt 的 `defer` 全部堆到最外层 Handler。

## 5. 数据面请求生命周期

```text
1. Handler 读取有限大小的请求体
2. 认证 Virtual Key，检查模型权限和额度
3. 从 SnapshotStore 加载一次不可变快照
4. Resolver 根据 model + path 生成 RouteCandidate 列表
5. Executor 创建本请求的 AttemptPlan
6. Pool 从候选账号中获取 Lease
7. Proxy 基于原始 body 和本次 candidate 构建上游请求
8. Transport 使用客户端 context 发起请求
9. 收到结果后分类：返回、冷却账号，或在安全边界内尝试下一个账号
10. Relay 提交响应并完成流式/非流式传输
11. Metering 使用最终 candidate 的协议和价格计算用量
12. 写 RequestRecord；每个失败 attempt 写结构化日志
```

上游尝试应封装为独立函数，以保证资源生命周期完整：

```text
runAttempt
  → acquire lease
  → defer lease.Release
  → build request from original body
  → transport.Do
  → classify response
  → close/drain failed response body
  → return AttemptResult
```

## 6. 响应提交边界

Relay 维护两个状态：

```text
Uncommitted：尚未向客户端发送响应头或 body
Committed：已经调用 WriteHeader 或成功写出 body
```

只有 `Uncommitted` 状态才允许选择另一个账号。`Committed` 后发生上游断流，只能结束当前响应并记录错误，不能拼接第二个上游的输出。

“未提交客户端响应”只代表响应拼接仍然安全，不代表上游一定没有计费。网络超时、连接重置和部分 5xx 的执行结果未知，默认不能据此断言重试无成本。

## 7. 配置快照

### 7.1 Snapshot 内容

```go
type Snapshot struct {
    Version  uint64
    Channels map[int64]ChannelSpec
    Models   map[string][]RouteCandidate
}
```

快照中包含完成一次路由所需的全部只读信息，包括已解密但不对外暴露的凭据引用。运行时并发状态不复制进 Snapshot，而由稳定的 `account_id` 关联到 Pool Registry。

### 7.2 发布流程

```text
管理 API 写数据库
  → 从数据库完整读取 channels/models/accounts
  → 校验唯一性、URL、协议、账号可用性和参数范围
  → 构建完整 Snapshot
  → Pool Registry 按 account_id 对账
  → 原子发布 Snapshot
```

构建或校验失败时继续使用旧快照。禁止先修改全局 map，再逐步补其他对象。

删除账号时，旧请求持有的 AccountRef 和 Lease 仍可完成；新快照不再产生该账号的新 Lease。待在途数归零后运行时对象自然回收。

## 8. Transport 与 Relay

Transport 应显式配置：

- Dial、TLS handshake、response header 超时；
- 最大空闲连接数、每 Host 空闲连接数和空闲连接寿命；
- `NewRequestWithContext` 传播客户端取消；
- 对流式请求不设置会截断长响应的整体 `Client.Timeout`；
- Header 白名单/黑名单，禁止转发客户端 Authorization 和 hop-by-hop header；
- 上游错误进入日志或响应前进行凭据脱敏。

`IdleConnTimeout` 只控制连接池中的空闲连接，不是 SSE 读取超时。是否增加流式读空闲超时应独立设计并可配置。

Relay 不应依赖固定大小的 `bufio.Scanner` 来承担字节转发。推荐：

- 字节路径使用 `bufio.Reader` 或受控缓冲复制，保留原始换行；
- 计量解析器旁路观察完整 SSE event；
- 计量事件过大或解析失败时只降级计量，不中断主响应；
- 检查客户端 `Write` 错误，使断开能尽快结束上游读取。

## 9. 核心不变量

以下规则作为架构验收标准：

1. 请求开始后只使用一个配置快照版本。
2. 控制面不原地修改数据面正在读取的配置和凭据 map。
3. 原始请求 body 永远保留，每个 attempt 独立构造上游 body。
4. Resolver 只生成候选；Pool 只选择账号；Executor 管理 attempt；Relay 只提交响应。
5. 每次成功 Acquire 必须且只能 Release 一次。
6. 客户端响应提交后禁止透明切换上游。
7. 客户端取消必须传播到上游。
8. 请求体、等待时间、尝试次数和错误响应缓存均有明确上限。
9. 计量、日志、指标或数据库失败不能改变已成功的代理响应。
10. 不做跨协议 body/response 转换；协议相关逻辑集中在解析、Header 和 usage extractor。
11. 上游凭据不出现在客户端响应、普通日志或 usage_logs 中。
12. 所有并发共享状态通过锁、原子快照或明确所有权保护，并通过 `go test -race` 验证。

## 10. 错误和观测模型

区分客户端请求和上游尝试：

```text
RequestRecord
  request_id / virtual_key / requested_model / final_status
  final_channel / final_account / attempts / total_latency / usage / cost

AttemptRecord
  request_id / attempt_no / channel / account
  status / error_class / latency / outcome_known
```

第一版不强制增加 `attempt_logs` 表。失败尝试先写结构化应用日志和计数指标；`usage_logs` 增加最终 `channel_id/account_id/attempts`。如果真实运维中需要查询完整尝试链，再新增独立表，避免把中间失败伪装成多次客户端请求。

## 11. 实施顺序

### A. 行为保护

- 为现有三种协议补代理和计量 characterization tests；
- 覆盖非流式、SSE、客户端取消、上游断流和大事件；
- 记录重构前基线行为。

### B. 无功能重构

- 引入 GatewayRequest、RouteCandidate 和 Snapshot；
- 拆出 routing、proxy 和 gateway Handler；
- Dispatcher 暂时保持 first-match；
- 保证对外行为不变。

### C. 基础可靠性

- Context 传播、请求体上限、Transport 超时和连接池；
- 原子配置快照和凭据快照；
- 正确的流式转发、写错误处理和优雅停机；
- `go test -race ./...` 通过。

### D. 多账号池

- 按配套文档实现账号数据模型、Lease、并发槽、选择和 429 冷却；
- 只加入保守、可解释的 failover；
- 加入账号归因、故障注入和并发测试。

完成 D 后再根据真实使用数据决定下一项能力，不在本设计中预设后续路线。
