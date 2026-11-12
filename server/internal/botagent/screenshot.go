package botagent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"image/png"
	"regexp"

	"github.com/oklog/ulid/v2"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/domain"
)

var screenshotDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Validate 校验文字和服务端签发的截图引用；二进制内容仅在受信持久化及模型装配阶段存在。
func (result ToolResult) Validate(maximum int) error {
	if !textValid(result.Content, maximum) {
		return ErrProtocol
	}
	ref := result.Artifact
	if ref == nil {
		return nil
	}
	if result.IsError || ref.Path != "" || ref.Unavailable || ref.MediaType != "image/png" || ref.Bytes < 1 || ref.Bytes > domain.MaxVisionImageBytes || ref.Width < 320 || ref.Width > computer.DesktopWidth || ref.Height < 240 || ref.Height > computer.DesktopHeight || !screenshotDigest.MatchString(ref.SHA256) {
		return ErrProtocol
	}
	if _, err := ulid.ParseStrict(ref.ID); err != nil {
		return ErrProtocol
	}
	if _, err := ulid.ParseStrict(ref.BotID); err != nil {
		return ErrProtocol
	}
	if len(ref.Content) > 0 {
		return ValidateScreenshotPixels(ref)
	}
	return nil
}

// ValidateScreenshotPixels 验证有界视口 PNG 的内容和摘要；完整桌面仍由桌面接口要求 1280×800。
func ValidateScreenshotPixels(ref *domain.ArtifactRef) error {
	if ref == nil || len(ref.Content) != ref.Bytes || len(ref.Content) < 1 || len(ref.Content) > domain.MaxVisionImageBytes {
		return ErrProtocol
	}
	digest := sha256.Sum256(ref.Content)
	if hex.EncodeToString(digest[:]) != ref.SHA256 {
		return ErrProtocol
	}
	configuration, err := png.DecodeConfig(bytes.NewReader(ref.Content))
	if err != nil || configuration.Width != ref.Width || configuration.Height != ref.Height || ref.Width < 320 || ref.Width > computer.DesktopWidth || ref.Height < 240 || ref.Height > computer.DesktopHeight {
		return ErrProtocol
	}
	frame, err := png.Decode(bytes.NewReader(ref.Content))
	if err != nil || frame.Bounds().Dx() != ref.Width || frame.Bounds().Dy() != ref.Height {
		return ErrProtocol
	}
	return nil
}
