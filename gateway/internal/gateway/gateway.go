// Package gateway 组装网关的依赖与 HTTP 处理器：数据面（透传 + 计量）
// 与管理面（key / 渠道 / 用量 API）。main.go 只负责调用 New 并注册路由。
package gateway

import (
	"log"
	"net/http"

	"gateway/internal/config"
	"gateway/internal/keys"
	"gateway/internal/metering"
	"gateway/internal/pool"
	"gateway/internal/proxy"
	"gateway/internal/routing"
	"gateway/internal/store"
)

// Backing 网关需要的存储能力（PgStore 生产实现，MemStore 开发/测试实现）。
// nil = 内存模式（计量只打日志）。
type Backing interface {
	store.ChannelStore
	store.AccountStore
	store.UpstreamStore
	store.UsageStore
	keys.Store
}

// Gateway 持有全部可变依赖。快照经 routing.Store 原子读写，
// 其余字段构造后只读（管理面板口令除外，由内部锁保护）。
type Gateway struct {
	Snapshots   *routing.Store
	KeyMgr      *keys.Manager
	Meter       *metering.Meter
	DB          Backing      // nil = 内存模式
	Client      *http.Client // 数据面：上游透传（无整体超时）
	AdminClient *http.Client // 管理面：渠道测试 / 余额 / 远端模型
	Pool        *pool.Pool   // 上游账号池运行态（并发槽 / 冷却 / 计数）

	panelPassword adminAuth // 管理 API 口令
	masterKeyHex  string    // GATEWAY_MASTER_KEY（上游凭据加解密），可空
}

// New 装配网关。channels/upstreamKeys 为启动时加载好的初始配置与凭据，
// panelPassword 为管理 API 口令（空 = 无认证，仅开发），masterKeyHex 可空。
func New(channels []config.Channel, upstreamKeys map[string]string, keyMgr *keys.Manager, db Backing, panelPassword, masterKeyHex string) *Gateway {
	if upstreamKeys == nil {
		upstreamKeys = map[string]string{}
	}
	g := &Gateway{
		Snapshots:    routing.NewStore(&config.Config{Channels: channels}, upstreamKeys),
		KeyMgr:       keyMgr,
		Meter:        metering.NewMeter(),
		DB:           db,
		Pool:         pool.New(),
		masterKeyHex: masterKeyHex,
	}
	g.Client = proxy.NewUpstreamClient()
	g.AdminClient = proxy.NewAdminClient()
	g.panelPassword.set(panelPassword)
	return g
}

// Handler 注册全部路由并返回根 mux（含静态文件兜底）。
func (g *Gateway) Handler(staticDir string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// 数据面
	mux.HandleFunc("GET /v1/models", g.handleModels)
	mux.HandleFunc("/v1/", g.handleProxy)

	// 管理 API（均需 Bearer PANEL_PASSWORD，留空 = 无认证，仅开发）
	mux.HandleFunc("GET /api/keys", g.handleListKeys)
	mux.HandleFunc("POST /api/keys", g.handleCreateKey)
	mux.HandleFunc("POST /api/keys/revoke", g.handleRevokeKey)
	mux.HandleFunc("GET /api/keys/{hash}", g.handleGetKey)
	mux.HandleFunc("PUT /api/keys/{hash}", g.handleUpdateKey)
	mux.HandleFunc("POST /api/keys/{hash}/rotate", g.handleRotateKey)
	mux.HandleFunc("POST /api/keys/{hash}/quota", g.handleUpdateQuota)
	mux.HandleFunc("GET /api/usage", g.handleUsage)
	mux.HandleFunc("GET /api/dashboard", g.handleDashboard)
	mux.HandleFunc("GET /api/usage/timeseries", g.handleTimeseries)
	mux.HandleFunc("GET /api/usage/grouped", g.handleGrouped)
	mux.HandleFunc("GET /api/logs", g.handleLogs)
	mux.HandleFunc("GET /api/channels", g.handleChannels)
	mux.HandleFunc("POST /api/channels", g.handleCreateChannel)
	mux.HandleFunc("PUT /api/channels/{provider}", g.handleUpdateChannel)
	mux.HandleFunc("DELETE /api/channels/{provider}", g.handleDeleteChannel)
	mux.HandleFunc("POST /api/channels/{provider}/key", g.handleSetChannelKey)
	mux.HandleFunc("POST /api/channels/{provider}/test", g.handleTestChannel)
	mux.HandleFunc("GET /api/channels/{provider}/balance", g.handleChannelBalance)
	mux.HandleFunc("GET /api/channels/{provider}/remote-models", g.handleRemoteModels)
	mux.HandleFunc("POST /api/channels/{provider}/models", g.handleCreateModel)
	mux.HandleFunc("PUT /api/channels/{provider}/models/{name}", g.handleUpdateModel)
	mux.HandleFunc("DELETE /api/channels/{provider}/models/{name}", g.handleDeleteModel)
	mux.HandleFunc("GET /api/channels/{provider}/accounts", g.handleListAccounts)
	mux.HandleFunc("POST /api/channels/{provider}/accounts", g.handleCreateAccount)
	mux.HandleFunc("POST /api/channels/{provider}/accounts/import", g.handleImportAccounts)
	mux.HandleFunc("POST /api/channels/{provider}/accounts/batch", g.handleBatchAccounts)
	mux.HandleFunc("PUT /api/channels/{provider}/accounts/{id}", g.handleUpdateAccount)
	mux.HandleFunc("DELETE /api/channels/{provider}/accounts/{id}", g.handleDeleteAccount)
	mux.HandleFunc("POST /api/channels/{provider}/accounts/{id}/test", g.handleTestAccount)
	mux.HandleFunc("POST /api/channels/{provider}/accounts/{id}/recover", g.handleRecoverAccount)
	mux.HandleFunc("GET /api/settings/upstream", g.handleUpstreamStatus)
	mux.HandleFunc("POST /api/settings/password", g.handleUpdatePassword)
	mux.HandleFunc("GET /api/upstream/balance", g.handleUpstreamBalance)

	// 静态文件（前端面板），作为 fallback
	if staticDir != "" {
		mux.Handle("/", http.FileServer(http.Dir(staticDir)))
	}
	return mux
}

