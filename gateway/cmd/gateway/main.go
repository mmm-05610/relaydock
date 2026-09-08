// gateway 主入口：依赖装配 + 路由注册 + 优雅停机。
// 数据面/管理面逻辑在 internal/gateway，透传与请求构造在 internal/proxy，
// 路由快照在 internal/routing。
package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gateway/internal/config"
	"gateway/internal/gateway"
	"gateway/internal/keys"
	"gateway/internal/store"
)

func main() {
	if len(os.Args) > 1 {
		runCLI(os.Args[1:])
		return
	}

	// 配置：config.yaml 种子（PG 空库时导入一次）
	cfg, err := config.Load("config.yaml")
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	log.Printf("loaded %d channels", len(cfg.Channels))

	// 存储：有 DATABASE_URL 用 PG，否则内存（本地开发）
	var db *store.PgStore
	var keyMgr *keys.Manager
	ctx := context.Background()
	if dbURL := os.Getenv("DATABASE_URL"); dbURL != "" {
		pg, err := store.NewPgStore(ctx, dbURL)
		if err != nil {
			log.Fatalf("connect pg: %v", err)
		}
		db = pg
		keyMgr = keys.NewManager(pg)
		log.Printf("using PostgreSQL store")
	} else {
		keyMgr = keys.NewManager(store.NewMemStore())
		if raw, err := keyMgr.CreateKey("bootstrap", "", "", 0); err == nil {
			log.Printf("bootstrap key (仅此一次可见): %s", raw)
		}
		log.Printf("using in-memory store (no DATABASE_URL)")
	}

	// 渠道从 PG 加载（空则从 config.yaml 种子导入）
	channels := cfg.Channels
	if db != nil {
		loaded, err := db.LoadChannels()
		if err != nil {
			log.Fatalf("load channels: %v", err)
		}
		if len(loaded) > 0 {
			channels = loaded
			log.Printf("loaded %d channels from DB", len(loaded))
		} else {
			for _, ch := range cfg.Channels {
				ch.Enabled = true
				for i := range ch.Models {
					ch.Models[i].Enabled = true
				}
				if err := db.CreateChannel(ch); err != nil {
					log.Printf("seed channel %s: %v", ch.Provider, err)
				}
			}
			log.Printf("seeded %d channels from config.yaml", len(cfg.Channels))
		}
	}

	// 上游凭据：PG（AES-GCM 加密）优先，env 回退
	panelPassword := os.Getenv("PANEL_PASSWORD")
	upstreamKeys := loadUpstreamKeys(db, channels)
	if panelPassword == "" {
		log.Printf("⚠️ 未设置 PANEL_PASSWORD，管理 API 无认证（仅限开发）")
	}

	// 注意：db 声明为 *PgStore，nil 时不能直接传给 Backing 接口参数
	//（typed-nil 陷阱：接口非 nil 但底层指针为 nil，网关内 DB==nil 检查会失效）
	cfg.Channels = channels
	var dbBacking gateway.Backing
	if db != nil {
		dbBacking = db
	}
	g := gateway.New(cfg, upstreamKeys, keyMgr, dbBacking, panelPassword, os.Getenv("GATEWAY_MASTER_KEY"))

	// PG 模式：从库加载渠道（含 ID）+ 上游账号池，发布初始快照
	if db != nil {
		if err := g.RebuildSnapshot(); err != nil {
			log.Fatalf("rebuild snapshot: %v", err)
		}
		log.Printf("snapshot rebuilt (channels + accounts)")
	}

	staticDir := os.Getenv("STATIC_DIR")
	if staticDir == "" {
		staticDir = "../web/dist"
	}

	srv := &http.Server{
		Addr:              ":8080",
		Handler:           g.Handler(staticDir),
		ReadHeaderTimeout: 30 * time.Second,
	}

	// 优雅停机：收到信号后停止接受新连接，给在途请求（含 SSE 长流）最多 30s 收尾
	done := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Printf("shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("shutdown: %v", err)
		}
		close(done)
	}()

	// 双栈显式监听：本机开发环境（WSL virtioproxy/mirrored 网络）下，
	// ":8080" 双栈 socket 的 IPv4-mapped 回环（127.0.0.1）会被静默拒绝，
	// Windows 侧的 localhost 转发恰好落在这一层。分别监听 tcp4/tcp6，
	// 保证 127.0.0.1 与 [::1] 都可达；生产 Linux 双监听同样无害。
	ln4, err := net.Listen("tcp4", "0.0.0.0:8080")
	if err != nil {
		log.Fatalf("listen tcp4: %v", err)
	}
	ln6, err := net.Listen("tcp6", "[::]:8080")
	if err != nil {
		log.Printf("listen tcp6: %v（仅 IPv4 可用）", err)
	}
	log.Printf("gateway listening on tcp4 0.0.0.0:8080 + tcp6 [::]:8080")
	go func() {
		if err := srv.Serve(ln4); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("serve tcp4: %v", err)
		}
	}()
	if ln6 != nil {
		if err := srv.Serve(ln6); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("serve tcp6: %v", err)
		}
	}
	<-done
}

// loadUpstreamKeys 从 PG（加密）或 env 加载各渠道上游 key。
func loadUpstreamKeys(db *store.PgStore, channels []config.Channel) map[string]string {
	out := map[string]string{}
	envFallback := map[string]string{
		"deepseek": os.Getenv("DEEPSEEK_API_KEY"),
		"minimax":  os.Getenv("MINIMAX_API_KEY"),
	}
	masterKey, _ := keys.MasterKeyFromHex(os.Getenv("GATEWAY_MASTER_KEY"))

	seen := map[string]bool{}
	for _, ch := range channels {
		if seen[ch.Provider] {
			continue
		}
		seen[ch.Provider] = true
		// PG 加密读取（master key 存在 + db 非空）
		if db != nil && masterKey != nil {
			if enc, err := db.GetUpstreamKey(ch.Provider); err == nil && len(enc) > 0 {
				if plain, err := keys.Decrypt(enc, masterKey); err == nil {
					out[ch.Provider] = string(plain)
					continue
				}
			}
		}
		out[ch.Provider] = envFallback[ch.Provider]
	}
	return out
}
