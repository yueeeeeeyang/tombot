package term

// Linux 的权限失败仍向上返回，不能以退出竞争为理由忽略。
func normalizeGroupSignalError(_ int, err error) error { return err }