// reloadChannels 渠道/模型/账号配置变更后调用：从 PG 全量重读并发布新快照。
func (g *Gateway) reloadChannels() {
	if err := g.RebuildSnapshot(); err != nil {
		log.Printf("reload channels: %v", err)
	}
}

// RebuildSnapshot 从 PG 全量重读渠道与上游账号，解密凭据、对账账号运行态，
// 一次性发布新快照（发布失败继续用旧快照）。隐式 UpstreamKeys 保持不变
// （渠道没有显式账号时仍走 upstream_keys/env 兼容路径）。
func (g *Gateway) RebuildSnapshot() error {
	if g.DB == nil {
		return nil
	}
	channels, err := g.DB.LoadChannels()
	if err != nil {
		return err
	}
	accounts, err := g.DB.ListUpstreamAccounts()
	if err != nil {
		return err
	}
	refs, byChannel := g.buildAccountView(accounts)
	g.Snapshots.Update(func(cur *routing.Snapshot) *routing.Snapshot {
		return cur.WithConfig(&config.Config{Channels: channels}).WithAccounts(byChannel)
	})
	_ = refs
	return nil
}

// setAccountSpecs 全量替换显式账号视图（测试注入用；生产路径走 RebuildSnapshot）。
func (g *Gateway) setAccountSpecs(specs []*pool.AccountSpec) {
	refs := g.Pool.Reconcile(specs)
	byChannel := make(map[int64][]*pool.AccountRef, len(refs))
	for _, ref := range refs {
		byChannel[ref.Spec.ChannelID] = append(byChannel[ref.Spec.ChannelID], ref)
	}
	g.Snapshots.Update(func(cur *routing.Snapshot) *routing.Snapshot {
		return cur.WithAccounts(byChannel)
	})
}

// buildAccountView 解密账号凭据并对账运行态：同 ID 复用 State、消失的移除。
// 解密失败的账号跳过（不阻塞发布），只记日志。返回 refs 主要供测试观察。
func (g *Gateway) buildAccountView(accounts []store.UpstreamAccount) ([]*pool.AccountRef, map[int64][]*pool.AccountRef) {
	var specs []*pool.AccountSpec
	if masterKey, err := keys.MasterKeyFromHex(g.masterKeyHex); err == nil {
		for _, a := range accounts {
			plain, err := keys.Decrypt(a.EncryptedKey, masterKey)
			if err != nil {
				log.Printf("account %d(%s): decrypt failed, skip: %v", a.ID, a.Name, err)
				continue
			}
			specs = append(specs, &pool.AccountSpec{
				ID:             a.ID,
				ChannelID:      a.ChannelID,
				Name:           a.Name,
				Credential:     string(plain),
				MaxConcurrency: a.MaxConcurrency,
				Enabled:        a.Enabled,
			})
		}
	}
	refs := g.Pool.Reconcile(specs)
	byChannel := make(map[int64][]*pool.AccountRef, len(refs))
	for _, ref := range refs {
		byChannel[ref.Spec.ChannelID] = append(byChannel[ref.Spec.ChannelID], ref)
	}
	return refs, byChannel
}
