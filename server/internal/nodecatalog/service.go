// Package nodecatalog 管理节点目录、控制心跳与独立电脑验收；目录登记本身不授予就绪状态。
package nodecatalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/store/postgres"
)

var (
	ErrInvalidInput = errors.New("节点或镜像登记参数无效")
	ErrConflict     = errors.New("节点或镜像登记与已有目录冲突，请查询原记录")
	ErrNotFound     = errors.New("节点或镜像尚未登记")
)

type Registration struct {
	Name           string `json:"name"`
	Capacity       int    `json:"capacity"`
	ImageID        string `json:"image_id"`
	ImageReference string `json:"image_reference"`
}

// Node 是登记时的元数据快照；连接入口仍只来自部署配置，不保存或返回地址。
type Node struct {
	ID                  string     `json:"id"`
	Name                string     `json:"name"`
	Status              string     `json:"status"`
	Capacity            int        `json:"capacity"`
	Reserved            int        `json:"reserved"`
	Generation          int64      `json:"generation"`
	LastSeenAt          *time.Time `json:"last_seen_at,omitempty"`
	VerifiedImageID     *string    `json:"verified_image_id,omitempty"`
	CapabilityExpiresAt *time.Time `json:"capability_expires_at,omitempty"`
	VerificationStatus  string     `json:"verification_status,omitempty"`
	VerificationError   *string    `json:"verification_error,omitempty"`
}

// Image 的启用仅表示允许供给选用；下载、固定摘要核验及用户电脑能力仍由节点负责。
type Image struct {
	ID        string `json:"id"`
	Reference string `json:"reference"`
	IsEnabled bool   `json:"is_enabled"`
}

type Catalog struct {
	Node  Node  `json:"node"`
	Image Image `json:"image"`
}

type Service struct {
	control *postgres.Database
	nodeID  string
}

// New 固定本机部署节点与最小控制角色；不能作为无鉴权 HTTP 管理接口使用。
func New(control *postgres.Database, nodeID string) (*Service, error) {
	if control == nil || control.Role() != postgres.ControlRole || !validID(nodeID) {
		return nil, errors.New("节点目录连接用途或部署身份无效")
	}
	return &Service{control: control, nodeID: nodeID}, nil
}

func validID(value string) bool {
	id, err := ulid.ParseStrict(value)
	return err == nil && id.String() == value
}

// Validate 限定目录字段，节点实际容量和镜像可执行性不能由此推断。
func (registration Registration) Validate() error {
	if !validID(registration.ImageID) || computer.ValidateImageReference(registration.ImageReference) != nil || registration.Capacity < 1 || registration.Capacity > 10000 || !utf8.ValidString(registration.Name) || strings.TrimSpace(registration.Name) != registration.Name || utf8.RuneCountInString(registration.Name) < 1 || utf8.RuneCountInString(registration.Name) > 120 || strings.ContainsFunc(registration.Name, unicode.IsControl) {
		return ErrInvalidInput
	}
	return nil
}

