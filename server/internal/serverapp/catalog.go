package serverapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/nodecatalog"
	"io.yueyang/tom/internal/store/postgres"
)

// CatalogRequest 只供本机部署命令使用，节点 ID 与连接入口固定读取服务配置。
type CatalogRequest struct {
	Action         string `json:"action"`
	Name           string `json:"name,omitempty"`
	Capacity       int    `json:"capacity,omitempty"`
	ImageID        string `json:"image_id"`
	ImageReference string `json:"image_reference,omitempty"`
}

type CatalogResult struct {
	Action    string              `json:"action"`
	RequestID string              `json:"request_id"`
	Catalog   nodecatalog.Catalog `json:"catalog"`
}

func (request CatalogRequest) registration() nodecatalog.Registration {
	return nodecatalog.Registration{Name: request.Name, Capacity: request.Capacity, ImageID: request.ImageID, ImageReference: request.ImageReference}
}

func (request CatalogRequest) validate() error {
	if request.Action == "register" {
		return request.registration().Validate()
	}
	id, err := ulid.ParseStrict(request.ImageID)
	if request.Action != "status" || err != nil || id.String() != request.ImageID || request.Name != "" || request.Capacity != 0 || request.ImageReference != "" {
		return nodecatalog.ErrInvalidInput
	}
	return nil
}

// LoadCatalogRequest 允许带缩进的部署文件，拒绝重复键、大小写别名、未知字段及混合动作。
func LoadCatalogRequest(path string) (CatalogRequest, error) {
	encoded, err := readFile(path, 16<<10, false)
	if err != nil {
		return CatalogRequest{}, err
	}
	request, err := decodeCatalogRequest(encoded)
	if err != nil || request.validate() != nil {
		return CatalogRequest{}, errors.New("节点目录部署操作无效")
	}
	return request, nil
}

func decodeCatalogRequest(encoded []byte) (CatalogRequest, error) {
	var request CatalogRequest
	if !utf8.Valid(encoded) {
		return request, nodecatalog.ErrInvalidInput
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return request, nodecatalog.ErrInvalidInput
	}
	fields := map[string]any{"action": &request.Action, "name": &request.Name, "capacity": &request.Capacity, "image_id": &request.ImageID, "image_reference": &request.ImageReference}
	for decoder.More() {
		key, err := decoder.Token()
		name, ok := key.(string)
		if err != nil || !ok || fields[name] == nil {
			return CatalogRequest{}, nodecatalog.ErrInvalidInput
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil || bytes.Equal(value, []byte("null")) || json.Unmarshal(value, fields[name]) != nil {
			return CatalogRequest{}, nodecatalog.ErrInvalidInput
		}
		delete(fields, name)
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') || decoder.Decode(new(any)) != io.EOF {
		return CatalogRequest{}, nodecatalog.ErrInvalidInput
	}
	if request.Action == "status" && (fields["name"] == nil || fields["capacity"] == nil || fields["image_reference"] == nil) {
		return CatalogRequest{}, nodecatalog.ErrInvalidInput
	}
	return request, nil
}

// RunCatalog 仅登记部署元数据，不连接 Docker、修改节点健康状态或启动账户供给。
// 提交结果未知时返回失败；查询原节点和镜像，或使用原登记重试，不生成替代资源身份。
func RunCatalog(ctx context.Context, configuration Config, request CatalogRequest) (result CatalogResult, failure error) {
	if err := configuration.validate(); err != nil {
		return result, err
	}
	if err := request.validate(); err != nil {
		return result, err
	}
	bounded, cancel := context.WithTimeout(ctx, configuration.HTTP.RequestTimeout.Value())
	defer cancel()
	roles := []postgres.Role{postgres.ControlRole, postgres.IdentityRole, postgres.TenantRole, postgres.DispatchRole}
	connections, err := configuration.Database.connections(roles)
	if err != nil {
		return result, err
	}
	database, err := configuration.Database.open(bounded, postgres.ControlRole, connections[0])
	if err != nil {
		return result, err
	}
	defer func() { failure = errors.Join(failure, database.Close()) }()
	service, err := nodecatalog.New(database, configuration.Node.ID)
	if err != nil {
		return result, err
	}
	result.Action, result.RequestID = request.Action, ids.New()
	if request.Action == "register" {
		result.Catalog, failure = service.Register(bounded, request.registration(), result.RequestID)
	} else {
		result.Catalog, failure = service.Get(bounded, request.ImageID)
	}
	return result, failure
}
