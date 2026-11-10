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

// NeedsApproval 只对已通过白名单及规则判断的动作生效；未知风险必须拒绝而非默认低风险。
func NeedsApproval(strictness, risk string) (bool, error) {
	switch risk {
	case "low", "medium", "high":
	default:
		return false, domain.NewError(500, "POLICY_INVALID", "Unknown action risk.")
	}
	switch strictness {
	case "strict":
		return true, nil
	case "standard":
		return risk != "low", nil
	case "loose":
		return risk == "high", nil
	}
	return false, domain.NewError(500, "POLICY_INVALID", "Unknown approval strictness.")
}
