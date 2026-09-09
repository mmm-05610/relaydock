// Package console 内嵌控制台前端（构建时由 web/dist 拷贝至此）。
// 使单个二进制即为完整产品：无需外置静态目录。
package console

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var embedded embed.FS

// FS 返回控制台静态文件系统（dist 子目录）。
// 构建前需存在 internal/console/dist（build.sh 会自动拷贝 web/dist）。
func FS() (fs.FS, error) {
	return fs.Sub(embedded, "dist")
}