// Register 原子登记本机节点和镜像并写审计；相同目录幂等，字段冲突不会覆盖既有状态。
// 节点首次登记固定为 bootstrapping，不修改 reserved、代次或健康证据，也不重新启用停用镜像。
func (service *Service) Register(ctx context.Context, registration Registration, requestID string) (Catalog, error) {
	if registration.Validate() != nil || !validID(requestID) {
		return Catalog{}, ErrInvalidInput
	}
	var catalog Catalog
	err := service.transaction(ctx, func(transaction *sql.Tx) error {
		// 首次并发写入也可能命中名称或引用唯一索引；先覆盖全部唯一冲突，再复验完整身份，不能吞掉业务冲突。
		node, err := transaction.ExecContext(ctx, `INSERT INTO tombot.computer_nodes(id,name,status,capacity) VALUES($1,$2,'bootstrapping',$3) ON CONFLICT DO NOTHING`, service.nodeID, registration.Name, registration.Capacity)
		if err != nil {
			return err
		}
		nodeCreated, err := node.RowsAffected()
		if err != nil {
			return err
		}
		image, err := transaction.ExecContext(ctx, `INSERT INTO tombot.computer_images(id,reference,is_enabled) VALUES($1,$2,true) ON CONFLICT DO NOTHING`, registration.ImageID, registration.ImageReference)
		if err != nil {
			return err
		}
		imageCreated, err := image.RowsAffected()
		if err != nil {
			return err
		}
		catalog, err = service.read(ctx, transaction, registration.ImageID)
		if errors.Is(err, ErrNotFound) {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		if catalog.Node.Name != registration.Name || catalog.Node.Capacity != registration.Capacity || catalog.Image.Reference != registration.ImageReference {
			return ErrConflict
		}
		if nodeCreated == 1 {
			if err = audit(ctx, transaction, requestID, "登记部署节点", "computer_node", service.nodeID, map[string]any{"capacity": registration.Capacity, "status": "bootstrapping"}); err != nil {
				return err
			}
		}
		if imageCreated == 1 {
			return audit(ctx, transaction, requestID, "登记电脑镜像", "computer_image", registration.ImageID, map[string]any{"is_enabled": true})
		}
		return nil
	})
	if err != nil {
		return Catalog{}, err
	}
	return catalog, nil
}

// Get 在同一数据库语句快照中查询固定节点与指定镜像；不存在明确失败，不补建记录。
func (service *Service) Get(ctx context.Context, imageID string) (Catalog, error) {
	if !validID(imageID) {
		return Catalog{}, ErrInvalidInput
	}
	var catalog Catalog
	err := service.transaction(ctx, func(transaction *sql.Tx) error {
		var err error
		catalog, err = service.read(ctx, transaction, imageID)
		return err
	})
	if err != nil {
		return Catalog{}, err
	}
	return catalog, nil
}

func (service *Service) read(ctx context.Context, transaction *sql.Tx, imageID string) (Catalog, error) {
	var catalog Catalog
	err := transaction.QueryRowContext(ctx, `SELECT n.id,n.name,tombot.computer_node_status(n.status,n.runtime_id,n.control_expires_at,n.capability_expires_at),n.capacity,n.reserved,n.generation,n.last_seen_at,i.id,i.reference,i.is_enabled,n.verified_image_id,n.capability_expires_at,COALESCE(v.state,''),v.error_code
		FROM tombot.computer_nodes n LEFT JOIN tombot.node_verifications v ON v.node_id=n.id CROSS JOIN tombot.computer_images i WHERE n.id=$1 AND i.id=$2`, service.nodeID, imageID).
		Scan(&catalog.Node.ID, &catalog.Node.Name, &catalog.Node.Status, &catalog.Node.Capacity, &catalog.Node.Reserved, &catalog.Node.Generation, &catalog.Node.LastSeenAt, &catalog.Image.ID, &catalog.Image.Reference, &catalog.Image.IsEnabled, &catalog.Node.VerifiedImageID, &catalog.Node.CapabilityExpiresAt, &catalog.Node.VerificationStatus, &catalog.Node.VerificationError)
	if errors.Is(err, sql.ErrNoRows) {
		return Catalog{}, ErrNotFound
	}
	return catalog, err
}

func audit(ctx context.Context, transaction *sql.Tx, requestID, action, kind, target string, changes map[string]any) error {
	encoded, err := json.Marshal(changes)
	if err != nil {
		return err
	}
	_, err = transaction.ExecContext(ctx, `INSERT INTO tombot.admin_audit_events(id,action,target_kind,target_id,request_id,result,changes) VALUES($1,$2,$3,$4,$5,'succeeded',$6)`, ids.New(), action, kind, target, requestID, encoded)
	return err
}

func (service *Service) transaction(ctx context.Context, apply func(*sql.Tx) error) error {
	err := service.control.Platform(ctx, apply)
	for _, known := range []error{ErrInvalidInput, ErrConflict, ErrNotFound, ErrStaleHeartbeat, ErrVerificationLease, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, known) {
			return known
		}
	}
	if err == nil {
		return nil
	}
	var state interface{ SQLState() string }
	if errors.As(err, &state) {
		if state.SQLState() == "23505" || state.SQLState() == "40001" || state.SQLState() == "40P01" {
			return ErrConflict
		}
		return fmt.Errorf("节点目录事务失败（SQLSTATE %s）；请查询原登记", state.SQLState())
	}
	return errors.New("节点目录事务未确认完成，请查询原登记")
}
