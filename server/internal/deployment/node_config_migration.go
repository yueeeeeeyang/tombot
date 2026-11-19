package deployment

import (
	"encoding/json"
	"errors"
	"fmt"

	"io.yueyang/tom/internal/computeragent"
	"io.yueyang/tom/internal/nodeapp"
)

// migrateBrowserLimits 只转换安装快照中的旧字段，保留同级超时配置。安装请求允许只覆盖部分限额。
func migrateBrowserLimits(encoded []byte, isOverride bool) ([]byte, error) {
	if len(encoded) == 0 && isOverride {
		return encoded, nil
	}
	var fields, browser map[string]json.RawMessage
	if err := decodePreparation(encoded, &fields); err != nil {
		return nil, err
	}
	if fields["browser"] == nil {
		return encoded, nil
	}
	if err := decodePreparation(fields["browser"], &browser); err != nil {
		return nil, err
	}
	if browser["limits"] == nil {
		return encoded, nil
	}
	var limits computeragent.BrowserLimits
	if err := decodePreparation(browser["limits"], &limits); err != nil {
		return nil, fmt.Errorf("旧版 browser.limits 格式无效：%w", err)
	}
	if !isOverride {
		if err := limits.Validate(); err != nil {
			return nil, err
		}
	}
	delete(browser, "limits")
	updated, err := json.Marshal(browser)
	if err != nil {
		return nil, err
	}
	fields["browser"] = updated
	return json.Marshal(fields)
}

// parseRepairNodeConfig 读取已核对原文摘要的旧配置，迁移后的运行配置仍由节点正式解析器严格校验。
func parseRepairNodeConfig(body []byte) (nodeapp.Config, error) {
	if len(body) > nodeapp.MaximumConfigBytes {
		return nodeapp.Config{}, errors.New("已安装的 node.json 超过节点配置大小上限")
	}
	migrated, err := migrateBrowserLimits(body, false)
	if err == nil {
		var config nodeapp.Config
		config, err = nodeapp.ParseConfig(migrated)
		if err == nil {
			return config, nil
		}
	}
	return nodeapp.Config{}, fmt.Errorf("已安装的 node.json 配置无效：%w", err)
}
