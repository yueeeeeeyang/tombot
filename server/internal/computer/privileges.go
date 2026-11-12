package computer

// AgentCapabilities 是电脑代理最小权限的唯一清单，节点创建和容器内引导复验使用同一规则。
// 每次返回独立切片，调用方排序或序列化不能改变其他电脑的配置。
func AgentCapabilities() []string {
	return []string{"CHOWN", "DAC_OVERRIDE", "FOWNER", "SETUID", "SETGID", "KILL"}
}
