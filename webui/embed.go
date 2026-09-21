// Package webui 内嵌单页前端。三个静态文件，没有构建步骤。
package webui

import (
	"embed"
	"fmt"
	"hash/fnv"
	"io/fs"
	"net/http"
	"strings"
	"sync"
)

//go:embed index.html app.js style.css
var files embed.FS

// asset 在启动时把内嵌文件读进来并算好 ETag。
//
// 不能给静态资源设 max-age 长缓存：这些文件内嵌在二进制里，改完代码重新编译，
// 内容变了但 URL 没变，浏览器会一直用旧的——改了半天没生效，还以为是没编译。
// 内容哈希做 ETag 正好绕开这点：二进制换了哈希就换，天然是版本标识，
// 不需要构建期注入版本号，也不需要 query string。
type asset struct {
	body        []byte
	contentType string
	etag        string
}

var assets = sync.OnceValue(func() map[string]asset {
	m := map[string]asset{}
	for _, name := range []string{"index.html", "app.js", "style.css"} {
		b, err := fs.ReadFile(files, name)
		if err != nil {
			panic(fmt.Sprintf("内嵌资源缺失 %s: %v", name, err))
		}
		h := fnv.New64a()
		_, _ = h.Write(b)
		m[name] = asset{body: b, contentType: contentType(name), etag: fmt.Sprintf(`"%x"`, h.Sum64())}
	}
	return m
})

// Handler 提供静态资源；未命中的非 /api 路径一律回退 index.html，交给前端路由。
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		a, ok := assets()[name]
		if !ok {
			a = assets()["index.html"]
		}
		// no-cache 是「每次都要跟服务端核对」，不是「不缓存」。
		// 配上 ETag 之后，没变就是一次 304，没有响应体
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("ETag", a.etag)
		if r.Header.Get("If-None-Match") == a.etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", a.contentType)
		_, _ = w.Write(a.body)
	})
}

// Version 给健康检查用，让「现在跑的是哪个构建」有据可查。
func Version() string {
	a := assets()["app.js"]
	return strings.Trim(a.etag, `"`)
}

func contentType(name string) string {
	switch {
	case strings.HasSuffix(name, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(name, ".js"):
		return "application/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".css"):
		return "text/css; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}
