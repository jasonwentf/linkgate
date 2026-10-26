// Package ui 内嵌面板静态资源。
//
// 面板是单页 HTML + 原生 JS，不引入前端框架，避免为一个小工具背上 Node 工具链。
package ui

import (
	"embed"
	"io/fs"
)

//go:embed assets
var embedded embed.FS

// IndexHTML 返回面板首页。
func IndexHTML() ([]byte, error) {
	return embedded.ReadFile("assets/index.html")
}

// FS 返回面板静态资源根。
func FS() fs.FS {
	sub, err := fs.Sub(embedded, "assets")
	if err != nil {
		return embedded
	}
	return sub
}
