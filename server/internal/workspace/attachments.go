package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"
	"io.yueyang/tom/internal/domain"
)

// Attachment 校验已上传文件的归属、原始摘要和图像边界。所有检查针对同一个已打开的句柄。
func (f *Files) Attachment(ctx context.Context, botID string, input domain.AttachmentInput) (ref domain.ArtifactRef, result error) {
	parts := strings.Split(strings.TrimPrefix(input.Path, "shared/"), "/")
	if len(parts) != 3 || parts[0] != "inbox" || !validUploadName(parts[2]) || len(input.SHA256) != 64 {
		return ref, failure("ATTACHMENT_INVALID", "Use the path and SHA-256 returned by a completed inbox upload.")
	}
	if _, err := ulid.ParseStrict(parts[1]); err != nil {
		return ref, failure("ATTACHMENT_INVALID", "Invalid inbox upload identifier.")
	}
	if _, err := hex.DecodeString(input.SHA256); err != nil {
		return ref, failure("ATTACHMENT_INVALID", "Invalid attachment digest.")
	}
	// 同时限制图像解码与整文件哈希，避免并发上传占满堆内存或磁盘带宽。
	select {
	case f.attachmentSlots <- struct{}{}:
		defer func() { <-f.attachmentSlots }()
	case <-ctx.Done():
		return ref, ctx.Err()
	}
	file, err := f.Download(ctx, botID, input.Path)
	if err != nil {
		return ref, err
	}
	defer func() { result = errors.Join(result, file.File.Close()) }()
	if file.Size > MaxUploadBytes {
		return ref, failure("ATTACHMENT_TOO_LARGE", "The attachment exceeds the upload limit.")
	}
	hash := sha256.New()
	reader := io.TeeReader(io.LimitReader(contextReader{ctx, file.File}, MaxUploadBytes+1), hash)
	header := make([]byte, min(file.Size, 512))
	if _, err := io.ReadFull(reader, header); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return ref, domain.NewError(409, "ATTACHMENT_CHANGED", "The attachment changed while reading. Upload it again.")
		}
		return ref, err
	}
	mediaType := strings.SplitN(http.DetectContentType(header), ";", 2)[0]
	ref = domain.ArtifactRef{AttachmentInput: input, Bytes: int(file.Size), MediaType: mediaType}
	if !strings.HasPrefix(input.Path, "shared/") {
		ref.BotID = botID
	}
	if ref.IsImage() && file.Size > domain.MaxVisionImageBytes {
		return ref, failure("ATTACHMENT_IMAGE_LIMIT", "Images must fit within four MiB and four million pixels.")
	}
	var content bytes.Buffer
	var sink io.Writer = io.Discard
	if ref.IsImage() || file.Size <= domain.MaxInlineTextBytes {
		content.Write(header)
		sink = &content
	}
	n, err := io.Copy(sink, io.LimitReader(reader, file.Size-int64(len(header))+1))
	if err != nil {
		return ref, err
	}
	if n+int64(len(header)) != file.Size || hex.EncodeToString(hash.Sum(nil)) != input.SHA256 {
		return ref, domain.NewError(409, "ATTACHMENT_CHANGED", "The attachment changed. Upload the intended file again.")
	}
	if ref.IsImage() {
		config, _, err := image.DecodeConfig(bytes.NewReader(content.Bytes()))
		if err != nil || config.Width < 1 || config.Height < 1 || config.Width > 4096 || config.Height > 4096 || int64(config.Width)*int64(config.Height) > 4_000_000 {
			return ref, failure("ATTACHMENT_IMAGE_LIMIT", "The image header or dimensions are unsupported.")
		}
		if _, _, err = image.Decode(bytes.NewReader(content.Bytes())); err != nil {
			return ref, failure("ATTACHMENT_INVALID", "The image is incomplete or corrupt.")
		}
		ref.Width, ref.Height, ref.Content = config.Width, config.Height, content.Bytes()
	} else if file.Size <= domain.MaxInlineTextBytes && utf8.Valid(content.Bytes()) && !bytes.ContainsRune(content.Bytes(), 0) && strings.HasPrefix(mediaType, "text/") {
		ref.Content = content.Bytes()
	}
	return ref, ctx.Err()
}
