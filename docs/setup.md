# 部署（实际落地版）

> 本文档记录**实际部署**的架构与步骤。架构决策见 [DECISIONS.md](DECISIONS.md)。

## 架构总览（三层架构）

```
服务器 A（女朋友账号，2核2G）— 数据层（隐藏）
  115.29.241.36
  └── postgres :5432   只放行 B 的 IP

服务器 B（毛庆辉账号，2核2G）— 应用 + 展示 + 入口
  121.40.184.111
  ├── litellm  4000（expose，连 A 的 PG）
  ├── panel    8080（expose，连 litellm）
  └── caddy    80/443（唯一公网入口）
      ├── /            → 导航页（/opt/site）
      ├── /gateway/*   → litellm（SERVER_ROOT_PATH=/gateway）
      └── /panel/*     → panel（caddy 剥前缀）
```

## 服务器 A：纯 PostgreSQL（数据层）

### docker-compose.yml

```yaml
services:
  db:
    image: postgres:16-alpine
    container_name: litellm-db
    restart: always
    ports:
      - "5432:5432" # 只放行 B 的 IP（安全组控制）
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

### .env

```bash
POSTGRES_USER=litellm
POSTGRES_PASSWORD=xxx
POSTGRES_DB=litellm
```

### 安全组

- `5432` → 只放行 B 的 IP（`121.40.184.111/32`）
- `22` → 你的 IP（SSH，密钥登录）
- 删 RDP / ICMP 等默认规则

## 服务器 B：LiteLLM + 面板 + Caddy

### 目录结构

```
/opt/litellm/
├── docker-compose.yml
├── config.yaml        # 8 条模型 + 计费（见仓库根 config.yaml）
├── .env               # master/salt key + DATABASE_URL + 供应商 key + panel 密码
└── Caddyfile          # 子路径反代
```

### docker-compose.yml

```yaml
services:
  litellm:
    image: ghcr.io/berriai/litellm-database:latest # database 版，非 main-latest
    container_name: litellm
    restart: always
    expose: ["4000"] # 只内网，不映射公网
    env_file: [".env"]
    environment:
      - STORE_MODEL_IN_DB=True
      - SERVER_ROOT_PATH=/gateway # 子路径反代
      - AIOHTTP_CONNECTOR_LIMIT=300 # 内存优化（2G 机器）
      - AIOHTTP_CONNECTOR_LIMIT_PER_HOST=50
      - AIOHTTP_KEEPALIVE_TIMEOUT=120
      - MAX_SIZE_IN_MEMORY_QUEUE=800
      - LITELLM_ASYNCIO_QUEUE_MAXSIZE=1000
      - MAX_REQUESTS_BEFORE_RESTART=50000
    volumes:
      - ./config.yaml:/app/config.yaml
    command:
      ["--config", "/app/config.yaml", "--port", "4000", "--telemetry", "False"]

  panel:
    image: litellm-spend-panel:latest # 自 build（litellm-spend-dashboard 仓库）
    container_name: litellm-spend-panel
    restart: always
    expose: ["8080"]
    env_file: [".env"]
    depends_on: [litellm]

  caddy:
    image: caddy:2
    container_name: caddy
    restart: always
    ports: ["80:80", "443:443"] # 唯一对外
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile
      - /opt/site:/srv
      - caddy_data:/data
      - caddy_config:/config

volumes:
  caddy_data:
  caddy_config:
```

### .env

```bash
LITELLM_MASTER_KEY=sk-xxx       # 管理端（UI 登录密码 = 这个）
LITELLM_SALT_KEY=sk-xxx         # 必须与 A 一致（DB 里凭据用 salt 加密）
DATABASE_URL=postgresql://litellm:PASS@115.29.241.36:5432/litellm   # 连 A 的 PG
DEEPSEEK_API_KEY=sk-xxx
MINIMAX_API_KEY=sk-cp-xxx       # Coding Plan 的 key 是 sk-cp- 前缀
PANEL_PASSWORD=xxx              # 面板登录密码
LITELLM_BASE_URL=http://litellm:4000
```

### Caddyfile（子路径反代）

```
:80 {
	handle_path /panel/* {
		reverse_proxy panel:8080
	}
	handle /gateway/* {
		reverse_proxy litellm:4000
	}
	handle {
		root * /srv
		file_server
	}
}
```

> 备案通过后：把 `:80` 换成域名（`maomaokingdom.top` 等），Caddy 自动 HTTPS。

## 国内镜像加速

| 镜像                                      | 拉取方式                                       |
| ----------------------------------------- | ---------------------------------------------- |
| `ghcr.io/.../litellm-database`            | `ghcr.nju.edu.cn/...` 加速后 tag 回原名        |
| `postgres:16-alpine` / `python:3.11-slim` | `docker.m.daocloud.io/library/...`             |
| 其他 Docker Hub 镜像                      | `dockerproxy.net/...`（daocloud 有白名单限制） |

## 关键坑（踩过）

- **用 `litellm-database` 镜像，不是 `main-latest`**：后者不带 Prisma migration，每次启动全量 `db push` 会 OOM（2G 机器）。
- **`--database_url` CLI 参数已移除**：数据库 URL 用 `DATABASE_URL` 环境变量。
- **改了 `.env` 要 `docker compose up -d` 重建容器**，`restart` 不重新注入 env_file。
- **改了 Caddyfile 要 `docker restart caddy`**，volume 挂载不自动重载。
- **`SERVER_ROOT_PATH`** 让 LiteLLM 同时接受带/不带 `/gateway` 前缀的路径（v1.81.3 之前有 UI 404 bug，现已修复）。
- **面板 `--root-path` 不剥前缀**：面板是根路径，要用 caddy `handle_path`（剥 `/panel`）反代，不能 `handle`（保留路径）。

## 安全

- 客户端用**带预算的 virtual key**（每个 100 元/月），**不用 master key**
- master key 只做管理（生成/吊销 key、看报表）
- A 只暴露 5432 给 B；B 只暴露 80/443
- 面板有自己的登录密码；导航页公开，私有服务靠各自登录

## 待办

- [ ] 备案通过 → 域名解析 + Caddy HTTPS
- [ ] 公开内容（博客/文档/简历）上线，替换导航页占位卡片
