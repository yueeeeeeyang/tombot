package domain

// AttachmentInput 只接受收件目录路径和上传摘要。类型、大小和图像尺寸由服务端从文件确定。
type AttachmentInput struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// ArtifactRef 是消息、工具结果和转录中的文件标识。调用模型所需的内容仅在内存中装配。
type ArtifactRef struct {
	AttachmentInput
	ID          string `json:"id,omitempty"`
	BotID       string `json:"bot_id,omitempty"`
	Bytes       int    `json:"bytes"`
	MediaType   string `json:"media_type"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
	Content     []byte `json:"-"`
	Unavailable bool   `json:"-"`
}

const (
	MaxMessageAttachments = 4
	MaxVisionImageBytes   = 4 << 20
	MaxInlineTextBytes    = 64 << 10
)

// IsImage 只标记已校验的 PNG 和 JPEG，其他文件由工作区工具按需读取。
func (a ArtifactRef) IsImage() bool { return a.MediaType == "image/png" || a.MediaType == "image/jpeg" }

// VisionTokens 按图块给出本地保守估值，实际费用以供应商返回的用量优先。
func (a ArtifactRef) VisionTokens() int { return 256 + 2*((a.Width+31)/32)*((a.Height+31)/32) }
