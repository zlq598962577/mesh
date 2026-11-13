package web

import (
	"embed"
	"errors"
)

// errBadAssetName 表示请求的静态资源不在白名单里。
// 定义在这里而不是 server.go：它属于白名单逻辑本身，
// 且 server.go 由另一名成员并行编写，放在这里可避免重复声明。
var errBadAssetName = errors.New("web: 静态资源名不在白名单内")

// assetsFS 内嵌单页前端。前端没有任何构建步骤：
// index.html / app.js / style.css 就是最终产物，直接进二进制，离线可用。
//
//go:embed assets
var assetsFS embed.FS

// assetWhitelist 静态资源白名单。只认这三个名字，
// 任何其它路径（包括 ../、绝对路径、带分隔符的名字）一律拒绝，
// 从根上排除目录穿越，也不依赖调用方做净化。
var assetWhitelist = map[string]string{
	"index.html": "text/html; charset=utf-8",
	"app.js":     "text/javascript; charset=utf-8",
	"style.css":  "text/css; charset=utf-8",
}

// asset 按白名单读取内嵌资源。第二个返回值是 Content-Type。
func asset(name string) ([]byte, string, error) {
	ctype, ok := assetWhitelist[name]
	if !ok {
		return nil, "", errBadAssetName
	}
	b, err := assetsFS.ReadFile("assets/" + name)
	if err != nil {
		return nil, "", err
	}
	return b, ctype, nil
}
