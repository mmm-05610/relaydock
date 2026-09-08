package store

// SqliteStore 内嵌 SQLite 存储（modernc.org/sqlite 纯 Go，无 CGO）。
// 定位：个人/小团队的默认形态——数据持久化 + 零外部依赖，Open 时自动建表。
// 时间列一律 INTEGER（unix 秒），避免 SQLite 文本时间的比较/时区陷阱。

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"gateway/internal/config"
	"gateway/internal/keys"
)

type SqliteStore struct {
	db *sql.DB
}

const sqliteSchema = `
CREATE TABLE IF NOT EXISTS keys (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  key_hash TEXT UNIQUE NOT NULL,
  name TEXT NOT NULL,
  owner TEXT NOT NULL DEFAULT '',
  agent_type TEXT NOT NULL DEFAULT '',
  quota_limit REAL NOT NULL DEFAULT 0,
  quota_used REAL NOT NULL DEFAULT 0,
  enabled BOOLEAN NOT NULL DEFAULT 1,
  allowed_models TEXT NOT NULL DEFAULT '',
  expires_at INTEGER,
  last_used_at INTEGER,
  created_at INTEGER DEFAULT (strftime('%s','now'))
);
CREATE TABLE IF NOT EXISTS upstream_keys (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  provider TEXT UNIQUE NOT NULL,
  encrypted_key BLOB NOT NULL,
  created_at INTEGER DEFAULT (strftime('%s','now'))
);
CREATE TABLE IF NOT EXISTS usage_logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  key_id INTEGER,
  model TEXT NOT NULL DEFAULT '',
  upstream_model TEXT,
  protocol TEXT NOT NULL DEFAULT '',
  input_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  cache_read_tokens INTEGER NOT NULL DEFAULT 0,
  cache_write_tokens INTEGER NOT NULL DEFAULT 0,
  cost REAL NOT NULL DEFAULT 0,
  latency_ms INTEGER NOT NULL DEFAULT 0,
  status INTEGER,
  error TEXT,
  request_id TEXT,
  unmetered BOOLEAN NOT NULL DEFAULT 0,
  channel_id INTEGER,
  account_id INTEGER,
  attempts INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_usage_key_time ON usage_logs(key_id, created_at);
CREATE TABLE IF NOT EXISTS channels (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  provider TEXT UNIQUE NOT NULL,
  name TEXT NOT NULL,
  balance_type TEXT NOT NULL DEFAULT 'balance',
  balance_url TEXT NOT NULL DEFAULT '',
  models_url TEXT NOT NULL DEFAULT '',
  enabled BOOLEAN NOT NULL DEFAULT 1,
  preset TEXT NOT NULL DEFAULT '{}',
  auth_mode TEXT NOT NULL DEFAULT 'bearer',
  created_at INTEGER DEFAULT (strftime('%s','now'))
);
CREATE TABLE IF NOT EXISTS models (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  channel_id INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  routes TEXT NOT NULL DEFAULT '{}',
  input_per_m REAL NOT NULL DEFAULT 0,
  output_per_m REAL NOT NULL DEFAULT 0,
  cache_read_per_m REAL NOT NULL DEFAULT 0,
  cache_write_per_m REAL NOT NULL DEFAULT 0,
  enabled BOOLEAN NOT NULL DEFAULT 1,
  created_at INTEGER DEFAULT (strftime('%s','now')),
  UNIQUE(channel_id, name)
);
CREATE TABLE IF NOT EXISTS upstream_accounts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  channel_id INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  encrypted_key BLOB,
  key_fingerprint TEXT NOT NULL DEFAULT '',
  max_concurrency INTEGER NOT NULL DEFAULT 0,
  enabled BOOLEAN NOT NULL DEFAULT 1,
  credential_type TEXT NOT NULL DEFAULT 'api_key',
  oauth_profile TEXT NOT NULL DEFAULT '',
  encrypted_token BLOB,
  token_expires_at INTEGER,
  last_refresh_at INTEGER,
  created_at INTEGER DEFAULT (strftime('%s','now')),
  updated_at INTEGER DEFAULT (strftime('%s','now')),
  UNIQUE(channel_id, name),
  UNIQUE(channel_id, key_fingerprint)
);
CREATE TABLE IF NOT EXISTS request_bodies (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  usage_request_id TEXT,
  model TEXT NOT NULL DEFAULT '',
  request_body BLOB,
  response_body BLOB,
  truncated BOOLEAN NOT NULL DEFAULT 0,
  status INTEGER,
  created_at INTEGER DEFAULT (strftime('%s','now'))
);
CREATE INDEX IF NOT EXISTS idx_request_bodies_usage_rid ON request_bodies(usage_request_id, created_at);
CREATE TABLE IF NOT EXISTS settings (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
`

