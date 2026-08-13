# 部署（实际落地版）

> 本文档记录**实际部署**的架构与步骤。架构决策见 [DECISIONS.md](DECISIONS.md)。

## 架构总览

```
服务器 A（女朋友账号，2核2G，数据节点）
  115.29.241.36
  ├── litellm（docker）      :4000 网关（DeepSeek + MiniMax，8 模型）
  ├── litellm-db（postgres） 内部网，虚拟 keys + spend tracking
  └── litellm-spend-panel    :8080 消耗统计面板

服务器 B（毛庆辉账号，2核2G，导航页）
  121.40.184.111
  └── caddy（docker）        :80 serve 导航页（备案通过后换 HTTPS）
```

## 服务器 A：LiteLLM + PostgreSQL

### 1. 目录结构

```
/opt/litellm/
├── docker-compose.yml
├── config.yaml        # 模型路由表（8 条 + 计费）
├── .env               # master key / salt key / PG 密码 / 供应商 key
└── data/pgdata/       # PostgreSQL 数据卷
```

### 2. docker-compose.yml

```yaml
services:
  litellm:
    image: ghcr.io/berriai/litellm-database:latest   # 注意：database 版，非 main-latest
    container_name: litellm
    restart: always
    ports:
      - "4000:4000"
    env_file:
      - .env
    environment:
      - DATABASE_URL=postgresql://${POSTGRES_USER}:${POSTGRES_PASSWORD}@db:5432/${POSTGRES_DB}
      - STORE_MODEL_IN_DB=True
      - AIOHTTP_CONNECTOR_LIMIT=300           # 内存优化（2G 机器）
      - AIOHTTP_CONNECTOR_LIMIT_PER_HOST=50
      - AIOHTTP_KEEPALIVE_TIMEOUT=120
      - MAX_SIZE_IN_MEMORY_QUEUE=800
      - LITELLM_ASYNCIO_QUEUE_MAXSIZE=1000
      - MAX_REQUESTS_BEFORE_RESTART=50000
    volumes:
      - ./config.yaml:/app/config.yaml
    depends_on:
      db:
        condition: service_healthy
    command:
      - "--config" - "/app/config.yaml" - "--port" - "4000" - "--telemetry" - "False"

  db:
    image: postgres:16-alpine
    container_name: litellm-db
    restart: always
    volumes:
      - postgres_data:/var/lib/postgresql/data
    env_file:
      - .env
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U ${POSTGRES_USER}"]
      interval: 5s
      timeout: 3s
      retries: 10

volumes:
  postgres_data:
```

### 3. .env

```bash
LITELLM_MASTER_KEY=sk-xxx          # 管理端（UI 登录密码 = 这个）
LITELLM_SALT_KEY=sk-xxx            # 必须设，加密存 DB 的凭据
POSTGRES_USER=litellm
POSTGRES_PASSWORD=xxx
POSTGRES_DB=litellm
DEEPSEEK_API_KEY=sk-xxx
MINIMAX_API_KEY=sk-cp-xxx          # Coding Plan 的 key 是 sk-cp- 前缀
```

### 4. 模型路由（config.yaml 要点）

- 命名三段式：`供应商:模型名(协议)`，见 [DECISIONS.md 决策 8](DECISIONS.md)
- 每模型 × 每协议端点 = 一条，共 8 条
- 前缀用**原生 provider**：`deepseek/`、`minimax/`、`anthropic/`（不要用 `openai/` 套 MiniMax，认证会错）
- MiniMax 必须覆盖 `api_base` 到国内端点（否则默认打国际 `api.minimax.io` → 401）
- 每个 `model_info` 配 `input_cost_per_token` / `output_cost_per_token`（人民币元/token）+ `custom_pricing: true`，否则 spend 计费为 0

### 5. 国内镜像加速

| 镜像                           | 拉取方式                                          |
| ------------------------------ | ------------------------------------------------- |
| `ghcr.io/.../litellm-database` | `ghcr.nju.edu.cn/...` 加速后 tag 回原名           |
| `postgres:16-alpine`           | `docker.m.daocloud.io/library/postgres:16-alpine` |
| 其他 Docker Hub 镜像           | `dockerproxy.net/...`（daocloud 有白名单限制）    |

### 6. 关键坑（踩过）

- **用 `litellm-database` 镜像，不是 `main-latest`**：后者不带 Prisma migration，每次启动全量 `db push` 会 OOM（2G 机器）。
- **`--database_url` CLI 参数已移除**：数据库 URL 放 config.yaml 的 `general_settings.database_url` 或 `DATABASE_URL` 环境变量。
- **改了 `.env` 要 `docker compose up -d` 重建容器**，`restart` 不重新注入 env_file。
- **首次启动会自动 `prisma migrate`**，健康检查要等 1-2 分钟。

## 服务器 A：消耗统计面板

独立仓库 `litellm-spend-dashboard`（FastAPI + 自绘 SVG，Docker 部署，端口 8080）。见该仓库 README。

## 服务器 B：导航页（猫猫王国）

- 独立仓库 `navpage/`（单 HTML，玻璃拟态）
- Caddy `file_server` 托管 `/opt/site/index.html`
- 备案通过后：Caddyfile 换成域名 + 自动 HTTPS + 子域名反代

## 安全

- 客户端用**带预算的 virtual key**（每个 100 元/月），**不用 master key**
- master key 只做管理（生成/吊销 key、看报表）
- 面板有自己的登录密码；导航页公开，私有服务靠各自登录
- 女朋友服务器 4000/8080 端口：备案完成后收窄到只对服务器 B 开放

## 待办

- [ ] 备案通过 → 域名解析 + HTTPS + 子域名反代（`ui.` / `panel.maomaokingdom.top`）
- [ ] 公开内容（博客/文档/简历）上线，替换导航页占位卡片
- [ ] 仓库根目录 `config.yaml` 同步成服务器上的实际 8 条配置
