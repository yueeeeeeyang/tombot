// Package computerimage 提供与专用镜像同步发布的固定运行策略。
package computerimage

import (
	"bytes"
	_ "embed"
	"encoding/json"
)

//go:embed seccomp.json
var seccompProfile string

// SecurityOptions 将镜像验收所用策略直接交给 Docker API，不依赖宿主上的同名文件。
// 每次返回独立切片；嵌入的无效 JSON 属于发布构建错误，不能退回默认或不受限策略。
func SecurityOptions() []string {
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(seccompProfile)); err != nil {
		panic("专用镜像 seccomp 策略不是有效 JSON")
	}
	return []string{"no-new-privileges:true", "seccomp=" + compact.String()}
}
