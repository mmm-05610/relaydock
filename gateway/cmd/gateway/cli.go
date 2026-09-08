package main

// 子命令入口：keys set-upstream（录入上游 key）/ seed-demo（灌演示数据）。

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"os"
	"strings"
	"time"

	"gateway/internal/gateway"
	"gateway/internal/keys"
	"gateway/internal/store"
)

func runCLI(args []string) {
	if len(args) >= 2 && args[0] == "keys" && args[1] == "set-upstream" {
		runSetUpstream(args[2:])
		return
	}
	if len(args) >= 1 && args[0] == "seed-demo" {
		runSeedDemo()
		return
	}
	fmt.Println("用法: gateway [keys set-upstream --provider <p> | seed-demo]")
}

func runSetUpstream(args []string) {
	provider := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--provider" && i+1 < len(args) {
			provider = args[i+1]
		}
	}
	if provider == "" {
		log.Fatal("用法: gateway keys set-upstream --provider <deepseek|minimax>")
	}
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("需要 DATABASE_URL 环境变量")
	}
	masterKey, err := keys.MasterKeyFromHex(os.Getenv("GATEWAY_MASTER_KEY"))
	if err != nil {
		log.Fatalf("GATEWAY_MASTER_KEY 需 32 字节 hex: %v", err)
	}
	fmt.Printf("输入 %s 的 API key（会回显，注意遮挡）: ", provider)
	apiKey, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		log.Fatalf("读取失败: %v", err)
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		log.Fatal("key 不能为空")
	}
	encrypted, err := keys.Encrypt([]byte(apiKey), masterKey)
	if err != nil {
		log.Fatalf("加密失败: %v", err)
	}
	pg, err := store.NewPgStore(context.Background(), dbURL)
	if err != nil {
		log.Fatalf("连 PG: %v", err)
	}
	if err := pg.SetUpstreamKey(provider, encrypted); err != nil {
		log.Fatalf("存 PG: %v", err)
	}
	fmt.Printf("✅ 已加密存储 %s 的 key（重启 gateway 生效）\n", provider)
}

