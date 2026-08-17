// dbclean 一次性工具：清理 usage_logs 里的基础设施噪声记录
// （missing model / unknown model / no route / unauthorized）。
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		fmt.Println("need DATABASE_URL")
		os.Exit(1)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		fmt.Println("connect:", err)
		os.Exit(1)
	}
	defer pool.Close()
	tag, err := pool.Exec(ctx, `DELETE FROM usage_logs
		WHERE error = 'missing model'
		   OR error LIKE 'unknown model %'
		   OR error LIKE 'no route for %'
		   OR (status = 401 AND error = 'unauthorized')`)
	if err != nil {
		fmt.Println("delete:", err)
		os.Exit(1)
	}
	fmt.Println("deleted", tag.RowsAffected(), "noise rows")
}
