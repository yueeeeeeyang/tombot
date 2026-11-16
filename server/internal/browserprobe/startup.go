package browserprobe

import "errors"

// ValidStartFailure 限制启动诊断的跨进程分类，不允许原始 Chrome 输出进入目录或日志。
func ValidStartFailure(code string) bool {
	switch code {
	case "browser_start_identity_failed", "browser_start_request_failed", "browser_start_directory_failed",
		"browser_start_pipe_failed", "browser_start_membership_failed", "browser_start_privileges_failed",
		"browser_start_launch_failed", "browser_start_group_failed", "browser_start_upload_failed",
		"browser_start_protocol_failed", "browser_start_sandbox_failed", "browser_start_profile_failed",
		"browser_start_process_exited", "browser_start_timeout", "browser_start_deadline_failed",
		"browser_start_budget_failed", "browser_start_state_failed":
		return true
	}
	return false
}

// StartFailure 保留更内层的已知启动阶段及取消原因，调用方仍须回收原服务实例。
func StartFailure(code string, cause error) error {
	var known *Failure
	if errors.As(cause, &known) && ValidStartFailure(known.Code) {
		return cause
	}
	if !ValidStartFailure(code) {
		code = "browser_start_protocol_failed"
	}
	return errors.Join(&Failure{Code: code}, cause)
}
