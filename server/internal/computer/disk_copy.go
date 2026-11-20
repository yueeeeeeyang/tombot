package computer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

const DiskChunkBytes = 64 << 10
const ZeroChunkBytes = 4 << 20

// DiskCopy 绑定一次离线复制的双方规格，节点只允许操作由业务身份派生的持久盘。
type DiskCopy struct {
	EditID    string        `json:"edit_id"`
	Source    Specification `json:"source"`
	Target    Specification `json:"target"`
	Container string        `json:"container"`
	Action    string        `json:"action"`
	Offset    int64         `json:"offset"`
	Chunk     *DiskChunk    `json:"chunk,omitempty"`
}
type DiskChunk struct {
	Size   int    `json:"size"`
	Data   []byte `json:"data,omitempty"`
	SHA256 string `json:"sha256"`
}

func DiskDigest(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}
func (c DiskChunk) Valid(remaining int64) bool {
	if c.Size <= 0 || int64(c.Size) > remaining || c.Size > ZeroChunkBytes {
		return false
	}
	if len(c.Data) == 0 {
		return c.SHA256 == DiskDigest(make([]byte, c.Size))
	}
	return len(c.Data) == c.Size && c.Size <= DiskChunkBytes && DiskDigest(c.Data) == c.SHA256
}
func (q DiskCopy) NodeID() string {
	if q.Action == "read" || q.Action == "freeze" {
		return q.Source.NodeID
	}
	return q.Target.NodeID
}
func (q DiskCopy) Validate() error {
	s, t := q.Source, q.Target
	err := (NodeQuery{NodeID: q.EditID}).Validate()
	if err == nil && s.Validate() == nil && t.Validate() == nil && containerPattern.MatchString(q.Container) && s.UserID == t.UserID && s.ComputerID == t.ComputerID && s.VolumeReference == t.VolumeReference && s.OperationID == t.OperationID && t.Generation-s.Generation == 1 && t.Attempt-s.Attempt == 1 && t.FencingToken > s.FencingToken {
		switch q.Action {
		case "preflight", "freeze", "install":
			if q.Offset == 0 && q.Chunk == nil {
				return nil
			}
		case "read", "write":
			if q.Offset >= 0 && q.Offset < s.Resources.DiskBytes && ((q.Action == "read" && q.Chunk == nil) || (q.Action == "write" && q.Chunk != nil && q.Chunk.Valid(s.Resources.DiskBytes-q.Offset))) {
				return nil
			}
		}
	}
	return errors.New("离线磁盘复制身份或范围无效")
}

type DiskReceipt struct {
	EditID      string       `json:"edit_id"`
	Offset      int64        `json:"offset"`
	Chunk       *DiskChunk   `json:"chunk,omitempty"`
	Observation *Observation `json:"observation,omitempty"`
}

func (r DiskReceipt) Matches(q DiskCopy) bool {
	if r.EditID != q.EditID {
		return false
	}
	switch q.Action {
	case "read":
		return r.Observation == nil && r.Offset == q.Offset && r.Chunk != nil && r.Chunk.Valid(q.Source.Resources.DiskBytes-q.Offset)
	case "write":
		return r.Observation == nil && r.Chunk == nil && q.Chunk != nil && r.Offset == q.Offset+int64(q.Chunk.Size)
	case "install":
		return r.Offset == 0 && r.Chunk == nil && r.Observation != nil && r.Observation.Matches(q.Target) && r.Observation.State == "ready" && r.Observation.Capabilities.Ready() && r.Observation.ContainerID != q.Container
	case "preflight", "freeze":
		return r.Chunk == nil && r.Observation == nil && r.Offset == 0
	default:
		return false
	}
}

type DiskCopier interface {
	CopyDisk(context.Context, DiskCopy) (DiskReceipt, error)
}

// EditSupport 用于变更入队前的在线检查，避免旧节点在已停机后才拒绝新协议。
type EditSupport interface {
	CheckEditSupport(context.Context, NodeQuery) error
}
