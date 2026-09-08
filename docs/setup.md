# 部署（实际落地版）

> 本文档记录**实际部署**的架构与步骤。架构决策见 [DECISIONS.md](DECISIONS.md)（当前架构：决策 12 自建 Go 网关）。
> 客户端接入见 [clients.md](clients.md)。

## 架构总览（双服务器分层）

```
服务器 A（女朋友账号，2核2G）— 数据层（隐藏）
  115.29.241.36
  └── postgres :5432   gateway 库（5 表）
  安全组：5432 → 只放行 B 的 IP（121.40.184.111/32）

服务器 B（毛庆辉账号，2核2G）— 服务层 + 入口层
  121.40.184.111
  ├── gateway   :8080（systemd gateway.service，单二进制 ~10MB）
  │              ├── /v1/*         → 自建 Go 网关（透传）
  │              ├── /api/*        → 自建 Go 网关（管理 API）
  │              └── /             → 静态面板（navpage/）
  └── caddy     :80/:443（唯一公网入口，TLS + 反代 /v1, /api 到 :8080）
```

客户端统一指向 `http://121.40.184.111`（Caddy 入口），**不再用 4000 端口**（那是 LiteLLM 时代）。

---

## 服务器 A：纯 PostgreSQL（数据层）

只装 PostgreSQL，**不跑任何应用**。网关 binary 不在 A 机部署。

### 安装 PG（Ubuntu/Debian）

```bash
sudo apt update
sudo apt install -y postgresql postgresql-contrib
sudo systemctl enable --now postgresql
```

### 创建库与角色

```bash
sudo -u postgres psql <<'SQL'
CREATE USER gateway WITH PASSWORD '你的密码';
CREATE DATABASE gateway OWNER gateway;
GRANT ALL PRIVILEGES ON DATABASE gateway TO gateway;
SQL
```

### 导入 schema

```bash
psql "postgresql://gateway:你的密码@127.0.0.1:5432/gateway" \
  -f gateway/schema.sql   # 仓库内：5 张表（upstream_keys/keys/usage_logs/channels/models）
```

### 安全组 / pg_hba.conf

- 公网安全组：`5432/tcp` → 只放行 `121.40.184.111/32`（B 机），其他全拒。
- `pg_hba.conf` 加一行限制 B 机访问（可选，双重防御）：
  ```
  host  gateway  gateway  121.40.184.111/32  scram-sha-256
  ```
- `22` → 你的 IP（SSH，密钥登录）；删 RDP / ICMP 等默认规则。

---

## 服务器 B：Go 网关 + Caddy（服务层 + 入口层）

### 目录结构

```
/opt/gateway/
├── gateway              # 编译产物（gateway/bin/gateway）
├── config.yaml          # 渠道配置（首次启动会种子导入到 PG）
├── navpage/             # 静态面板（仓库 navpage/，含 index.html / panel.html）
├── migrate              # cmd/migrate 编译产物（手动跑迁移时用）
└── dbclean              # cmd/dbclean 编译产物（清理用量日志）
```

> 注：仓库根目录的 `config.yaml`（LiteLLM 风格的 `model_list`）是历史遗留，**不再使用**——当前实际配置是 `gateway/config.yaml`（channels 格式）。

### 编译网关

```bash
cd gateway
CGO_ENABLED=0 go build -o bin/gateway ./cmd/gateway
CGO_ENABLED=0 go build -o bin/migrate ./cmd/migrate    # 可选
CGO_ENABLED=0 go build -o bin/dbclean ./cmd/dbclean    # 可选
# 产物 ~10MB 单二进制，无运行时依赖
```

或者直接用 `gateway/bin/` 里已经编译好的产物（git 不跟踪，部署时自行构建）。

### systemd 单元（`/etc/systemd/system/gateway.service`）

```ini
[Unit]
Description=云端模型路由 Go 网关
After=network.target

[Service]
Type=simple
WorkingDirectory=/opt/gateway
ExecStart=/opt/gateway/gateway
Restart=on-failure
RestartSec=2
# 环境变量（建议放在 /etc/gateway.env，chmod 600）
EnvironmentFile=/etc/gateway.env
# 安全加固
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/opt/gateway

[Install]
WantedBy=multi-user.target
```

### 环境变量（`/etc/gateway.env`，chmod 600）

```bash
# PostgreSQL（A 机）
DATABASE_URL=postgresql://gateway:你的密码@115.29.241.36:5432/gateway

# master key（32 字节 hex，openssl rand -hex 32 生成）
# 用于 AES-256-GCM 解密 PG 里加密的上游 key
GATEWAY_MASTER_KEY=你的32字节hex

# 管理 API 口令（Bearer 认证，浏览器登录面板用）
PANEL_PASSWORD=你的面板口令

# 上游 key（PG 录入后这里就不用了，留作 fallback / 本地开发）
DEEPSEEK_API_KEY=sk-xxx
MINIMAX_API_KEY=sk-cp-xxx

# 静态面板目录（默认 ../navpage，相对 WorkingDirectory）
# STATIC_DIR=/opt/gateway/navpage
```

### 录入上游 key（AES 加密存 PG）

```bash
cd /opt/gateway
sudo -E ./gateway keys set-upstream --provider deepseek   # 交互输入 key（不回显）
sudo -E ./gateway keys set-upstream --provider minimax
```

> 也可在面板「渠道」页填 `POST /api/channels/{provider}/key` 完成录入。

### Caddyfile（`/etc/caddy/Caddyfile`）

```
:80 {
    encode zstd gzip
    reverse_proxy /v1/*    127.0.0.1:8080
    reverse_proxy /api/*   127.0.0.1:8080
    # 根路径 / 落到网关内置的 http.FileServer（serve /opt/gateway/navpage）
    reverse_proxy /        127.0.0.1:8080
}
```

