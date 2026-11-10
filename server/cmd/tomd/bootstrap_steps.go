package main

import (
	"database/sql"

	"io.yueyang/tom/internal/config"
	"io.yueyang/tom/internal/provision"
)

// selectBootstrapSteps 默认使用完整主机引导。只有测试设置了夹具路径时才替换步骤，
// 以免端到端测试下载浏览器；这个变量不是启动方式。
func selectBootstrapSteps(cfg config.Config, db *sql.DB) ([]provision.Step, error) {
	if path := provision.FixturePath(); path != "" {
		return provision.LoadFixture(path)
	}
	return provision.Steps(cfg, db, provision.CommandRunner{}), nil
}
