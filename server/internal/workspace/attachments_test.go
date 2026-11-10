package workspace

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"io.yueyang/tom/internal/domain"
)

func TestAttachmentChecksDigestScopeImageBoundsAndCancel(t *testing.T) {
	files, root := fixture(t)
	ctx := t.Context()
	upload := func(name string, body []byte) domain.AttachmentInput {
		t.Helper()
		file, err := files.Upload(ctx, "first", name, bytes.NewReader(body), audited)
		if err != nil {
			t.Fatal(err)
		}
		return domain.AttachmentInput{Path: file.Path, SHA256: file.SHA256}
	}
	input := upload("材料.txt", []byte("指定文件"))
	ref, err := files.Attachment(ctx, "first", input)
	if err != nil || string(ref.Content) != "指定文件" || ref.BotID != "first" {
		t.Fatal("文本回读错误", err)
	}
	if _, err = files.Attachment(ctx, "second", input); err == nil {
		t.Fatal("附件跨 Bot 读取")
	}
	if err = os.WriteFile(filepath.Join(root, "bots", "first", input.Path), []byte("替换文件"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = files.Attachment(ctx, "first", input)
	requireCode(t, err, "ATTACHMENT_CHANGED")
	for _, path := range []string{"out/result.txt", "inbox/../file", "/inbox/arbitrary/file", "inbox/arbitrary/file", "shared/../secret"} {
		_, err = files.Attachment(ctx, "first", domain.AttachmentInput{Path: path, SHA256: input.SHA256})
		requireCode(t, err, "ATTACHMENT_INVALID")
	}
	var img bytes.Buffer
	if err = png.Encode(&img, image.NewGray(image.Rect(0, 0, 16, 8))); err != nil {
		t.Fatal(err)
	}
	valid := upload("错后缀.bin", img.Bytes())
	ref, err = files.Attachment(ctx, "first", valid)
	if err != nil || ref.MediaType != "image/png" || ref.Width != 16 || ref.Height != 8 || !bytes.Equal(img.Bytes(), ref.Content) {
		t.Fatal("类型或图片字节错误", err)
	}
	_, err = files.Attachment(ctx, "first", upload("截断.png", img.Bytes()[:33]))
	requireCode(t, err, "ATTACHMENT_INVALID")
	oversized := bytes.Clone(img.Bytes())
	binary.BigEndian.PutUint32(oversized[16:20], 4097)
	binary.BigEndian.PutUint32(oversized[29:33], crc32.ChecksumIEEE(oversized[12:29]))
	_, err = files.Attachment(ctx, "first", upload("超宽.png", oversized))
	requireCode(t, err, "ATTACHMENT_IMAGE_LIMIT")
	large := append(bytes.Clone(img.Bytes()), make([]byte, domain.MaxVisionImageBytes)...)
	_, err = files.Attachment(ctx, "first", upload("超大.png", large))
	requireCode(t, err, "ATTACHMENT_IMAGE_LIMIT")
	long := upload("长文.txt", []byte(strings.Repeat("x", domain.MaxInlineTextBytes+1)))
	ref, err = files.Attachment(ctx, "first", long)
	if err != nil || ref.Content != nil || ref.Bytes != domain.MaxInlineTextBytes+1 {
		t.Fatal("长文件应保留引用", err)
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if _, err = files.Attachment(canceled, "first", valid); !errors.Is(err, context.Canceled) {
		t.Fatal("取消未生效", err)
	}
}
