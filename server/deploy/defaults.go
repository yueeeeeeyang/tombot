// Package deploy 保存随发布包交付的配置基线，避免生成器与部署示例各自维护默认值。
package deploy

import _ "embed"

//go:embed node.example.json
var nodeExample []byte

// NodeExample 返回独立副本，调用者修改不会影响后续生成。
func NodeExample() []byte { return append([]byte(nil), nodeExample...) }
