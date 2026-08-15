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
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
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
