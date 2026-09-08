-- gateway 数据库 schema（对齐 docs/architecture.md §7）
-- 部署到 A 机（数据库服务器）PG 的独立库 gateway（不动 litellm 库）

-- 上游真 key：AES-256-GCM 密文，master key 解密
CREATE TABLE IF NOT EXISTS upstream_keys (
  id              BIGSERIAL PRIMARY KEY,
  provider        TEXT NOT NULL UNIQUE,   -- deepseek | minimax
  encrypted_key   BYTEA NOT NULL,         -- nonce||ciphertext(GCM tag)
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 虚拟 key（发给 agent）：只存 SHA-256 哈希
CREATE TABLE IF NOT EXISTS keys (
  id             BIGSERIAL PRIMARY KEY,
  key_hash       TEXT UNIQUE NOT NULL,
  name           TEXT NOT NULL,
  owner          TEXT,                      -- 谁拥有（多用户归属）
  agent_type     TEXT,
  quota_limit    NUMERIC(12,6),             -- 额度（USD），NULL = 不限
  quota_used     NUMERIC(12,6) NOT NULL DEFAULT 0,
  enabled        BOOLEAN NOT NULL DEFAULT TRUE,
  allowed_models TEXT NOT NULL DEFAULT '',  -- 允许访问的模型（逗号分隔，空=不限）
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at     TIMESTAMPTZ,               -- NULL = 永不过期
  last_used_at   TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS usage_logs (
  id               BIGSERIAL PRIMARY KEY,
  key_id           BIGINT REFERENCES keys(id),
  model            TEXT NOT NULL,         -- 客户端 model 名
  upstream_model   TEXT,                  -- 上游实际 model 名
  protocol         TEXT NOT NULL,         -- anthropic | responses | chat_completions
  input_tokens     INTEGER,
  output_tokens    INTEGER,
  cache_read_tokens  INTEGER,
  cache_write_tokens INTEGER,
  cost             NUMERIC(12,6),
  latency_ms       INTEGER,
  status           INTEGER,               -- HTTP 状态码（200/401/404/502...）
  error            TEXT,                  -- 失败原因（空 = 成功）
  request_id       TEXT,                  -- 客户端请求 ID
  unmetered        BOOLEAN NOT NULL DEFAULT FALSE,
  channel_id       BIGINT,                -- 最终承载响应的渠道
  account_id       BIGINT,                -- 最终承载响应的上游账号（隐式账号为 NULL）
  attempts         INTEGER NOT NULL DEFAULT 1,  -- 上游尝试次数（含首次）
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_usage_key_time ON usage_logs (key_id, created_at DESC);

-- 渠道（上游供应商）
CREATE TABLE IF NOT EXISTS channels (
  id           BIGSERIAL PRIMARY KEY,
  provider     TEXT UNIQUE NOT NULL,
  name         TEXT NOT NULL,
  balance_type TEXT NOT NULL DEFAULT 'balance',  -- balance(余额) | quota(余量百分比)
  balance_url  TEXT NOT NULL DEFAULT '',
  models_url   TEXT NOT NULL DEFAULT '',
  enabled      BOOLEAN NOT NULL DEFAULT TRUE,
  preset       JSONB NOT NULL DEFAULT '{}',     -- {endpoints:{proto:url}, pricing:{model:{...}}}
  auth_mode    TEXT NOT NULL DEFAULT 'bearer',  -- 上游认证 header：bearer(默认) | x_api_key
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 模型（客户端模型 → 路由 + 价格），挂在渠道下
CREATE TABLE IF NOT EXISTS models (
  id               BIGSERIAL PRIMARY KEY,
  channel_id       BIGINT REFERENCES channels(id) ON DELETE CASCADE,
  name             TEXT NOT NULL,               -- 客户端模型名
  routes           JSONB NOT NULL DEFAULT '{}', -- {"/v1/messages": {upstream,model,usage}}
  input_per_m      NUMERIC(12,6) NOT NULL DEFAULT 0,
  output_per_m     NUMERIC(12,6) NOT NULL DEFAULT 0,
  cache_read_per_m  NUMERIC(12,6) NOT NULL DEFAULT 0,
  cache_write_per_m NUMERIC(12,6) NOT NULL DEFAULT 0,
  enabled          BOOLEAN NOT NULL DEFAULT TRUE,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(channel_id, name)
);

-- 上游账号：渠道下的独立凭据 + 并发容量（docs/design-upstream-account-pool.md §2.1）
-- 兼容策略：渠道没有账号时回退 upstream_keys 作为隐式 default 账号
CREATE TABLE IF NOT EXISTS upstream_accounts (
  id               BIGSERIAL PRIMARY KEY,
  channel_id       BIGINT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  name             TEXT NOT NULL,
  encrypted_key    BYTEA NOT NULL,         -- AES-256-GCM 密文
  key_fingerprint  TEXT NOT NULL,          -- HMAC（服务端密钥参与），仅去重/日志关联
  max_concurrency  INTEGER NOT NULL DEFAULT 0,  -- 0 = 不限
  enabled          BOOLEAN NOT NULL DEFAULT TRUE,
  credential_type  TEXT NOT NULL DEFAULT 'api_key', -- api_key | oauth
  oauth_profile    TEXT NOT NULL DEFAULT '',        -- OAuthProfile 名称（oauth 型必填）
  encrypted_token  BYTEA,                           -- oauth 型：JSON{access_token,refresh_token,account_id,expires_at}
  token_expires_at TIMESTAMPTZ,                     -- oauth 型：access token 过期时间
  last_refresh_at  TIMESTAMPTZ,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(channel_id, name),
  UNIQUE(channel_id, key_fingerprint),
  CHECK (max_concurrency >= 0)
);
CREATE INDEX IF NOT EXISTS idx_upstream_accounts_channel ON upstream_accounts(channel_id) WHERE enabled = TRUE;

-- 既有库增量迁移（幂等）
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS channel_id BIGINT;
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS account_id BIGINT;
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS attempts INTEGER NOT NULL DEFAULT 1;

-- OAuth 凭据列（既有库幂等迁移）
ALTER TABLE upstream_accounts ADD COLUMN IF NOT EXISTS credential_type TEXT NOT NULL DEFAULT 'api_key';
ALTER TABLE upstream_accounts ADD COLUMN IF NOT EXISTS oauth_profile TEXT NOT NULL DEFAULT '';
ALTER TABLE upstream_accounts ADD COLUMN IF NOT EXISTS encrypted_token BYTEA;
ALTER TABLE upstream_accounts ADD COLUMN IF NOT EXISTS token_expires_at TIMESTAMPTZ;
ALTER TABLE upstream_accounts ADD COLUMN IF NOT EXISTS last_refresh_at TIMESTAMPTZ;

-- 虚拟 key 过期/最后使用（既有库幂等迁移）
ALTER TABLE keys ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ;
ALTER TABLE keys ADD COLUMN IF NOT EXISTS last_used_at TIMESTAMPTZ;
