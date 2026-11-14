package nodecatalog

import (
	"context"

	"io.yueyang/tom/internal/identity"
)

// Authorizer 由身份模块校验管理会话，不接受 HTTP 自行声明的角色。
type Authorizer interface {
	RequireAdmin(context.Context, identity.Principal, bool) error
}
