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

	"encoding/json"

	"gateway/internal/config"
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
	if len(args) >= 1 && args[0] == "merge-channels" {
		runMergeChannels(args[1:])
		return
	}
	fmt.Println("用法: gateway [keys set-upstream --provider <p> | seed-demo | merge-channels --into <p> --sources <p1,p2,...>]")
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

// runMergeChannels 把多个旧式渠道（一渠道一 key）收敛为一个渠道的多个账号：
//   - 各源渠道的上游 key 解密后收编为目标渠道的 upstream_account（指纹去重）
//   - 模型路由取并集（同名模型的协议路由合并），定价取非零者优先
//   - 源渠道置为禁用（不删除，保留历史归因与回滚余地）
//   - 写出合并前快照 JSON 到当前目录
func runMergeChannels(args []string) {
	into, sources := "", ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--into":
			if i+1 < len(args) {
				into = args[i+1]
			}
		case "--sources":
			if i+1 < len(args) {
				sources = args[i+1]
			}
		}
	}
	if into == "" || sources == "" {
		log.Fatal("用法: gateway merge-channels --into <provider> --sources <p1,p2,...>")
	}
	masterKey, err := keys.MasterKeyFromHex(os.Getenv("GATEWAY_MASTER_KEY"))
	if err != nil {
		log.Fatalf("GATEWAY_MASTER_KEY 需 32 字节 hex: %v", err)
	}
	backing, err := openStoreFromEnv()
	if err != nil {
		log.Fatalf("open store: %v", err)
	}

	channels, err := backing.LoadChannels()
	if err != nil {
		log.Fatalf("load channels: %v", err)
	}
	sourceList := strings.Split(sources, ",")
	for i := range sourceList {
		sourceList[i] = strings.TrimSpace(sourceList[i])
	}

	// 快照备份
	snapshot := map[string]any{"into": into, "sources": sourceList, "channels": channels}
	raw, _ := json.MarshalIndent(snapshot, "", "  ")
	bakFile := fmt.Sprintf("merge-backup-%s.json", time.Now().Format("20060102-150405"))
	if err := os.WriteFile(bakFile, raw, 0o600); err != nil {
		log.Fatalf("write backup: %v", err)
	}
	fmt.Printf("  源数据快照: %s\n", bakFile)

	var target *config.Channel
	var sourceChs []*config.Channel
	for i := range channels {
		switch {
		case channels[i].Provider == into:
			target = &channels[i]
		default:
			for _, sp := range sourceList {
				if channels[i].Provider == sp {
					ch := channels[i]
					sourceChs = append(sourceChs, &ch)
				}
			}
		}
	}
	if target == nil {
		log.Fatalf("目标渠道 %q 不存在", into)
	}
	if len(sourceChs) == 0 {
		log.Fatalf("源渠道均不存在: %s", sources)
	}

	// 1. 收编凭据为账号（同一 master key 加密，密文直接复用）
	adopted := 0
	existing, _ := backing.ListUpstreamAccounts()
	seenFP := map[string]bool{}
	for _, a := range existing {
		if a.ChannelID == target.ID {
			seenFP[a.KeyFingerprint] = true
		}
	}
	for _, sc := range sourceChs {
		enc, err := backing.GetUpstreamKey(sc.Provider)
		if err != nil || len(enc) == 0 {
			fmt.Printf("  跳过 %s：无上游凭据\n", sc.Provider)
			continue
		}
		plain, err := keys.Decrypt(enc, masterKey)
		if err != nil {
			log.Fatalf("decrypt %s: %v", sc.Provider, err)
		}
		fp := keys.Fingerprint(string(plain), masterKey)
		if seenFP[fp] {
			fmt.Printf("  跳过 %s：凭据指纹重复\n", sc.Provider)
			continue
		}
		a := &store.UpstreamAccount{
			ChannelID:      target.ID,
			Name:           sc.Provider,
			EncryptedKey:   enc,
			KeyFingerprint: fp,
			Enabled:        true,
			CredentialType: "api_key",
		}
		if err := backing.CreateUpstreamAccount(a); err != nil {
			log.Fatalf("create account %s: %v", sc.Provider, err)
		}
		seenFP[fp] = true
		adopted++
		fmt.Printf("  收编账号 %s（id=%d）\n", sc.Provider, a.ID)
	}

	// 2. 合并模型：路由并集、定价取非零、enabled 取或
	merged := map[string]config.Model{}
	order := []string{}
	add := func(m config.Model) {
		m.Provider = into
		if exist, ok := merged[m.Name]; ok {
			routes := exist.Routes
			for k, v := range m.Routes {
				routes[k] = v
			}
			exist.Routes = routes
			if pricingSum(m.Pricing) > pricingSum(exist.Pricing) {
				exist.Pricing = m.Pricing
			}
			exist.Enabled = exist.Enabled || m.Enabled
			if m.ContextLength > exist.ContextLength {
				exist.ContextLength = m.ContextLength
			}
			merged[m.Name] = exist
		} else {
			order = append(order, m.Name)
			merged[m.Name] = m
		}
	}
	for _, m := range target.Models {
		add(m)
	}
	for _, sc := range sourceChs {
		for _, m := range sc.Models {
			add(m)
		}
	}
	final := make([]config.Model, 0, len(order))
	for _, name := range order {
		final = append(final, merged[name])
	}
	target.Models = final
	target.Enabled = true
	if err := backing.UpdateChannel(*target); err != nil {
		log.Fatalf("update target: %v", err)
	}
	fmt.Printf("  目标 %s 模型合并后: %d 个\n", into, len(final))

	// 3. 源渠道禁用
	for _, sc := range sourceChs {
		sc.Enabled = false
		if err := backing.UpdateChannel(*sc); err != nil {
			log.Printf("disable %s: %v", sc.Provider, err)
		}
		fmt.Printf("  源渠道 %s 已禁用\n", sc.Provider)
	}
	fmt.Printf("✅ 收敛完成：收编 %d 个账号，合并 %d 个模型。重启网关生效。\n", adopted, len(final))
}

func pricingSum(p config.Pricing) float64 {
	return p.InputPerM + p.OutputPerM + p.CacheReadPerM + p.CacheWritePerM
}
