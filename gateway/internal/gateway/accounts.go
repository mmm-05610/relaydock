package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"gateway/internal/config"
	"gateway/internal/keys"
	"gateway/internal/pool"
	"gateway/internal/store"
)

// 账号管理 API（docs/design-upstream-account-pool.md §11）。
// 响应只含账号 ID、名称、启用状态、并发限制、key 配置状态与运行时健康摘要，
// 不返回明文 key、密文或可识别的长 key 前缀。

// accountsView 列表响应：池级汇总（健康桶计数）+ 按可操作性排序的账号条目。
// 排序：冷却中 > 禁用 > 健康（需要关注的在前，gpt-load 同款严重度序）。
type accountsView struct {
	Summary accountsSummary        `json:"summary"`
	Items   []accountItemWithStats `json:"items"`
}

type accountsSummary struct {
	Total    int64 `json:"total"`
	Healthy  int64 `json:"healthy"`
	Cooling  int64 `json:"cooling"`
	Disabled int64 `json:"disabled"`
}

type accountItemWithStats struct {
	ID             int64               `json:"id"`
	Name           string              `json:"name"`
	Enabled        bool                `json:"enabled"`
	MaxConcurrency int64               `json:"max_concurrency"`
	KeyConfigured  bool                `json:"key_configured"`
	Inflight       int64               `json:"inflight"`
	State          string              `json:"state"` // healthy | cooling | disabled
	CoolingUntil   any                 `json:"cooling_until"`
	CooldownCause  string              `json:"cooldown_cause"`
	SuccessCount   int64               `json:"success_count"`
	FailureCount   int64               `json:"failure_count"`
	ConsecutiveFai int64               `json:"consecutive_failures"`
	LastStatusCode int                 `json:"last_status_code"`
	LastError      string              `json:"last_error"`
	LastUsedAt     any                 `json:"last_used_at"`
	Usage          *store.AccountUsage `json:"usage"` // 最近 7 天（PG 模式），内存模式 nil
}

// handleListAccounts GET /api/channels/{provider}/accounts[?stats=1]
func (g *Gateway) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	provider := r.PathValue("provider")
	snap := g.Snapshots.Load()
	ch := snap.FindChannel(provider)
	if ch == nil {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}

	// 7 天用量聚合（PG 模式）
	var usageByAccount map[int64]*store.AccountUsage
	if r.URL.Query().Get("stats") == "1" && g.DB != nil {
		usageByAccount = map[int64]*store.AccountUsage{}
		if rows, err := g.DB.AccountUsageStats(7); err == nil {
			for i := range rows {
				usageByAccount[rows[i].AccountID] = &rows[i]
			}
		} else {
			// 统计查询失败不阻塞列表（旁路）
		}
	}

	now := time.Now()
	view := accountsView{}
	for _, ref := range snap.AccountsFor(ch) {
		st := ref.Status()
		state := "healthy"
		switch {
		case !ref.Spec.Enabled:
			state = "disabled"
		case now.Before(st.CoolingUntil):
			state = "cooling"
		}
		switch state {
		case "healthy":
			view.Summary.Healthy++
		case "cooling":
			view.Summary.Cooling++
		case "disabled":
			view.Summary.Disabled++
		}
		view.Summary.Total++

		item := accountItemWithStats{
			ID:             ref.Spec.ID,
			Name:           ref.Spec.Name,
			Enabled:        ref.Spec.Enabled,
			MaxConcurrency: ref.Spec.MaxConcurrency,
			KeyConfigured:  ref.Spec.Credential != "",
			Inflight:       st.Inflight,
			State:          state,
			CoolingUntil:   nullableTime(st.CoolingUntil),
			CooldownCause:  st.CooldownCause,
			SuccessCount:   st.SuccessCount,
			FailureCount:   st.FailureCount,
			ConsecutiveFai: st.ConsecutiveFailures,
			LastStatusCode: st.LastStatusCode,
			LastError:      st.LastError,
			LastUsedAt:     nullableTime(st.LastUsedAt),
			Usage:          usageByAccount[ref.Spec.ID],
		}
		view.Items = append(view.Items, item)
	}
	sort.SliceStable(view.Items, func(i, j int) bool {
		return accountSeverity(view.Items[i].State) < accountSeverity(view.Items[j].State)
	})
	writeJSON(w, view)
}

func accountSeverity(state string) int {
	switch state {
	case "cooling":
		return 0
	case "disabled":
		return 1
	default:
		return 2
	}
}

