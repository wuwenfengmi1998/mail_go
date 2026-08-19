package web

// 静态资源内嵌进二进制：install.sh 只部署二进制 + templates，
// 此前 internal/web/static/ 未被复制导致线上 /static 全部 404。
// go:embed 使前端资源自包含，不再依赖部署目录与工作目录。

import "embed"

// staticFS 包含 internal/web/static/ 下的全部静态资源
// （目前为本地化的 Quill 编辑器 css/js）。
//
//go:embed all:static
var staticFS embed.FS
