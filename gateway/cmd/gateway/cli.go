package main

// 子命令入口。目前只有 keys set-upstream：录入上游 key（AES-GCM 加密存 PG）。

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"gateway/internal/keys"
	"gateway/internal/store"
)

func runCLI(args []string) {
	if len(args) >= 2 && args[0] == "keys" && args[1] == "set-upstream" {
		runSetUpstream(args[2:])
		return
	}
	fmt.Println("用法: gateway [keys set-upstream --provider <deepseek|minimax>]")
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
