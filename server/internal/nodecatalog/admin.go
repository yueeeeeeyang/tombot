package nodecatalog

import (
	"context"

	"io.yueyang/tom/internal/identity"
)

// Authorizer 由身份模块校验管理会话，不接受 HTTP 自行声明的角色。
type Authorizer interface {
	RequireAdmin(context.Context, identity.Principal, bool) error
}

// Admin 把目录登记暴露给管理平台；节点身份仍固定取自服务部署配置，
// 管理平台因此只能配置本机登记的那个节点及其镜像，不能凭空登记任意地址。
type Admin struct {
	service    *Service
	authorizer Authorizer
}

// NewAdmin 要求目录服务与授权器同时存在。
func NewAdmin(service *Service, authorizer Authorizer) (*Admin, error) {
	if service == nil || authorizer == nil {
		return nil, ErrInvalidInput
	}
	return &Admin{service: service, authorizer: authorizer}, nil
}

// Register 由管理员登记或复验节点与镜像目录；相同字段幂等，冲突时返回既有记录差异。
func (admin *Admin) Register(ctx context.Context, actor identity.Principal, registration Registration, requestID string) (Catalog, error) {
	if err := admin.authorizer.RequireAdmin(ctx, actor, true); err != nil {
		return Catalog{}, err
	}
	return admin.service.Register(ctx, registration, requestID)
}

// Get 读取指定镜像与固定节点的目录快照，不返回节点连接地址。
func (admin *Admin) Get(ctx context.Context, actor identity.Principal, imageID string) (Catalog, error) {
	if err := admin.authorizer.RequireAdmin(ctx, actor, false); err != nil {
		return Catalog{}, err
	}
	return admin.service.Get(ctx, imageID)
}
