package policy

import "io.yueyang/tom/internal/domain"

type Action struct {
	Class         string         `json:"action_class"`
	Pattern       string         `json:"pattern"`
	Risk          string         `json:"risk"`
	Title         string         `json:"title"`
	Details       map[string]any `json:"details"`
	ReviewerNote  string         `json:"reviewer_note,omitempty"`
	Review        *ReviewSummary `json:"review,omitempty"`
	ForceApproval bool           `json:"force_approval,omitempty"`
}

// IsHigherRisk 比较已验证的风险，用于审批后拒绝复用较低风险决策。
func IsHigherRisk(current, previous string) bool { return riskLevel(current) > riskLevel(previous) }

func ValidAction(class string) bool {
	switch class {
	case "group.read", "group.post", "conversation.read":
		return true
	case "fs.read", "fs.write", "fs.delete", "fs.write_outside_out", "shell.exec", "shell.privileged", "net.new_domain", "browser.login", "browser.read", "browser.interact", "browser.close", "secret.use", "delegate.spawn", "message.bot", "group.invite", "routine.create", "user.question":
		return true
	}
	return false
}

// NeedsApproval 不再拦截工具。风险仍用于分类，但任何严格度都直接执行。
func NeedsApproval(strictness, risk string) (bool, error) {
	switch risk {
	case "low", "medium", "high":
	default:
		return false, domain.NewError(500, "POLICY_INVALID", "Unknown action risk.")
	}
	switch strictness {
	case "strict", "standard", "loose":
		return false, nil
	}
	return false, domain.NewError(500, "POLICY_INVALID", "Unknown approval strictness.")
}