func NewSqliteStore(path string) (*SqliteStore, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create sqlite dir: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.Exec(sqliteSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate sqlite: %w", err)
	}
	s := &SqliteStore{db: db}
	log.Printf("sqlite store ready: %s", path)
	return s, nil
}

func (s *SqliteStore) Close() error { return s.db.Close() }

// toUnix 时间 → unix 秒（零值 → nil）。
func toUnix(t any) any {
	switch v := t.(type) {
	case *time.Time:
		if v == nil || v.IsZero() {
			return nil
		}
		return v.Unix()
	case time.Time:
		if v.IsZero() {
			return nil
		}
		return v.Unix()
	}
	return t
}

func fromUnix(n sql.NullInt64) time.Time {
	if !n.Valid || n.Int64 <= 0 {
		return time.Time{}
	}
	return time.Unix(n.Int64, 0)
}

// --- keys.Store ---

func (s *SqliteStore) CreateKey(k keys.Key) error {
	_, err := s.db.Exec(
		`INSERT INTO keys (key_hash, name, owner, agent_type, quota_limit, quota_used, enabled, allowed_models, expires_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		k.KeyHash, k.Name, k.Owner, k.AgentType, k.QuotaLimit, 0, k.Enabled, k.AllowedModels, toUnix(k.ExpiresAt))
	return err
}

func (s *SqliteStore) GetKeyByHash(hash string) (*keys.Key, error) {
	var k keys.Key
	var quotaLimit sql.NullFloat64
	var expires, lastUsed sql.NullInt64
	err := s.db.QueryRow(
		`SELECT id, key_hash, name, owner, agent_type, quota_limit, quota_used, enabled, allowed_models, expires_at, last_used_at
		 FROM keys WHERE key_hash = ?`, hash).
		Scan(&k.ID, &k.KeyHash, &k.Name, &k.Owner, &k.AgentType, &quotaLimit, &k.QuotaUsed, &k.Enabled, &k.AllowedModels, &expires, &lastUsed)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if quotaLimit.Valid {
		k.QuotaLimit = quotaLimit.Float64
	}
	k.ExpiresAt = ptrTime(fromUnix(expires))
	k.LastUsedAt = ptrTime(fromUnix(lastUsed))
	return &k, nil
}

func (s *SqliteStore) ListKeys() ([]keys.Key, error) {
	rows, err := s.db.Query(
		`SELECT id, key_hash, name, owner, agent_type, quota_limit, quota_used, enabled, allowed_models, expires_at, last_used_at FROM keys ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []keys.Key
	for rows.Next() {
		var k keys.Key
		var quotaLimit sql.NullFloat64
		var expires, lastUsed sql.NullInt64
		if err := rows.Scan(&k.ID, &k.KeyHash, &k.Name, &k.Owner, &k.AgentType, &quotaLimit, &k.QuotaUsed, &k.Enabled, &k.AllowedModels, &expires, &lastUsed); err != nil {
			return nil, err
		}
		if quotaLimit.Valid {
			k.QuotaLimit = quotaLimit.Float64
		}
		k.ExpiresAt = ptrTime(fromUnix(expires))
		k.LastUsedAt = ptrTime(fromUnix(lastUsed))
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *SqliteStore) RevokeKey(hash string) error {
	_, err := s.db.Exec(`UPDATE keys SET enabled = 0 WHERE key_hash = ?`, hash)
	return err
}

func (s *SqliteStore) AddQuotaUsed(hash string, amount float64) error {
	_, err := s.db.Exec(`UPDATE keys SET quota_used = quota_used + ? WHERE key_hash = ?`, amount, hash)
	return err
}

func (s *SqliteStore) UpdateQuota(hash string, limit float64) error {
	_, err := s.db.Exec(`UPDATE keys SET quota_limit = ? WHERE key_hash = ?`, limit, hash)
	return err
}

func (s *SqliteStore) UpdateKey(k keys.Key) error {
	_, err := s.db.Exec(
		`UPDATE keys SET name=?, owner=?, agent_type=?, quota_limit=?, allowed_models=?, expires_at=? WHERE key_hash=?`,
		k.Name, k.Owner, k.AgentType, k.QuotaLimit, k.AllowedModels, toUnix(k.ExpiresAt), k.KeyHash)
	return err
}

func (s *SqliteStore) TouchLastUsed(hash string) error {
	_, err := s.db.Exec(`UPDATE keys SET last_used_at = strftime('%s','now') WHERE key_hash = ?`, hash)
	return err
}

// --- UpstreamStore ---

func (s *SqliteStore) SetUpstreamKey(provider string, encrypted []byte) error {
	_, err := s.db.Exec(
		`INSERT INTO upstream_keys (provider, encrypted_key) VALUES (?,?)
		 ON CONFLICT(provider) DO UPDATE SET encrypted_key = excluded.encrypted_key`, provider, encrypted)
	return err
}

func (s *SqliteStore) GetUpstreamKey(provider string) ([]byte, error) {
	var enc []byte
	err := s.db.QueryRow(`SELECT encrypted_key FROM upstream_keys WHERE provider = ?`, provider).Scan(&enc)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return enc, nil
}

// --- AccountStore ---

func (s *SqliteStore) ListUpstreamAccounts() ([]UpstreamAccount, error) {
	rows, err := s.db.Query(
		`SELECT id, channel_id, name, encrypted_key, key_fingerprint, max_concurrency, enabled,
		        credential_type, oauth_profile, encrypted_token, token_expires_at, last_refresh_at
		 FROM upstream_accounts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UpstreamAccount
	for rows.Next() {
		var a UpstreamAccount
		var tokenExp, lastRefresh sql.NullInt64
		if err := rows.Scan(&a.ID, &a.ChannelID, &a.Name, &a.EncryptedKey, &a.KeyFingerprint, &a.MaxConcurrency, &a.Enabled,
			&a.CredentialType, &a.OAuthProfile, &a.EncryptedToken, &tokenExp, &lastRefresh); err != nil {
			return nil, err
		}
		a.TokenExpiresAt = fromUnix(tokenExp)
		a.LastRefreshAt = fromUnix(lastRefresh)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *SqliteStore) CreateUpstreamAccount(a *UpstreamAccount) error {
	res, err := s.db.Exec(
		`INSERT INTO upstream_accounts (channel_id, name, encrypted_key, key_fingerprint, max_concurrency, enabled,
		                                credential_type, oauth_profile, encrypted_token, token_expires_at, last_refresh_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		a.ChannelID, a.Name, a.EncryptedKey, a.KeyFingerprint, a.MaxConcurrency, a.Enabled,
		a.CredentialType, a.OAuthProfile, a.EncryptedToken, toUnix(a.TokenExpiresAt), toUnix(a.LastRefreshAt))
	if err != nil {
		return err
	}
	a.ID, err = res.LastInsertId()
	return err
}

func (s *SqliteStore) UpdateUpstreamAccount(a UpstreamAccount) error {
	_, err := s.db.Exec(
		`UPDATE upstream_accounts SET name=?, encrypted_key=?, key_fingerprint=?, max_concurrency=?, enabled=?, updated_at=strftime('%s','now') WHERE id=?`,
		a.Name, a.EncryptedKey, a.KeyFingerprint, a.MaxConcurrency, a.Enabled, a.ID)
	return err
}

func (s *SqliteStore) DeleteUpstreamAccount(id int64) error {
	_, err := s.db.Exec(`DELETE FROM upstream_accounts WHERE id=?`, id)
	return err
}

func (s *SqliteStore) SetUpstreamToken(id int64, encryptedToken []byte, expiresAt time.Time) error {
	_, err := s.db.Exec(
		`UPDATE upstream_accounts SET encrypted_token=?, token_expires_at=?, last_refresh_at=strftime('%s','now') WHERE id=?`,
		encryptedToken, toUnix(expiresAt), id)
	return err
}

func (s *SqliteStore) AccountUsageStats(days int) ([]AccountUsage, error) {
	rows, err := s.db.Query(
		`SELECT account_id, count(*), coalesce(sum(cost),0),
		        coalesce(sum(input_tokens),0)+coalesce(sum(output_tokens),0)+coalesce(sum(cache_read_tokens),0)+coalesce(sum(cache_write_tokens),0),
		        coalesce(avg(attempts),1),
		        count(*) FILTER (WHERE status >= 400)
		 FROM usage_logs
		 WHERE account_id IS NOT NULL AND created_at > ?
		 GROUP BY account_id`, time.Now().AddDate(0, 0, -days).Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccountUsage
	for rows.Next() {
		var a AccountUsage
		if err := rows.Scan(&a.AccountID, &a.Requests, &a.Cost, &a.Tokens, &a.AvgAttempts, &a.Errors); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// --- UsageStore ---

func (s *SqliteStore) InsertUsageLog(log UsageLog) error {
	_, err := s.db.Exec(
		`INSERT INTO usage_logs (key_id, model, upstream_model, protocol, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost, latency_ms, status, error, request_id, unmetered, channel_id, account_id, attempts, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?, coalesce(?, strftime('%s','now')))`,
		nullInt64(log.KeyID), log.Model, log.UpstreamModel, log.Protocol, log.InputTokens, log.OutputTokens,
		log.CacheReadTokens, log.CacheWriteTokens, log.Cost, log.LatencyMs, log.Status, log.Error, log.RequestID, log.Unmetered,
		nullInt64(log.ChannelID), nullInt64(log.AccountID), maxInt(log.Attempts, 1), toUnix(log.CreatedAt))
	return err
}

func (s *SqliteStore) GetUsageStats() (UsageStats, error) {
	var st UsageStats
	err := s.db.QueryRow(
		`SELECT coalesce(sum(cost),0),
		        coalesce(sum(input_tokens),0)+coalesce(sum(output_tokens),0)+coalesce(sum(cache_read_tokens),0)+coalesce(sum(cache_write_tokens),0),
		        count(*)
		 FROM usage_logs`).Scan(&st.TotalCost, &st.TotalTokens, &st.TotalRequests)
	if err != nil {
		return st, err
	}
	rows, err := s.db.Query(
		`SELECT model, coalesce(sum(cost),0), coalesce(sum(input_tokens),0), coalesce(sum(output_tokens),0),
		        coalesce(sum(input_tokens),0)+coalesce(sum(output_tokens),0)+coalesce(sum(cache_read_tokens),0)+coalesce(sum(cache_write_tokens),0), count(*),
		        coalesce(avg(CASE WHEN status < 400 THEN 1.0 ELSE 0.0 END),0)
		 FROM usage_logs WHERE model <> '' GROUP BY model ORDER BY sum(cost) DESC`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var m ModelUsage
		if err := rows.Scan(&m.Model, &m.Cost, &m.InputTokens, &m.OutputTokens, &m.TotalTokens, &m.Requests, &m.SuccessRate); err != nil {
			return st, err
		}
		st.ByModel = append(st.ByModel, m)
	}
	rows2, err := s.db.Query(
		`SELECT u.key_id, coalesce(k.name,''), coalesce(k.owner,''), coalesce(sum(u.cost),0),
		        coalesce(sum(u.input_tokens),0)+coalesce(sum(u.output_tokens),0)+coalesce(sum(u.cache_read_tokens),0)+coalesce(sum(u.cache_write_tokens),0), count(*)
		 FROM usage_logs u LEFT JOIN keys k ON u.key_id = k.id
		 WHERE u.key_id IS NOT NULL
		 GROUP BY u.key_id, k.name, k.owner ORDER BY sum(u.cost) DESC`)
	if err != nil {
		return st, err
	}
	defer rows2.Close()
	for rows2.Next() {
		var k KeyUsage
		if err := rows2.Scan(&k.KeyID, &k.Name, &k.Owner, &k.Cost, &k.Tokens, &k.Requests); err != nil {
			return st, err
		}
		st.ByKey = append(st.ByKey, k)
	}
	return st, nil
}

func (s *SqliteStore) GetDashboard() (DashboardStats, error) {
	var st DashboardStats
	startOfDay := time.Now().Truncate(24 * time.Hour).Unix()
	err := s.db.QueryRow(
		`SELECT coalesce(sum(cost),0),
		        coalesce(sum(input_tokens),0)+coalesce(sum(output_tokens),0)+coalesce(sum(cache_read_tokens),0)+coalesce(sum(cache_write_tokens),0),
		        count(*),
		        coalesce(avg(CASE WHEN status < 400 THEN 1.0 ELSE 0.0 END),0),
		        coalesce(avg(latency_ms),0),
		        coalesce(sum(cache_read_tokens),0),
		        CASE WHEN coalesce(sum(input_tokens),0)+coalesce(sum(cache_read_tokens),0) > 0
		             THEN coalesce(sum(cache_read_tokens),0) / (coalesce(sum(input_tokens),0)+coalesce(sum(cache_read_tokens),0))
		             ELSE 0 END
		 FROM usage_logs WHERE created_at >= ?`, startOfDay).
		Scan(&st.TodayCost, &st.TodayTokens, &st.TodayRequests, &st.SuccessRate, &st.AvgLatencyMs, &st.CacheReadTokens, &st.CacheHitRate)
	if err != nil {
		return st, err
	}
	st.RecentLogs, err = s.QueryLogs(LogFilter{Limit: 5})
	return st, err
}

func (s *SqliteStore) GetTimeseries(r TimeRange) ([]TimeseriesPoint, error) {
	q, args := "SELECT strftime('%m-%d', created_at, 'unixepoch'), coalesce(sum(cost),0), coalesce(sum(input_tokens),0)+coalesce(sum(output_tokens),0)+coalesce(sum(cache_read_tokens),0)+coalesce(sum(cache_write_tokens),0), count(*) FROM usage_logs WHERE 1=1", []any{}
	if r.From != "" && r.To != "" {
		from, _ := time.Parse("2006-01-02", r.From)
		to, _ := time.Parse("2006-01-02", r.To)
		to = to.AddDate(0, 0, 1)
		q += " AND created_at >= ? AND created_at < ?"
		args = append(args, from.Unix(), to.Unix())
	} else if r.Days > 0 {
		q += " AND created_at >= ?"
		args = append(args, time.Now().AddDate(0, 0, -(r.Days-1)).Truncate(24*time.Hour).Unix())
	}
	q += " GROUP BY strftime('%m-%d', created_at, 'unixepoch') ORDER BY 1"
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TimeseriesPoint{}
	for rows.Next() {
		var p TimeseriesPoint
		if err := rows.Scan(&p.Date, &p.Cost, &p.Tokens, &p.Requests); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *SqliteStore) GetGrouped(by string, r TimeRange) ([]GroupedUsage, error) {
	var col string
	switch by {
	case "key":
		col = "(SELECT coalesce(name, 'key-' || u.key_id) FROM keys k WHERE k.id = u.key_id)"
	case "protocol":
		col = "u.protocol"
	case "owner":
		col = "(SELECT coalesce(owner, '-') FROM keys k WHERE k.id = u.key_id)"
	default:
		col = "u.model"
	}
	q, args := "SELECT "+col+", count(*), coalesce(sum(u.input_tokens),0)+coalesce(sum(u.output_tokens),0)+coalesce(sum(u.cache_read_tokens),0)+coalesce(sum(u.cache_write_tokens),0), coalesce(sum(u.cost),0), coalesce(avg(CASE WHEN u.status < 400 THEN 1.0 ELSE 0.0 END),0), coalesce(sum(u.cache_read_tokens),0), coalesce(avg(u.latency_ms),0) FROM usage_logs u WHERE 1=1", []any{}
	if r.From != "" && r.To != "" {
		from, _ := time.Parse("2006-01-02", r.From)
		to, _ := time.Parse("2006-01-02", r.To)
		q += " AND u.created_at >= ? AND u.created_at < ?"
		args = append(args, from.Unix(), to.AddDate(0, 0, 1).Unix())
	} else if r.Days > 0 {
		q += " AND u.created_at >= ?"
		args = append(args, time.Now().AddDate(0, 0, -r.Days).Unix())
	}
	q += " GROUP BY 1 ORDER BY 2 DESC"
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GroupedUsage{}
	for rows.Next() {
		var g GroupedUsage
		if err := rows.Scan(&g.Group, &g.Requests, &g.Tokens, &g.Cost, &g.SuccessRate, &g.CacheReadTokens, &g.AvgLatencyMs); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *SqliteStore) GetUsageOverview(days int) (*UsageOverview, error) {
	cutoff := time.Now().AddDate(0, 0, -days).Unix()
	gran := "%Y-%m-%d"
	if days <= 2 {
		gran = "%Y-%m-%dT%H:00:00"
	}
	ov := &UsageOverview{}
	err := s.db.QueryRow(
		`SELECT count(*), count(*) FILTER (WHERE status >= 400),
		        coalesce(sum(input_tokens),0), coalesce(sum(output_tokens),0),
		        coalesce(sum(cache_read_tokens),0), coalesce(sum(cache_write_tokens),0),
		        coalesce(sum(cost),0)
		 FROM usage_logs WHERE created_at > ?`, cutoff).Scan(
		&ov.Summary.Requests, &ov.Summary.Errors,
		&ov.Summary.InputTokens, &ov.Summary.OutputTokens,
		&ov.Summary.CacheReadTokens, &ov.Summary.CacheWriteTokens, &ov.Summary.Cost)
	if err != nil {
		return nil, err
	}
	ov.Summary.Tokens = ov.Summary.InputTokens + ov.Summary.OutputTokens + ov.Summary.CacheReadTokens + ov.Summary.CacheWriteTokens
	if ov.Summary.Requests > 0 {
		ov.Summary.SuccessRate = float64(ov.Summary.Requests-ov.Summary.Errors) / float64(ov.Summary.Requests)
	}
	rows, err := s.db.Query(
		`SELECT strftime('`+gran+`', created_at, 'unixepoch'), count(*), count(*) FILTER (WHERE status >= 400),
		        coalesce(sum(input_tokens),0)+coalesce(sum(output_tokens),0)+coalesce(sum(cache_read_tokens),0)+coalesce(sum(cache_write_tokens),0),
		        coalesce(sum(cost),0)
		 FROM usage_logs WHERE created_at > ? GROUP BY 1 ORDER BY 1`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p UsageOverviewPoint
		if err := rows.Scan(&p.Date, &p.Requests, &p.Errors, &p.Tokens, &p.Cost); err != nil {
			return nil, err
		}
		ov.Series = append(ov.Series, p)
	}
	dist := func(col string) ([]UsageDistribution, error) {
		rows, err := s.db.Query(
			`SELECT coalesce(`+col+`,'-'), count(*),
			        coalesce(sum(input_tokens),0)+coalesce(sum(output_tokens),0)+coalesce(sum(cache_read_tokens),0)+coalesce(sum(cache_write_tokens),0),
			        coalesce(sum(cost),0)
			 FROM usage_logs WHERE created_at > ? GROUP BY 1 ORDER BY 2 DESC LIMIT 6`, cutoff)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []UsageDistribution
		for rows.Next() {
			var d UsageDistribution
			if err := rows.Scan(&d.Group, &d.Requests, &d.Tokens, &d.Cost); err != nil {
				return nil, err
			}
			out = append(out, d)
		}
		return out, rows.Err()
	}
	if ov.ByModel, err = dist("model"); err != nil {
		return nil, err
	}
	if ov.ByKey, err = dist("(SELECT coalesce(name,'-') FROM keys WHERE keys.id = usage_logs.key_id)"); err != nil {
		return nil, err
	}
	return ov, nil
}

func (s *SqliteStore) QueryLogs(filter LogFilter) ([]UsageLog, error) {
	q, args := `SELECT id, coalesce(key_id,0), coalesce(model,''), coalesce(upstream_model,''), coalesce(protocol,''),
	          coalesce(input_tokens,0), coalesce(output_tokens,0), coalesce(cache_read_tokens,0), coalesce(cache_write_tokens,0),
	          coalesce(cost,0), coalesce(latency_ms,0), coalesce(status,0), coalesce(error,''), coalesce(request_id,''), coalesce(unmetered,0), coalesce(channel_id,0), coalesce(account_id,0), attempts, coalesce(created_at,0)
	          FROM usage_logs WHERE 1=1`, []any{}
	if filter.KeyID > 0 {
		args = append(args, filter.KeyID)
		q += " AND key_id = ?"
	}
	if filter.Model != "" {
		args = append(args, filter.Model)
		q += " AND model = ?"
	}
	if filter.Status > 0 {
		args = append(args, filter.Status)
		q += " AND status = ?"
	}
	if filter.RequestID != "" {
		args = append(args, filter.RequestID)
		q += " AND request_id = ?"
	}
	if filter.FailedOnly {
		q += " AND status >= 400"
	}
	if filter.Days > 0 {
		args = append(args, time.Now().AddDate(0, 0, -filter.Days).Unix())
		q += " AND created_at > ?"
	}
	q += " ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?"
	args = append(args, filter.Limit, filter.Offset)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UsageLog{}
	for rows.Next() {
		var l UsageLog
		var created sql.NullInt64
		if err := rows.Scan(&l.ID, &l.KeyID, &l.Model, &l.UpstreamModel, &l.Protocol, &l.InputTokens, &l.OutputTokens,
			&l.CacheReadTokens, &l.CacheWriteTokens, &l.Cost, &l.LatencyMs, &l.Status, &l.Error, &l.RequestID, &l.Unmetered,
			&l.ChannelID, &l.AccountID, &l.Attempts, &created); err != nil {
			return nil, err
		}
		l.CreatedAt = fromUnix(created)
		out = append(out, l)
	}
	return out, rows.Err()
}

// --- ChannelStore ---

func (s *SqliteStore) LoadChannels() ([]config.Channel, error) {
	rows, err := s.db.Query(
		`SELECT id, provider, name, balance_type, balance_url, models_url, enabled, preset, auth_mode FROM channels ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var channels []config.Channel
	for rows.Next() {
		var ch config.Channel
		var presetJSON []byte
		if err := rows.Scan(&ch.ID, &ch.Provider, &ch.Name, &ch.BalanceType, &ch.BalanceURL, &ch.ModelsURL, &ch.Enabled, &presetJSON, &ch.AuthMode); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(presetJSON, &ch.Preset)
		models, err := s.loadModels(ch.ID, ch.Provider)
		if err != nil {
			return nil, err
		}
		ch.Models = models
		channels = append(channels, ch)
	}
	return channels, rows.Err()
}

func (s *SqliteStore) loadModels(channelID int64, provider string) ([]config.Model, error) {
	rows, err := s.db.Query(
		`SELECT name, routes, input_per_m, output_per_m, cache_read_per_m, cache_write_per_m, enabled
		 FROM models WHERE channel_id = ? ORDER BY id`, channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var models []config.Model
	for rows.Next() {
		var m config.Model
		var routesJSON []byte
		if err := rows.Scan(&m.Name, &routesJSON, &m.Pricing.InputPerM, &m.Pricing.OutputPerM, &m.Pricing.CacheReadPerM, &m.Pricing.CacheWritePerM, &m.Enabled); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(routesJSON, &m.Routes)
		m.Provider = provider
		models = append(models, m)
	}
	return models, rows.Err()
}

func (s *SqliteStore) CreateChannel(ch config.Channel) error {
	presetJSON, _ := json.Marshal(ch.Preset)
	res, err := s.db.Exec(
		`INSERT INTO channels (provider, name, balance_type, balance_url, models_url, enabled, preset, auth_mode)
		 VALUES (?,?,?,?,?,?,?,?)`,
		ch.Provider, ch.Name, ch.BalanceType, ch.BalanceURL, ch.ModelsURL, ch.Enabled, presetJSON, ch.AuthMode)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	return s.insertModels(id, ch.Models)
}

func (s *SqliteStore) insertModels(channelID int64, models []config.Model) error {
	for _, m := range models {
		routesJSON, _ := json.Marshal(m.Routes)
		if _, err := s.db.Exec(
			`INSERT INTO models (channel_id, name, routes, input_per_m, output_per_m, cache_read_per_m, cache_write_per_m, enabled)
			 VALUES (?,?,?,?,?,?,?,?)`,
			channelID, m.Name, routesJSON, m.Pricing.InputPerM, m.Pricing.OutputPerM, m.Pricing.CacheReadPerM, m.Pricing.CacheWritePerM, m.Enabled); err != nil {
			return err
		}
	}
	return nil
}

func (s *SqliteStore) UpdateChannel(ch config.Channel) error {
	presetJSON, _ := json.Marshal(ch.Preset)
	if _, err := s.db.Exec(
		`UPDATE channels SET name=?, balance_type=?, balance_url=?, models_url=?, enabled=?, preset=?, auth_mode=? WHERE provider=?`,
		ch.Name, ch.BalanceType, ch.BalanceURL, ch.ModelsURL, ch.Enabled, presetJSON, ch.AuthMode, ch.Provider); err != nil {
		return err
	}
	var id int64
	if err := s.db.QueryRow(`SELECT id FROM channels WHERE provider=?`, ch.Provider).Scan(&id); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM models WHERE channel_id=?`, id); err != nil {
		return err
	}
	return s.insertModels(id, ch.Models)
}

func (s *SqliteStore) DeleteChannel(provider string) error {
	_, err := s.db.Exec(`DELETE FROM channels WHERE provider=?`, provider)
	return err
}

func (s *SqliteStore) CreateModel(provider string, m config.Model) error {
	var channelID int64
	if err := s.db.QueryRow(`SELECT id FROM channels WHERE provider=?`, provider).Scan(&channelID); err != nil {
		return err
	}
	routesJSON, _ := json.Marshal(m.Routes)
	_, err := s.db.Exec(
		`INSERT INTO models (channel_id, name, routes, input_per_m, output_per_m, cache_read_per_m, cache_write_per_m, enabled)
		 VALUES (?,?,?,?,?,?,?,?)`,
		channelID, m.Name, routesJSON, m.Pricing.InputPerM, m.Pricing.OutputPerM, m.Pricing.CacheReadPerM, m.Pricing.CacheWritePerM, m.Enabled)
	return err
}

func (s *SqliteStore) UpdateModel(provider string, m config.Model) error {
	routesJSON, _ := json.Marshal(m.Routes)
	_, err := s.db.Exec(
		`UPDATE models SET routes=?, input_per_m=?, output_per_m=?, cache_read_per_m=?, cache_write_per_m=?, enabled=?
		 WHERE channel_id=(SELECT id FROM channels WHERE provider=?) AND name=?`,
		routesJSON, m.Pricing.InputPerM, m.Pricing.OutputPerM, m.Pricing.CacheReadPerM, m.Pricing.CacheWritePerM, m.Enabled, provider, m.Name)
	return err
}

func (s *SqliteStore) DeleteModel(provider, modelName string) error {
	_, err := s.db.Exec(
		`DELETE FROM models WHERE channel_id=(SELECT id FROM channels WHERE provider=?) AND name=?`, provider, modelName)
	return err
}

// --- 设置与全文日志 ---

func (s *SqliteStore) GetSetting(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", err
	}
	return v, nil
}

func (s *SqliteStore) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings (key, value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func (s *SqliteStore) InsertLogBody(b *LogBody) error {
	res, err := s.db.Exec(
		`INSERT INTO request_bodies (usage_request_id, model, request_body, response_body, truncated, status)
		 VALUES (?,?,?,?,?,?)`,
		b.UsageRequestID, b.Model, b.RequestBody, b.ResponseBody, b.Truncated, b.Status)
	if err != nil {
		return err
	}
	b.ID, _ = res.LastInsertId()
	return nil
}

func (s *SqliteStore) GetLogBodyByRequestID(usageRequestID string) (*LogBody, error) {
	var b LogBody
	var req, resp []byte
	var truncated any
	var created sql.NullInt64
	err := s.db.QueryRow(
		`SELECT id, coalesce(usage_request_id,''), coalesce(model,''), request_body, response_body, truncated, coalesce(status,0), created_at
		 FROM request_bodies WHERE usage_request_id = ? ORDER BY id DESC LIMIT 1`, usageRequestID).
		Scan(&b.ID, &b.UsageRequestID, &b.Model, &req, &resp, &truncated, &b.Status, &created)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	b.RequestBody = req
	b.ResponseBody = resp
	b.Truncated = truncated == int64(1) || truncated == true
	b.CreatedAt = fromUnix(created)
	return &b, nil
}

func (s *SqliteStore) CleanupLogBodies(olderThan time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM request_bodies WHERE created_at < ?`, olderThan.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
