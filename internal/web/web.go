// Package web 提供静态页（试听 + 配置生成器）。
//
// 与 Python 版的差异：静态页用 //go:embed 打进二进制，部署时不用再挂目录。
// ADAPTER_STATIC_DIR 可指向外部目录覆盖，便于本地改页面不重新编译。
package web

import (
	"embed"
	"io/fs"
	"os"
)

//go:embed static
var embedded embed.FS

// FS 返回静态资源根目录。
func FS() fs.FS {
	if dir := os.Getenv("ADAPTER_STATIC_DIR"); dir != "" {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return os.DirFS(dir)
		}
	}
	sub, err := fs.Sub(embedded, "static")
	if err != nil {
		return nil
	}
	return sub
}