> 备案通过后：把 `:80` 换成域名（`maomaokingdom.top` 等），Caddy 自动 HTTPS。

也可以让 Caddy 直接 serve 静态面板（减少一次反代）：

```
:80 {
    encode zstd gzip
    reverse_proxy /v1/*    127.0.0.1:8080
    reverse_proxy /api/*   127.0.0.1:8080
    root * /opt/gateway/navpage
    file_server
}
```

### 启动

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now gateway
sudo systemctl status gateway
# 启动日志里应看到：loaded N channels / using PostgreSQL store / gateway listening on :8080

# Caddy
sudo systemctl enable --now caddy   # 已装则跳过
sudo systemctl reload caddy         # 改完 Caddyfile 后
```

---

## 渠道配置（`/opt/gateway/config.yaml`）

渠道为中心：每个渠道（provider）挂多个模型，每个模型挂多条协议路由（纯透传）+ 价格。

首次启动时网关会从 `config.yaml` 种子导入 channels/models 到 PG；之后面板增删改都走 PG，`config.yaml` 不再被读取（除了再次手动导入覆盖）。

```yaml
channels:
  - provider: deepseek
    name: DeepSeek
    balance_type: balance    # 余额型（金额） | quota（百分比）
    balance_url: https://api.deepseek.com/user/balance
    models_url: https://api.deepseek.com/models
    models:
      - name: deepseek-v4-pro
        routes:
          "/v1/messages":        { upstream: "https://api.deepseek.com/anthropic/v1/messages", model: "deepseek-v4-pro", usage: anthropic }
          "/v1/responses":       { upstream: "https://api.deepseek.com/responses",              model: "deepseek-v4-pro", usage: responses }
          "/v1/chat/completions":{ upstream: "https://api.deepseek.com/v1/chat/completions",    model: "deepseek-v4-pro", usage: chat_completions }
        pricing: { input_per_m: 3, output_per_m: 6, cache_read_per_m: 0.025, cache_write_per_m: 3 }
      - name: deepseek-v4-flash
        routes: { ... 同上 ... }
        pricing: { input_per_m: 1, output_per_m: 2, cache_read_per_m: 0.02, cache_write_per_m: 1 }

  - provider: minimax
    name: MiniMax
    balance_type: quota
    balance_url: https://api.minimaxi.com/v1/api/openplatform/coding_plan/remains
    models_url: https://api.minimaxi.com/v1/models
    models:
      - name: minimax-m3          # 客户端 model 名（直白）
        routes:
          "/v1/messages":        { upstream: "https://api.minimaxi.com/anthropic/v1/messages", model: "MiniMax-M3", usage: anthropic }
          "/v1/responses":       { upstream: "https://api.minimaxi.com/v1/responses",          model: "MiniMax-M3", usage: responses }
          "/v1/chat/completions":{ upstream: "https://api.minimaxi.com/v1/chat/completions",   model: "MiniMax-M3", usage: chat_completions }
        pricing: { input_per_m: 2.1, output_per_m: 8.4, cache_read_per_m: 0.42, cache_write_per_m: 2.625 }
```

> 注意：`minimax-m3` 是客户端可用的名字（直白），上游 model 名是 `MiniMax-M3`（MiniMax 官方大小写）。网关转发时会自动替换（`replaceModel(body, route.Model)`）。

---

## 关键坑（踩过）

- **改完 Caddyfile 必 `systemctl reload caddy`**——Caddy 不监听 SIGUSR1 自动 reload。
- **改完 .env 必 `systemctl restart gateway`**——systemd 不会自动重读 env 文件。
- **`GATEWAY_MASTER_KEY` 必须 32 字节 hex**（64 个字符）——`keys.MasterKeyFromHex` 启动校验，错就 fatal。
- **`PANEL_PASSWORD` 留空 = 管理 API 无认证**（仅开发用），生产务必设置。
- **`DATABASE_URL` 留空 = 内存模式**——自动签发一条 bootstrap key（首次启动日志可见，仅此一次）。
- **渠道配置改完 `config.yaml` 不会自动 reload 到 PG**——要么重启 gateway（启动时会重新种子导入，仅 PG 里没数据才导入），要么在面板里改（写 PG，`reloadChannels()` 立刻生效）。
- **Caddy 反代 `/` 时网关会 serve `STATIC_DIR`（默认 `../navpage`）**——确认 `WorkingDirectory=/opt/gateway`，静态面板就在 `/opt/gateway/navpage/`。
- **anthropic 协议缺 `anthropic-version` header 会被上游 401**——网关 `applyProtocolHeaders` 缺失时补 `2023-06-01` 默认值，但透传优先（客户端传了就用客户端的）。

---

## 安全

- 客户端用**带预算的 virtual key**（每个 100 元/月），**不用 master key**。
- 上游 key 在 PG 里以 **AES-256-GCM 密文** 存，`GATEWAY_MASTER_KEY` 仅 B 机 env 持有。
- A 机安全组只放行 5432 给 B；B 机只暴露 80/443。
- 面板登录用 `PANEL_PASSWORD`（Bearer 对称口令，subtle.ConstantTimeCompare 防时序攻击）。
- 虚拟 key SHA-256 哈希存 PG，明文只在签发时返回一次。

---

## 待办

- [ ] 备案通过 → 域名解析 + Caddy HTTPS
- [ ] 公开内容（博客/文档/简历）上线，替换导航页占位卡片
- [ ] `cmd/migrate` / `cmd/dbclean` 文档化（当前是裸二进制，按需使用）