// handleCreateAccount POST /api/channels/{provider}/accounts
// body: {name, key, max_concurrency}
func (g *Gateway) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	provider := r.PathValue("provider")
	snap := g.Snapshots.Load()
	ch := snap.FindChannel(provider)
	if ch == nil {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}
	var req struct {
		Name           string `json:"name"`
		Key            string `json:"key"`
		MaxConcurrency int64  `json:"max_concurrency"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.Key == "" || req.MaxConcurrency < 0 {
		http.Error(w, "name/key required, max_concurrency >= 0", http.StatusBadRequest)
		return
	}
	a, err := g.buildAccountRow(ch.ID, req.Name, req.Key, req.MaxConcurrency)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := g.DB.CreateUpstreamAccount(a); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	g.reloadChannels()
	writeJSON(w, map[string]any{"id": a.ID, "name": a.Name})
}

// handleImportAccounts POST /api/channels/{provider}/accounts/import
// body: {items: [{name?, key, max_concurrency?}]} —— 凭据指纹去重，重复的跳过并计数。
func (g *Gateway) handleImportAccounts(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	provider := r.PathValue("provider")
	snap := g.Snapshots.Load()
	ch := snap.FindChannel(provider)
	if ch == nil {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}
	var req struct {
		Items []struct {
			Name           string `json:"name"`
			Key            string `json:"key"`
			MaxConcurrency int64  `json:"max_concurrency"`
		} `json:"items"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if len(req.Items) == 0 {
		http.Error(w, "empty items", http.StatusBadRequest)
		return
	}
	if len(req.Items) > 100 {
		http.Error(w, "too many items (max 100 per batch)", http.StatusBadRequest)
		return
	}

	existing, err := g.DB.ListUpstreamAccounts()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	seen := map[string]bool{} // channel 内指纹去重
	for _, a := range existing {
		if a.ChannelID == ch.ID {
			seen[a.KeyFingerprint] = true
		}
	}

	masterKey, err := keys.MasterKeyFromHex(g.masterKeyHex)
	if err != nil {
		http.Error(w, "master key 未配置", http.StatusInternalServerError)
		return
	}
	added, duplicated := 0, 0
	seq := len(existing) + 1
	for _, item := range req.Items {
		if item.Key == "" {
			continue
		}
		fp := keys.Fingerprint(item.Key, masterKey)
		if seen[fp] {
			duplicated++
			continue
		}
		name := item.Name
		if name == "" {
			name = fmt.Sprintf("account-%d", seq)
		}
		seq++
		enc, err := keys.Encrypt([]byte(item.Key), masterKey)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		a := &store.UpstreamAccount{
			ChannelID:      ch.ID,
			Name:           name,
			EncryptedKey:   enc,
			KeyFingerprint: fp,
			MaxConcurrency: item.MaxConcurrency,
			Enabled:        true,
		}
		if err := g.DB.CreateUpstreamAccount(a); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		seen[fp] = true
		added++
	}
	g.reloadChannels()
	writeJSON(w, map[string]any{"added": added, "duplicated": duplicated})
}

// handleBatchAccounts POST /api/channels/{provider}/accounts/batch
// body: {action: enable|disable|delete|recover, ids: [..]}
func (g *Gateway) handleBatchAccounts(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	provider := r.PathValue("provider")
	snap := g.Snapshots.Load()
	ch := snap.FindChannel(provider)
	if ch == nil {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}
	var req struct {
		Action string  `json:"action"`
		IDs    []int64 `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if len(req.IDs) == 0 {
		http.Error(w, "ids required", http.StatusBadRequest)
		return
	}
	ids := map[int64]bool{}
	for _, id := range req.IDs {
		ids[id] = true
	}

	affected := []int64{}
	switch req.Action {
	case "enable", "disable":
		accounts, err := g.DB.ListUpstreamAccounts()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, a := range accounts {
			if a.ChannelID != ch.ID || !ids[a.ID] {
				continue
			}
			a.Enabled = req.Action == "enable"
			if err := g.DB.UpdateUpstreamAccount(a); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			affected = append(affected, a.ID)
		}
		g.reloadChannels()
	case "delete":
		for _, id := range req.IDs {
			if err := g.DB.DeleteUpstreamAccount(id); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			affected = append(affected, id)
		}
		g.reloadChannels()
	case "recover":
		now := time.Now()
		for _, ref := range snap.AccountsFor(ch) {
			if !ids[ref.Spec.ID] {
				continue
			}
			// 仅对冷却中的账号合法（gpt-load 同款状态校验），运行态在内存
			if ref.Spec.Enabled && ref.IsCooling(now) {
				ref.State.Recover()
				affected = append(affected, ref.Spec.ID)
			}
		}
	default:
		http.Error(w, "action must be enable|disable|delete|recover", http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"affected": affected})
}

// handleRecoverAccount POST /api/channels/{provider}/accounts/{id}/recover
// 手动恢复冷却中的账号：清冷却与失败状态。
// 带 CAS：请求可携带 expected_cooling_until（列表接口返回的值），
// 与内存当前值不一致（如冷却刚被新的 429 刷新）则 409 拒绝——
// 恢复动作只在它所依据的观测仍然成立时执行。
func (g *Gateway) handleRecoverAccount(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	provider := r.PathValue("provider")
	snap := g.Snapshots.Load()
	ch := snap.FindChannel(provider)
	if ch == nil {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	var req struct {
		// 预期的冷却截止时间（RFC3339，来自列表接口）。空 = 跳过 CAS。
		ExpectedCoolingUntil string `json:"expected_cooling_until"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}

	for _, ref := range snap.AccountsFor(ch) {
		if ref.Spec.ID != id {
			continue
		}
		if !ref.Spec.Enabled {
			http.Error(w, "account disabled, enable it first", http.StatusBadRequest)
			return
		}
		if !ref.IsCooling(time.Now()) {
			http.Error(w, "account is not cooling", http.StatusBadRequest)
			return
		}
		// CAS：调用方看到的冷却截止与当前不一致 = 期间状态已变（如新 429 刷新），拒绝
		if req.ExpectedCoolingUntil != "" {
			cur := ref.Status().CoolingUntil.Format(time.RFC3339Nano)
			if cur != req.ExpectedCoolingUntil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error":                 "state changed since observation, re-test and retry",
					"current_cooling_until": cur,
				})
				return
			}
		}
		ref.State.Recover()
		writeJSON(w, map[string]string{"status": "recovered"})
		return
	}
	http.Error(w, "account not found", http.StatusNotFound)
}

