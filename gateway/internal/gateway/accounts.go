package gateway

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"gateway/internal/config"
	"gateway/internal/keys"
	"gateway/internal/pool"
	"gateway/internal/store"
)

// nullableTime 零值时间输出 null。
func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// 账号管理 API（docs/design-upstream-account-pool.md §11）。
// 响应只含账号 ID、名称、启用状态、并发限制、key 配置状态与运行时健康摘要，
// 不返回明文 key、密文或可识别的长 key 前缀。

// handleListAccounts GET /api/channels/{provider}/accounts
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
	now := time.Now()
	out := []map[string]any{}
	for _, ref := range snap.AccountsFor(ch) {
		st := ref.Status()
		state := "healthy"
		if !ref.Spec.Enabled {
			state = "disabled"
		} else if now.Before(st.CoolingUntil) {
			state = "cooling"
		}
		out = append(out, map[string]any{
			"id":              ref.Spec.ID,
			"name":            ref.Spec.Name,
			"enabled":         ref.Spec.Enabled,
			"max_concurrency": ref.Spec.MaxConcurrency,
			"key_configured":  ref.Spec.Credential != "",
			"inflight":        st.Inflight,
			"state":           state,
			"cooling_until":   nullableTime(st.CoolingUntil),
			"cooldown_cause":  st.CooldownCause,
		})
	}
	writeJSON(w, out)
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
	enc, fp, err := g.encryptAccountKey(req.Key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a := &store.UpstreamAccount{
		ChannelID:      ch.ID,
		Name:           req.Name,
		EncryptedKey:   enc,
		KeyFingerprint: fp,
		MaxConcurrency: req.MaxConcurrency,
		Enabled:        true,
	}
	if err := g.DB.CreateUpstreamAccount(a); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	g.reloadChannels()
	writeJSON(w, map[string]any{"id": a.ID, "name": a.Name})
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
