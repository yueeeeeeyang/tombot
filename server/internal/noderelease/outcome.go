package noderelease

import (
	"regexp"
	"strings"
)

var reasonCode = regexp.MustCompile(`^[a-z][a-z0-9_]{0,95}$`)

// ValidUpgradeStep 仅允许协议中的步骤和有界的运行错误分类进入升级回执。
func ValidUpgradeStep(step string) bool {
	name, reason, hasReason := strings.Cut(step, "/")
	switch name {
	case "verification", "verification_quarantined", "computer_quarantined":
		return hasReason && reasonCode.MatchString(reason)
	case "download", "preflight", "maintenance", "stop", "activate", "start", "finish", "heartbeat", "node_draining", "draining", "switching", "awaiting_worker", "active_session", "computer_upgrade", "computer_transition", "verification_lease", "rollback", "rolled_back":
		return !hasReason
	}
	return false
}

// ParseUpgradeOutcome 同时校验本机持久回执与已签名的远程结果，新的错误详情随重放保留。
func ParseUpgradeOutcome(outcome string) (state, step string, ok bool) {
	state, step, hasStep := strings.Cut(outcome, "/")
	switch state {
	case "failed", "busy":
		return state, step, !hasStep || ValidUpgradeStep(step)
	case "started", "program_applied":
		return state, step, !hasStep
	}
	return "", "", false
}
