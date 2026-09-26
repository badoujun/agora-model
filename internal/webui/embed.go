// Package webui 内嵌构建后的 Web UI 静态资源，并提供 SPA 路由与回退。
//
// dist 目录由 `npm --prefix web run build` 生成（vite 的 outDir 指向本包的 dist）。
// 仓库里保留一份 dist/robots.txt 作为占位，保证未构建前端时本包仍可编译。
package webui

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// FS 返回以 dist 为根的文件系统。
func FS() (fs.FS, error) {
	return fs.Sub(distFS, "dist")
}

// Available 报告前端是否已构建（存在 index.html）。
func Available(fsys fs.FS) bool {
	if fsys == nil {
		return false
	}
	info, err := fs.Stat(fsys, "index.html")
	return err == nil && !info.IsDir()
}

// Register 把静态资源与 SPA fallback 挂载到 mux。
//
// 注意：/api、/v1 与 /healthz 下的未知路径仍返回 404，不会被 SPA fallback 吞掉，
// 以免前端路由掩盖接口拼写错误。
func Register(mux *http.ServeMux, fsys fs.FS) error {
	index, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		return fmt.Errorf("前端未构建（缺少 index.html）：请先执行 npm --prefix web run build: %w", err)
	}
	files := http.FileServer(http.FS(fsys))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "healthz" || strings.HasPrefix(path, "api/") || strings.HasPrefix(path, "v1/") {
			http.NotFound(w, r)
			return
		}
		if path == "" {
			writeIndex(w, index)
			return
		}
		if info, statErr := fs.Stat(fsys, path); statErr == nil && !info.IsDir() {
			if strings.HasPrefix(path, "assets/") {
				// 文件名带内容哈希，可长期缓存
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			files.ServeHTTP(w, r)
			return
		}
		// SPA fallback：交由前端路由处理
		writeIndex(w, index)
	})
	return nil
}

func writeIndex(w http.ResponseWriter, index []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(index)
}
