# 部署指南

RelayDock 是单二进制网关：前端构建产物内嵌目录 serve，存储默认 SQLite（零外部依赖），可选 PostgreSQL / 内存。

## 快速开始（本地）

```bash
# 方式一：源码
git clone <repo> && cd relaydock
./install.sh                  # 构建前端 + 网关 + 生成 .env
source .env
cd gateway && ../bin/relaydock

# 方式二：Docker
PANEL_PASSWORD=你的口令 docker compose up -d
```

控制台：`http://localhost:8080`。首次启动自动建表并从 `gateway/config.yaml` 种子渠道。

## 存储三态

| 模式 | 触发 | 数据 | 适用 |
| --- | --- | --- | --- |
| **SQLite（默认）** | 不设任何变量 | `SQLITE_PATH`（默认 `data/relaydock.db`），WAL 模式 | 个人/小团队，数据持久 + 零依赖 |
| PostgreSQL | 设 `DATABASE_URL` | 外部 PG | 多实例/已有 PG |
| 内存 | 设 `MEMORY=1` | 重启清零 | 快速试用/测试 |

## 环境变量

| 变量 | 必填 | 说明 |
| --- | --- | --- |
| `PANEL_PASSWORD` | ✅ | 控制台与管理 API 口令（不设 = 无认证，仅限本机调试） |
| `GATEWAY_MASTER_KEY` | 建议 | 64 位 hex（`openssl rand -hex 32`）；上游凭据加解密。内存模式未设时自动生成一次性密钥 |
| `SQLITE_PATH` | — | SQLite 文件路径（默认 `data/relaydock.db`） |
| `DATABASE_URL` | — | 设了则用 PG（优先于 SQLite） |
| `MEMORY` | — | `1` = 内存模式 |
| `STATIC_DIR` | — | 可选：外置控制台目录覆盖。默认控制台已内嵌二进制，无需设置 |
| `ALLOW_NO_AUTH` | — | `1` = 允许空口令启动（管理 API 无认证，且只监听回环；仅本机调试） |

## 配置文件

`gateway/config.yaml`（种子数据，进 PG/SQLite 后以库为准，控制台热更新）：

- `channels`：渠道 = 上游供应商 → 模型路由（三协议）→ 价格（¥/M，缓存读写独立单价）
- `oauth_profiles`：OAuth 订阅账号授权参数（codex 已内置，参数取自官方 CLI 公开常量）

结构详见 [architecture.md](architecture.md) §5。

## 安全清单

- [ ] `PANEL_PASSWORD` 用强口令（控制台全部管理操作的唯一门禁）
- [ ] `GATEWAY_MASTER_KEY` 妥善保管：泄露 = 上游凭据泄露；丢失 = 已存凭据不可解密
- [ ] 公网入口走 TLS 反代（Caddy 示例见下），不要裸露 8080
- [ ] 下游 agent 只发虚拟 key（`gw-`），上游真 key 永不出网关
- [ ] 全文请求/响应日志默认关闭；开启后内容含对话，按保留期自动清理

## 升级与备份

- **升级**：换二进制重启即可。schema 迁移全部幂等（`CREATE IF NOT EXISTS` + `ADD COLUMN IF NOT EXISTS`），启动时自动执行
- **备份**：停服务复制 `data/relaydock.db`（或 WAL 模式下连同 `-wal`/`-shm`），或 `sqlite3 data/relaydock.db ".backup backup.db"`
- **版本**：`GET /api/version`、`/healthz` 返回构建版本（CI/Docker 构建自动注入）

## 反向代理（Caddy）

```caddy
your-domain.com {
    # TLS 自动（Let's Encrypt）
    reverse_proxy /v1/*  127.0.0.1:8080
    reverse_proxy /api/* 127.0.0.1:8080
    reverse_proxy /healthz 127.0.0.1:8080
    root * /opt/relaydock/web/dist
    file_server
}
```

也可全部交给网关内置 FileServer（`root *` 一行省略，Caddy 仅做 TLS 反代 `/`）。

## 示例：双服务器部署（展示 + 存储分离）

```
B 机（公网入口）：Caddy(TLS) → relaydock(:8080, SQLite 或连 A 机 PG)
A 机（内网）：PostgreSQL :5432（安全组只信任 B 机）
```

- relaydock systemd 单元要点：`WorkingDirectory=/opt/relaydock/gateway`（config.yaml 所在）、`Environment=PANEL_PASSWORD/GATEWAY_MASTER_KEY/DATABASE_URL`
- 控制台静态文件：`STATIC_DIR=/opt/relaydock/web/dist`
- 客户端接入（agent 侧）：`ANTHROPIC_BASE_URL`/`base_url` 指向本域名 + 虚拟 key，见 [clients.md](clients.md)