// handleTestAccount POST /api/channels/{provider}/accounts/{id}/test
// 用该账号的凭据发最小真实调用（测通 = 能用）。
func (g *Gateway) handleTestAccount(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	provider := r.PathValue("provider")
	snap := g.Snapshots.Load()
	ch := snap.FindChannel(provider)
	if ch == nil {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	var ref *pool.AccountRef
	for _, a := range snap.AccountsFor(ch) {
		if a.Spec.ID == id {
			ref = a
			break
		}
	}
	if ref == nil {
		http.Error(w, "account not found", http.StatusNotFound)
		return
	}
	var req struct {
		Model string `json:"model"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	var target *config.Model
	models := ch.Models
	if req.Model != "" {
		for i := range models {
			if models[i].Name == req.Model {
				target = &models[i]
				break
			}
		}
		if target == nil {
			writeJSON(w, map[string]any{"ok": false, "error": "model not found"})
			return
		}
	} else {
		for i := range models {
			if models[i].Enabled {
				target = &models[i]
				break
			}
		}
		if target == nil {
			writeJSON(w, map[string]any{"ok": false, "error": "渠道无 enabled 模型"})
			return
		}
	}
	writeJSON(w, g.testModelCall(r.Context(), *target, ch, ref.Spec.Credential))
}

// handleUpdateAccount PUT /api/channels/{provider}/accounts/{id}
// body: {name?, max_concurrency?, enabled?, key?}（key 为空 = 保留原凭据）
func (g *Gateway) handleUpdateAccount(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	provider := r.PathValue("provider")
	snap := g.Snapshots.Load()
	ch := snap.FindChannel(provider)
	if ch == nil {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	var req struct {
		Name           string `json:"name"`
		Key            string `json:"key"`
		MaxConcurrency *int64 `json:"max_concurrency"`
		Enabled        *bool  `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}

	accounts, err := g.DB.ListUpstreamAccounts()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var cur *store.UpstreamAccount
	for i := range accounts {
		if accounts[i].ID == id && accounts[i].ChannelID == ch.ID {
			cur = &accounts[i]
			break
		}
	}
	if cur == nil {
		http.Error(w, "account not found", http.StatusNotFound)
		return
	}
	if req.Name != "" {
		cur.Name = req.Name
	}
	if req.MaxConcurrency != nil {
		if *req.MaxConcurrency < 0 {
			http.Error(w, "max_concurrency >= 0", http.StatusBadRequest)
			return
		}
		cur.MaxConcurrency = *req.MaxConcurrency
	}
	if req.Enabled != nil {
		cur.Enabled = *req.Enabled
	}
	if req.Key != "" {
		enc, fp, err := g.encryptAccountKey(req.Key)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		cur.EncryptedKey = enc
		cur.KeyFingerprint = fp
	}
	if err := g.DB.UpdateUpstreamAccount(*cur); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	g.reloadChannels()
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleDeleteAccount DELETE /api/channels/{provider}/accounts/{id}
func (g *Gateway) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	provider := r.PathValue("provider")
	snap := g.Snapshots.Load()
	ch := snap.FindChannel(provider)
	if ch == nil {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := g.DB.DeleteUpstreamAccount(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	g.reloadChannels()
	writeJSON(w, map[string]string{"status": "ok"})
}

// buildAccountRow 加密凭据并组装待入库的账号记录。
func (g *Gateway) buildAccountRow(channelID int64, name, key string, maxConcurrency int64) (*store.UpstreamAccount, error) {
	enc, fp, err := g.encryptAccountKey(key)
	if err != nil {
		return nil, err
	}
	return &store.UpstreamAccount{
		ChannelID:      channelID,
		Name:           name,
		EncryptedKey:   enc,
		KeyFingerprint: fp,
		MaxConcurrency: maxConcurrency,
		Enabled:        true,
	}, nil
}

// encryptAccountKey 加密凭据并计算指纹（服务端密钥参与 HMAC）。
func (g *Gateway) encryptAccountKey(key string) (enc []byte, fingerprint string, err error) {
	masterKey, err := keys.MasterKeyFromHex(g.masterKeyHex)
	if err != nil {
		return nil, "", err
	}
	enc, err = keys.Encrypt([]byte(key), masterKey)
	if err != nil {
		return nil, "", err
	}
	return enc, keys.Fingerprint(key, masterKey), nil
}