// runSeedDemo 灌一套演示数据：四种状态的虚拟 key、14 天用量日志（约 300 条）、
// 若干带全文的请求。幂等：settings 里留标记，重复执行跳过。
func runSeedDemo() {
	backing, err := openStoreFromEnv()
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	if done, _ := backing.GetSetting("seed-demo-done"); done == "1" {
		fmt.Println("演示数据已存在，跳过（如需重灌请删除 settings 中的 seed-demo-done）。")
		return
	}
	mgr := keys.NewManager(backing)

	// 1. 虚拟 key 四态：正常高用量 / 不限额度 / 已耗尽 / 已过期
	type keySpec struct {
		name, owner, agent, allowed string
		quota, used                 float64
		expiresIn                   time.Time
		touch                       bool
	}
	specs := []keySpec{
		{"claude-code-main", "maoqh", "claude-code", "", 50, 32.5, time.Time{}, true},
		{"codex-cli", "maoqh", "codex", "", 0, 18.2, time.Now().AddDate(0, 0, 21), true},
		{"batch-worker", "maoqh", "opencode", "deepseek-v4-flash", 10, 10.0, time.Time{}, false},
		{"demo-expired", "demo", "claude-code", "", 5, 1.2, time.Now().AddDate(0, 0, -3), false},
	}
	keyIDs := map[string]int64{}
	for _, sp := range specs {
		raw, err := mgr.CreateKeyWithModels(sp.name, sp.owner, sp.agent, sp.quota, sp.allowed, sp.expiresIn)
		if err != nil {
			log.Fatalf("create key %s: %v", sp.name, err)
		}
		k, _ := mgr.GetKey(keys.SHA256Hash(raw))
		keyIDs[sp.name] = k.ID
		if sp.used > 0 {
			_ = mgr.AddUsage(k.KeyHash, sp.used)
		}
		if sp.touch {
			mgr.TouchLastUsed(k.KeyHash)
		}
		fmt.Printf("  key %-18s（明文仅本次: %s）\n", sp.name, raw)
	}

	// 2. 14 天用量日志：模型加权 + 状态分布 + 缓存命中 + 成本按单价计算
	pricing := map[string][4]float64{ // 输入/输出/缓存读/缓存写 ¥/M
		"deepseek-v4-pro":   {3, 6, 0.025, 3},
		"deepseek-v4-flash": {1, 2, 0.025, 1},
		"MiniMax-M3":        {2.1, 8.4, 0, 0},
	}
	type mSpec struct {
		model, protocol string
		weight, chID    int
		latency         [2]int
	}
	models := []mSpec{
		{"deepseek-v4-pro", "anthropic", 55, 1, [2]int{1200, 9000}},
		{"deepseek-v4-flash", "chat_completions", 25, 1, [2]int{300, 1500}},
		{"MiniMax-M3", "chat_completions", 20, 2, [2]int{500, 3000}},
	}
	totalWeight := 0
	for _, m := range models {
		totalWeight += m.weight
	}
	statuses := []struct {
		code, weight int
		err          string
	}{{200, 93, ""}, {429, 3, "rate limited by upstream"}, {500, 2, "upstream internal error"}, {400, 2, "invalid request"}}

	now := time.Now()
	rnd := rand.New(rand.NewPCG(42, 7))
	count := 0
	for d := 13; d >= 0; d-- {
		base := 14 + rnd.IntN(18)
		for i := 0; i < base; i++ {
			pick := rnd.IntN(totalWeight)
			ms := models[0]
			for _, m := range models {
				if pick < m.weight {
					ms = m
					break
				}
				pick -= m.weight
			}
			keyName := "claude-code-main"
			switch rnd.IntN(10) {
			case 0, 1:
				keyName = "codex-cli"
			case 2:
				keyName = "batch-worker"
			}
			sp := rnd.IntN(100)
			status, errText := 200, ""
			acc := 0
			for _, st := range statuses {
				acc += st.weight
				if sp < acc {
					status, errText = st.code, st.err
					break
				}
			}
			input := 2000 + rnd.IntN(58000)
			output := 100 + rnd.IntN(3800)
			var cacheRead int64
			if status == 200 && rnd.IntN(10) < 6 {
				cacheRead = int64(float64(input) * (0.3 + rnd.Float64()*0.5))
			}
			p := pricing[ms.model]
			cost := float64(input)/1e6*p[0] + float64(output)/1e6*p[1] + float64(cacheRead)/1e6*p[2]
			attempts := 1
			if status == 429 && rnd.IntN(3) == 0 {
				attempts = 2 + rnd.IntN(2)
			}
			latency := ms.latency[0] + rnd.IntN(ms.latency[1]-ms.latency[0])
			if status >= 400 {
				latency = 100 + rnd.IntN(900)
			}
			created := now.AddDate(0, 0, -d).Add(time.Duration(rnd.IntN(24))*time.Hour + time.Duration(rnd.IntN(60))*time.Minute)
			reqID := ""
			if status == 200 && count%20 == 0 {
				reqID = fmt.Sprintf("msg_seed_%04d", count)
			}
			var keyID int64
			if id, ok := keyIDs[keyName]; ok {
				keyID = id
			}
			if err := backing.InsertUsageLog(store.UsageLog{
				KeyID: keyID, Model: ms.model, UpstreamModel: ms.model, Protocol: ms.protocol,
				InputTokens: int64(input), OutputTokens: int64(output), CacheReadTokens: cacheRead,
				Cost: cost, LatencyMs: int64(latency), Status: status, Error: errText,
				RequestID: reqID, Unmetered: status == 200 && rnd.IntN(20) == 0,
				ChannelID: int64(ms.chID), Attempts: attempts, CreatedAt: created,
			}); err != nil {
				log.Fatalf("insert usage log: %v", err)
			}
			count++

			// 全文（模拟 anthropic 请求/响应，与 usage_log 的 request_id 关联）
			if reqID != "" {
				rb := fmt.Sprintf(`{"model":%q,"max_tokens":1024,"stream":true,"system":[{"type":"text","text":"你是资深 Go 工程师，回答简洁。"}],"messages":[{"role":"user","content":[{"type":"text","text":"演示数据：帮我审查这段并发的 channel 关闭逻辑，注意 select 与 context 取消的配合。"}]}],"metadata":{"user_id":"demo"}}`, ms.model)
				respBody := fmt.Sprintf(`{"id":%q,"type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":"演示响应：select 里对已关闭 channel 的接收会立即返回零值，应使用 v, ok := <-ch 区分；context 取消时建议用 sync.Once 统一关闭，避免双重 close panic。"}],"stop_reason":"end_turn","usage":{"input_tokens":%d,"output_tokens":%d}}`, reqID, ms.model, input, output)
				_ = backing.InsertLogBody(&store.LogBody{
					UsageRequestID: reqID, Model: ms.model,
					RequestBody: []byte(rb), ResponseBody: []byte(respBody),
					Status: status,
				})
			}
		}
	}
	fmt.Printf("  usage logs: %d 条（14 天）+ 全文 %d 条\n", count, (count+19)/20)

	_ = backing.SetSetting("seed-demo-done", "1")
	fmt.Println("✅ 演示数据灌入完成，刷新控制台即可查看。")
}

// openStoreFromEnv CLI 子命令共用的存储选择（与 main 一致）。
func openStoreFromEnv() (gateway.Backing, error) {
	if os.Getenv("DATABASE_URL") != "" {
		return store.NewPgStore(context.Background(), os.Getenv("DATABASE_URL"))
	}
	if os.Getenv("MEMORY") == "1" {
		return store.NewMemStore(), nil
	}
	path := os.Getenv("SQLITE_PATH")
	if path == "" {
		path = "data/relaydock.db"
	}
	return store.NewSqliteStore(path)
}
