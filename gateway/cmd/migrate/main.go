// migrate 一次性迁移工具：
//   1) 给 channels 加 preset 列（JSONB 默认为 '{}'）。
//   2) 给 channels 加 auth_mode 列（默认 'bearer'）。
//   3) 对 DeepSeek/MiniMax 两条旧渠道灌默认端点+价格（仅 preset 为空时才写）。
//   4) opencode（go）渠道设 auth_mode='x_api_key'（opencode.ai 只认 x-api-key）。
// 部署新版本前手动跑一次。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"gateway/internal/config"
)

func main() {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		log.Fatal("need DATABASE_URL")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	_, err = pool.Exec(ctx, `ALTER TABLE channels ADD COLUMN IF NOT EXISTS preset JSONB NOT NULL DEFAULT '{}'`)
	if err != nil {
		log.Fatalf("alter table preset: %v", err)
	}
	fmt.Println("ALTER TABLE channels ADD COLUMN preset: OK")

	_, err = pool.Exec(ctx, `ALTER TABLE channels ADD COLUMN IF NOT EXISTS auth_mode TEXT NOT NULL DEFAULT 'bearer'`)
	if err != nil {
		log.Fatalf("alter table auth_mode: %v", err)
	}
	fmt.Println("ALTER TABLE channels ADD COLUMN auth_mode: OK")

	defaults := map[string]config.Preset{
		"deepseek": {
			Endpoints: map[string]string{
				"anthropic":        "https://api.deepseek.com/anthropic/v1/messages",
				"responses":        "https://api.deepseek.com/responses",
				"chat_completions": "https://api.deepseek.com/v1/chat/completions",
			},
			Pricing: map[string]config.PresetItem{
				"deepseek-v4-pro":   {InputPerM: 3, OutputPerM: 6, CacheReadPerM: 0.025, CacheWritePerM: 3},
				"deepseek-v4-flash": {InputPerM: 1, OutputPerM: 2, CacheReadPerM: 0.02, CacheWritePerM: 1},
			},
		},
		"minimax": {
			Endpoints: map[string]string{
				"anthropic":        "https://api.minimaxi.com/anthropic/v1/messages",
				"responses":        "https://api.minimaxi.com/v1/responses",
				"chat_completions": "https://api.minimaxi.com/v1/chat/completions",
			},
			Pricing: map[string]config.PresetItem{
				"MiniMax-M3": {InputPerM: 2.1, OutputPerM: 8.4, CacheReadPerM: 0.42, CacheWritePerM: 2.625},
			},
		},
	}
	for provider, p := range defaults {
		raw, _ := json.Marshal(p)
		tag, err := pool.Exec(ctx,
			`UPDATE channels SET preset=$1 WHERE provider=$2 AND (preset IS NULL OR preset = '{}'::jsonb)`,
			raw, provider)
		if err != nil {
			log.Printf("seed %s: %v", provider, err)
			continue
		}
		fmt.Printf("seed preset for %s: %d row(s)\n", provider, tag.RowsAffected())
	}

	// opencode（go）只认 x-api-key
	tag, err := pool.Exec(ctx, `UPDATE channels SET auth_mode='x_api_key' WHERE provider='opencode（go）'`)
	if err != nil {
		log.Printf("set opencode auth_mode: %v", err)
	} else {
		fmt.Printf("set auth_mode=x_api_key for opencode（go）: %d row(s)\n", tag.RowsAffected())
	}

	fmt.Println("DONE")
}