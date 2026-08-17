package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gateway/internal/config"
	"gateway/internal/keys"
)

// PgStore pgx 实现（部署用，连 A 机 PG）。
type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(ctx context.Context, connString string) (*PgStore, error) {
	pool, err := pgxpool.New(ctx, connString)
	if err != nil {
		return nil, fmt.Errorf("connect pg: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("ping pg: %w", err)
	}
	return &PgStore{pool: pool}, nil
}

// --- keys.Store 实现 ---

func (s *PgStore) CreateKey(k keys.Key) error {
	_, err := s.pool.Exec(context.Background(),
		`INSERT INTO keys (key_hash, name, owner, agent_type, quota_limit, quota_used, enabled, allowed_models)
		 VALUES ($1, $2, $3, $4, $5, 0, $6, $7)`,
		k.KeyHash, k.Name, k.Owner, k.AgentType, nullFloat(k.QuotaLimit), k.Enabled, k.AllowedModels)
	return err
}

func (s *PgStore) GetKeyByHash(hash string) (*keys.Key, error) {
	var k keys.Key
	var quotaLimit *float64
	err := s.pool.QueryRow(context.Background(),
		`SELECT id, key_hash, name, coalesce(owner,''), agent_type, quota_limit, quota_used, enabled, coalesce(allowed_models,'')
		 FROM keys WHERE key_hash = $1`, hash).
		Scan(&k.ID, &k.KeyHash, &k.Name, &k.Owner, &k.AgentType, &quotaLimit, &k.QuotaUsed, &k.Enabled, &k.AllowedModels)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if quotaLimit != nil {
		k.QuotaLimit = *quotaLimit
	}
	return &k, nil
}

func (s *PgStore) ListKeys() ([]keys.Key, error) {
	rows, err := s.pool.Query(context.Background(),
		`SELECT id, key_hash, name, coalesce(owner,''), agent_type, quota_limit, quota_used, enabled, coalesce(allowed_models,'') FROM keys ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []keys.Key
	for rows.Next() {
		var k keys.Key
		var quotaLimit *float64
		if err := rows.Scan(&k.ID, &k.KeyHash, &k.Name, &k.Owner, &k.AgentType, &quotaLimit, &k.QuotaUsed, &k.Enabled, &k.AllowedModels); err != nil {
			return nil, err
		}
		if quotaLimit != nil {
			k.QuotaLimit = *quotaLimit
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *PgStore) RevokeKey(hash string) error {
	_, err := s.pool.Exec(context.Background(),
		`UPDATE keys SET enabled = false WHERE key_hash = $1`, hash)
	return err
}

func (s *PgStore) AddQuotaUsed(hash string, amount float64) error {
	_, err := s.pool.Exec(context.Background(),
		`UPDATE keys SET quota_used = quota_used + $1 WHERE key_hash = $2`, amount, hash)
	return err
}

func (s *PgStore) UpdateQuota(hash string, limit float64) error {
	_, err := s.pool.Exec(context.Background(),
		`UPDATE keys SET quota_limit = $1 WHERE key_hash = $2`, nullFloat(limit), hash)
	return err
}

func (s *PgStore) UpdateKey(k keys.Key) error {
	_, err := s.pool.Exec(context.Background(),
		`UPDATE keys SET name=$1, owner=$2, agent_type=$3, quota_limit=$4, allowed_models=$5 WHERE key_hash=$6`,
		k.Name, k.Owner, k.AgentType, nullFloat(k.QuotaLimit), k.AllowedModels, k.KeyHash)
	return err
}

// --- UpstreamStore 实现 ---

func (s *PgStore) SetUpstreamKey(provider string, encrypted []byte) error {
	_, err := s.pool.Exec(context.Background(),
		`INSERT INTO upstream_keys (provider, encrypted_key) VALUES ($1, $2)
		 ON CONFLICT (provider) DO UPDATE SET encrypted_key = EXCLUDED.encrypted_key`,
		provider, encrypted)
	return err
}

func (s *PgStore) GetUpstreamKey(provider string) ([]byte, error) {
	var encrypted []byte
	err := s.pool.QueryRow(context.Background(),
		`SELECT encrypted_key FROM upstream_keys WHERE provider = $1`, provider).
		Scan(&encrypted)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return encrypted, nil
}

// --- UsageStore 实现 ---

func (s *PgStore) InsertUsageLog(log UsageLog) error {
	_, err := s.pool.Exec(context.Background(),
		`INSERT INTO usage_logs (key_id, model, upstream_model, protocol, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost, latency_ms, status, error, request_id, unmetered)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		nullInt64(log.KeyID), log.Model, log.UpstreamModel, log.Protocol, log.InputTokens, log.OutputTokens,
		log.CacheReadTokens, log.CacheWriteTokens, log.Cost, log.LatencyMs, log.Status, log.Error, log.RequestID, log.Unmetered)
	return err
}

// nullInt64 0 值存 NULL（如认证失败的 key_id=0）。
func nullInt64(v int64) any {
	if v <= 0 {
		return nil
	}
	return v
}

// GetUsageStats 用量聚合（汇总 + 按模型 + 按 key）。
func (s *PgStore) GetUsageStats() (UsageStats, error) {
	ctx := context.Background()
	var st UsageStats

	// 汇总
	err := s.pool.QueryRow(ctx,
		`SELECT coalesce(sum(cost),0)::float8,
		        coalesce(sum(input_tokens),0)+coalesce(sum(output_tokens),0)+coalesce(sum(cache_read_tokens),0)+coalesce(sum(cache_write_tokens),0),
		        count(*)
		 FROM usage_logs`).Scan(&st.TotalCost, &st.TotalTokens, &st.TotalRequests)
	if err != nil {
		return st, err
	}

	// 按模型
	rows, err := s.pool.Query(ctx,
		`SELECT model, coalesce(sum(cost),0)::float8, coalesce(sum(input_tokens),0), coalesce(sum(output_tokens),0),
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
	if err := rows.Err(); err != nil {
		return st, err
	}

	// 按 key
	rows2, err := s.pool.Query(ctx,
		`SELECT u.key_id, coalesce(k.name,''), coalesce(k.owner,''), coalesce(sum(u.cost),0)::float8,
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
	if err := rows2.Err(); err != nil {
		return st, err
	}

	return st, nil
}

// GetDashboard 概览聚合（今日汇总 + 最近请求）。
func (s *PgStore) GetDashboard() (DashboardStats, error) {
	ctx := context.Background()
	var st DashboardStats
	err := s.pool.QueryRow(ctx,
		`SELECT coalesce(sum(cost),0)::float8,
		        coalesce(sum(input_tokens),0)+coalesce(sum(output_tokens),0)+coalesce(sum(cache_read_tokens),0)+coalesce(sum(cache_write_tokens),0),
		        count(*),
		        coalesce(avg(CASE WHEN status < 400 THEN 1.0 ELSE 0.0 END),0),
		        coalesce(avg(latency_ms),0)::float8,
		        coalesce(sum(cache_read_tokens),0),
		        CASE WHEN coalesce(sum(input_tokens),0)+coalesce(sum(cache_read_tokens),0) > 0
		             THEN coalesce(sum(cache_read_tokens),0)::float8 / (coalesce(sum(input_tokens),0)+coalesce(sum(cache_read_tokens),0))
		             ELSE 0 END
		 FROM usage_logs WHERE created_at >= current_date`).
		Scan(&st.TodayCost, &st.TodayTokens, &st.TodayRequests, &st.SuccessRate, &st.AvgLatencyMs, &st.CacheReadTokens, &st.CacheHitRate)
	if err != nil {
		return st, err
	}
	st.RecentLogs, err = s.QueryLogs(LogFilter{Limit: 5})
	return st, err
}

// timeFilter 返回时间过滤的 WHERE 片段与参数（col 需含表别名前缀，如 "u.created_at"）。
func timeFilter(col string, r TimeRange) (string, []any) {
	if r.From != "" && r.To != "" {
		return fmt.Sprintf(" AND %s >= $1::date AND %s < ($2::date + 1)", col, col), []any{r.From, r.To}
	}
	if r.Days > 0 {
		return fmt.Sprintf(" AND %s >= current_date - ($1::int - 1)", col), []any{r.Days}
	}
	return "", nil
}

// GetTimeseries 按天聚合（趋势图）。
func (s *PgStore) GetTimeseries(r TimeRange) ([]TimeseriesPoint, error) {
	ctx := context.Background()
	filter, fargs := timeFilter("created_at", r)
	rows, err := s.pool.Query(ctx,
		`SELECT to_char(date_trunc('day', created_at), 'MM-DD'),
		        coalesce(sum(cost),0)::float8,
		        coalesce(sum(input_tokens),0)+coalesce(sum(output_tokens),0)+coalesce(sum(cache_read_tokens),0)+coalesce(sum(cache_write_tokens),0),
		        count(*)
		 FROM usage_logs
		 WHERE 1=1`+filter+`
		 GROUP BY date_trunc('day', created_at)
		 ORDER BY date_trunc('day', created_at)`, fargs...)
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

// GetGrouped 按维度聚合（model/key/owner/protocol）。
func (s *PgStore) GetGrouped(by string, r TimeRange) ([]GroupedUsage, error) {
	ctx := context.Background()
	var sql string
	var filter string
	var fargs []any
	switch by {
	case "protocol":
		sql = `SELECT coalesce(protocol,''), coalesce(sum(cost),0)::float8, coalesce(sum(input_tokens),0), coalesce(sum(output_tokens),0),
		        coalesce(sum(input_tokens),0)+coalesce(sum(output_tokens),0)+coalesce(sum(cache_read_tokens),0)+coalesce(sum(cache_write_tokens),0), count(*),
		        coalesce(avg(CASE WHEN status < 400 THEN 1.0 ELSE 0.0 END),0),
		        coalesce(sum(cache_read_tokens),0), coalesce(avg(latency_ms),0)::float8
		 FROM usage_logs WHERE protocol <> ''`
		filter, fargs = timeFilter("created_at", r)
		sql += filter + ` GROUP BY protocol ORDER BY sum(cost) DESC`
	case "key":
		sql = `SELECT coalesce(k.name,'(未知)'), coalesce(sum(u.cost),0)::float8, coalesce(sum(u.input_tokens),0), coalesce(sum(u.output_tokens),0),
		        coalesce(sum(u.input_tokens),0)+coalesce(sum(u.output_tokens),0)+coalesce(sum(u.cache_read_tokens),0)+coalesce(sum(u.cache_write_tokens),0), count(*),
		        coalesce(avg(CASE WHEN u.status < 400 THEN 1.0 ELSE 0.0 END),0),
		        coalesce(sum(u.cache_read_tokens),0), coalesce(avg(u.latency_ms),0)::float8
		 FROM usage_logs u LEFT JOIN keys k ON u.key_id = k.id
		 WHERE u.key_id IS NOT NULL`
		filter, fargs = timeFilter("u.created_at", r)
		sql += filter + ` GROUP BY k.name ORDER BY sum(u.cost) DESC`
	case "owner":
		sql = `SELECT coalesce(k.owner,'(未归属)'), coalesce(sum(u.cost),0)::float8, coalesce(sum(u.input_tokens),0), coalesce(sum(u.output_tokens),0),
		        coalesce(sum(u.input_tokens),0)+coalesce(sum(u.output_tokens),0)+coalesce(sum(u.cache_read_tokens),0)+coalesce(sum(u.cache_write_tokens),0), count(*),
		        coalesce(avg(CASE WHEN u.status < 400 THEN 1.0 ELSE 0.0 END),0),
		        coalesce(sum(u.cache_read_tokens),0), coalesce(avg(u.latency_ms),0)::float8
		 FROM usage_logs u LEFT JOIN keys k ON u.key_id = k.id
		 WHERE u.key_id IS NOT NULL`
		filter, fargs = timeFilter("u.created_at", r)
		sql += filter + ` GROUP BY k.owner ORDER BY sum(u.cost) DESC`
	default: // model
		sql = `SELECT coalesce(model,''), coalesce(sum(cost),0)::float8, coalesce(sum(input_tokens),0), coalesce(sum(output_tokens),0),
		        coalesce(sum(input_tokens),0)+coalesce(sum(output_tokens),0)+coalesce(sum(cache_read_tokens),0)+coalesce(sum(cache_write_tokens),0), count(*),
		        coalesce(avg(CASE WHEN status < 400 THEN 1.0 ELSE 0.0 END),0),
		        coalesce(sum(cache_read_tokens),0), coalesce(avg(latency_ms),0)::float8
		 FROM usage_logs WHERE model <> ''`
		filter, fargs = timeFilter("created_at", r)
		sql += filter + ` GROUP BY model ORDER BY sum(cost) DESC`
	}
	rows, err := s.pool.Query(ctx, sql, fargs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GroupedUsage{}
	for rows.Next() {
		var g GroupedUsage
		if err := rows.Scan(&g.Group, &g.Cost, &g.InputTokens, &g.OutputTokens, &g.Tokens, &g.Requests, &g.SuccessRate, &g.CacheReadTokens, &g.AvgLatencyMs); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// QueryLogs 请求日志查询（筛选 + 分页）。
func (s *PgStore) QueryLogs(filter LogFilter) ([]UsageLog, error) {
	ctx := context.Background()
	query := `SELECT id, coalesce(key_id,0), coalesce(model,''), coalesce(upstream_model,''), coalesce(protocol,''),
	          coalesce(input_tokens,0), coalesce(output_tokens,0), coalesce(cache_read_tokens,0), coalesce(cache_write_tokens,0),
	          coalesce(cost,0)::float8, coalesce(latency_ms,0), coalesce(status,0), coalesce(error,''), coalesce(request_id,''), coalesce(unmetered,false), created_at
	          FROM usage_logs WHERE 1=1`
	args := []any{}
	if filter.KeyID > 0 {
		args = append(args, filter.KeyID)
		query += fmt.Sprintf(" AND key_id = $%d", len(args))
	}
	if filter.Model != "" {
		args = append(args, filter.Model)
		query += fmt.Sprintf(" AND model = $%d", len(args))
	}
	if filter.Status > 0 {
		args = append(args, filter.Status)
		query += fmt.Sprintf(" AND status = $%d", len(args))
	}
	query += " ORDER BY created_at DESC, id DESC"
	if filter.Limit > 0 {
		args = append(args, filter.Limit)
		query += fmt.Sprintf(" LIMIT $%d", len(args))
	}
	if filter.Offset > 0 {
		args = append(args, filter.Offset)
		query += fmt.Sprintf(" OFFSET $%d", len(args))
	}

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageLog
	for rows.Next() {
		var l UsageLog
		if err := rows.Scan(&l.ID, &l.KeyID, &l.Model, &l.UpstreamModel, &l.Protocol,
			&l.InputTokens, &l.OutputTokens, &l.CacheReadTokens, &l.CacheWriteTokens,
			&l.Cost, &l.LatencyMs, &l.Status, &l.Error, &l.RequestID, &l.Unmetered, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// --- ChannelStore 实现 ---

func (s *PgStore) LoadChannels() ([]config.Channel, error) {
	ctx := context.Background()
	rows, err := s.pool.Query(ctx, `SELECT id, provider, name, balance_type, balance_url, models_url, enabled, preset, auth_mode FROM channels ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var channels []config.Channel
	for rows.Next() {
		var ch config.Channel
		var id int64
		var presetJSON []byte
		if err := rows.Scan(&id, &ch.Provider, &ch.Name, &ch.BalanceType, &ch.BalanceURL, &ch.ModelsURL, &ch.Enabled, &presetJSON, &ch.AuthMode); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(presetJSON, &ch.Preset)
		models, err := s.loadModels(ctx, id, ch.Provider)
		if err != nil {
			return nil, err
		}
		ch.Models = models
		channels = append(channels, ch)
	}
	return channels, rows.Err()
}

func (s *PgStore) loadModels(ctx context.Context, channelID int64, provider string) ([]config.Model, error) {
	rows, err := s.pool.Query(ctx, `SELECT name, routes, input_per_m, output_per_m, cache_read_per_m, cache_write_per_m, enabled FROM models WHERE channel_id = $1 ORDER BY id`, channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var models []config.Model
	for rows.Next() {
		var m config.Model
		var routesJSON []byte
		var in, out, cr, cw float64
		if err := rows.Scan(&m.Name, &routesJSON, &in, &out, &cr, &cw, &m.Enabled); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(routesJSON, &m.Routes)
		m.Provider = provider
		m.Pricing = config.Pricing{InputPerM: in, OutputPerM: out, CacheReadPerM: cr, CacheWritePerM: cw}
		models = append(models, m)
	}
	return models, rows.Err()
}

func (s *PgStore) CreateChannel(ch config.Channel) error {
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	presetJSON, _ := json.Marshal(ch.Preset)
	var id int64
	if err := tx.QueryRow(ctx, `INSERT INTO channels (provider, name, balance_type, balance_url, models_url, enabled, preset, auth_mode) VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`,
		ch.Provider, ch.Name, ch.BalanceType, ch.BalanceURL, ch.ModelsURL, ch.Enabled, presetJSON, ch.AuthMode).Scan(&id); err != nil {
		return err
	}
	for _, m := range ch.Models {
		routesJSON, _ := json.Marshal(m.Routes)
		if _, err := tx.Exec(ctx, `INSERT INTO models (channel_id, name, routes, input_per_m, output_per_m, cache_read_per_m, cache_write_per_m, enabled) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			id, m.Name, routesJSON, m.Pricing.InputPerM, m.Pricing.OutputPerM, m.Pricing.CacheReadPerM, m.Pricing.CacheWritePerM, m.Enabled); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *PgStore) UpdateChannel(ch config.Channel) error {
	presetJSON, _ := json.Marshal(ch.Preset)
	_, err := s.pool.Exec(context.Background(), `UPDATE channels SET name=$1, balance_type=$2, balance_url=$3, models_url=$4, enabled=$5, preset=$6, auth_mode=$7 WHERE provider=$8`,
		ch.Name, ch.BalanceType, ch.BalanceURL, ch.ModelsURL, ch.Enabled, presetJSON, ch.AuthMode, ch.Provider)
	return err
}

func (s *PgStore) DeleteChannel(provider string) error {
	_, err := s.pool.Exec(context.Background(), `DELETE FROM channels WHERE provider=$1`, provider)
	return err
}

func (s *PgStore) CreateModel(provider string, m config.Model) error {
	ctx := context.Background()
	var channelID int64
	if err := s.pool.QueryRow(ctx, `SELECT id FROM channels WHERE provider=$1`, provider).Scan(&channelID); err != nil {
		return err
	}
	routesJSON, _ := json.Marshal(m.Routes)
	_, err := s.pool.Exec(ctx, `INSERT INTO models (channel_id, name, routes, input_per_m, output_per_m, cache_read_per_m, cache_write_per_m, enabled) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		channelID, m.Name, routesJSON, m.Pricing.InputPerM, m.Pricing.OutputPerM, m.Pricing.CacheReadPerM, m.Pricing.CacheWritePerM, m.Enabled)
	return err
}

func (s *PgStore) UpdateModel(provider string, m config.Model) error {
	routesJSON, _ := json.Marshal(m.Routes)
	_, err := s.pool.Exec(context.Background(), `UPDATE models SET routes=$1, input_per_m=$2, output_per_m=$3, cache_read_per_m=$4, cache_write_per_m=$5, enabled=$6 WHERE channel_id=(SELECT id FROM channels WHERE provider=$7) AND name=$8`,
		routesJSON, m.Pricing.InputPerM, m.Pricing.OutputPerM, m.Pricing.CacheReadPerM, m.Pricing.CacheWritePerM, m.Enabled, provider, m.Name)
	return err
}

func (s *PgStore) DeleteModel(provider, modelName string) error {
	_, err := s.pool.Exec(context.Background(), `DELETE FROM models WHERE channel_id=(SELECT id FROM channels WHERE provider=$1) AND name=$2`, provider, modelName)
	return err
}

// nullFloat 额度 0 表示不限，存 NULL。
func nullFloat(f float64) any {
	if f <= 0 {
		return nil
	}
	return f
}
