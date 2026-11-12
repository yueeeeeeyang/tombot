//go:build !linux && !darwin

package deployment

import "errors"

func publishDirectory(string, string) error {
	return errors.New("当前系统不支持配置目录的原子发布")
}
