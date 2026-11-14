// Package bundle 将管理平台构建产物编入服务端，运行时不读取外部前端目录。
package bundle

import (
	"embed"
	"io/fs"

	"io.yueyang/tom/web/admin"
)

// 缺少构建产物时编译失败，防止发布不带管理页面的服务端。
//
//go:embed dist
var distribution embed.FS

// New 校验内置页面并创建同源静态处理器；资源边界不符合约定时返回错误。
func New() (*admin.Handler, error) {
	root, err := fs.Sub(distribution, "dist")
	if err != nil {
		return nil, err
	}
	return admin.New(root)
}
