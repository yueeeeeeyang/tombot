package identity

import "strings"

// subject 描述一类身份拥有的表与归属列。
// 普通用户与管理员是两套独立身份：表名和归属列不同，查询语句按同一模板拼接，
// 避免两套认证逻辑各自演化出不一致的状态机。
// 模板中的表名只能来自本文件的常量，不接受任何外部输入。
type subject struct {
	accounts         string
	credentials      string
	devices          string
	sessions         string
	tokens           string
	mfa              string
	activationTokens string
	activationMFA    string
	// owner 是身份归属列名：用户表用 user_id，管理员表用 administrator_id。
	owner string
	// hasScope 表示会话表是否保留 scope 列；管理员会话表恒为管理会话，不需要该列。
	hasScope bool
	// role 仅用于展示与审计，不再作为数据库中的可变字段。
	role string
}

var (
	userSubject = subject{
		accounts:         "tombot.users",
		credentials:      "tombot.user_credentials",
		devices:          "tombot.devices",
		sessions:         "tombot.user_sessions",
		tokens:           "tombot.session_tokens",
		mfa:              "tombot.user_mfa",
		activationTokens: "tombot.activation_tokens",
		activationMFA:    "tombot.activation_mfa",
		owner:            "user_id",
		hasScope:         true,
		role:             "user",
	}
	administratorSubject = subject{
		accounts:    "tombot.administrators",
		credentials: "tombot.administrator_credentials",
		devices:     "tombot.administrator_devices",
		sessions:    "tombot.administrator_sessions",
		tokens:      "tombot.administrator_session_tokens",
		mfa:         "tombot.administrator_mfa",
		owner:       "administrator_id",
		role:        "admin",
	}
)

// subjectForScope 只接受两个固定作用域；管理会话只能使用管理员身份表。
func subjectForScope(scope string) (subject, bool) {
	switch scope {
	case "desktop":
		return userSubject, true
	case "admin":
		return administratorSubject, true
	default:
		return subject{}, false
	}
}

// render 展开模板中的表名与归属列。模板中的表名只能来自本文件的常量，
// 不接受任何外部输入；出现未定义占位符时保持原样，让写法错误在测试中直接暴露。
func (value subject) render(template string) string {
	return strings.NewReplacer(
		"{accounts}", value.accounts,
		"{credentials}", value.credentials,
		"{devices}", value.devices,
		"{sessions}", value.sessions,
		"{tokens}", value.tokens,
		"{mfa}", value.mfa,
		"{activationTokens}", value.activationTokens,
		"{activationMFA}", value.activationMFA,
		"{owner}", value.owner,
	).Replace(template)
}
