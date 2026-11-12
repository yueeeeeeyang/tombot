package botagent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"net"
	"testing"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/provider"
)

func screenshotResult(t *testing.T) ToolResult {
	t.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 1280, 800))); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded.Bytes())
	return ToolResult{Content: "Desktop captured.", Artifact: &domain.ArtifactRef{ID: ids.New(), BotID: ids.New(), AttachmentInput: domain.AttachmentInput{SHA256: hex.EncodeToString(digest[:])}, MediaType: "image/png", Bytes: encoded.Len(), Width: 1280, Height: 800, Content: encoded.Bytes()}}
}

func TestDispatchRejectsChangedScreenshotReference(t *testing.T) {
	for _, field := range []string{"id", "bot", "digest", "omitted"} {
		t.Run(field, func(t *testing.T) {
			parent, child := net.Pipe()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			result := screenshotResult(t)
			result.Artifact.Content = nil
			done := make(chan error, 1)
			go func() {
				err := func() error {
					start, err := ReadFrame(child)
					if err != nil {
						return err
					}
					input := start.Start.Input
					if err := WriteFrame(child, Frame{Version: ProtocolVersion, Sequence: 1, Kind: "model", Key: "model.1", Input: &input}); err != nil {
						return err
					}
					response, err := ReadFrame(child)
					if err != nil {
						return err
					}
					output := *response.Model
					call := output.Tools[0]
					if err := WriteFrame(child, Frame{Version: ProtocolVersion, Sequence: 2, Kind: "tool", Key: "tool.1.1", Call: &call}); err != nil {
						return err
					}
					response, err = ReadFrame(child)
					if err != nil {
						return err
					}
					altered := *response.Tool
					switch field {
					case "id":
						altered.Artifact.ID = ids.New()
					case "bot":
						altered.Artifact.BotID = ids.New()
					case "digest":
						altered.Artifact.SHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
					case "omitted":
						altered.Artifact = nil
					}
					input.Messages = append(input.Messages, assistantMessage(output), provider.Message{Role: "user", Blocks: []domain.MessageBlock{toolBlock(call, altered)}})
					return WriteFrame(child, Frame{Version: ProtocolVersion, Sequence: 3, Kind: "model", Key: "model.2", Input: &input})
				}()
				done <- errors.Join(err, child.Close())
			}()
			models := 0
			relay := testRelay{model: func(context.Context, string, modelgateway.Input) (ModelOutput, error) {
				models++
				return toolOutput(), nil
			}, tool: func(context.Context, string, ToolCall) (ToolResult, error) { return result, nil }}
			if _, err := DispatchStream(ctx, parent, fixtureStart(), relay); !errors.Is(err, ErrProtocol) || models != 1 {
				t.Fatal("篡改图片引用仍进入模型", models, err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestScreenshotReferenceExcludesPixelsFromFrames(t *testing.T) {
	result := screenshotResult(t)
	if err := result.Validate(1024); err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := WriteFrame(&encoded, Frame{Version: ProtocolVersion, Sequence: 1, Kind: "result", Tool: &result}); err != nil {
		t.Fatal(err)
	}
	if encoded.Len() > 1024 {
		t.Fatal("图片像素进入 Agent 控制帧")
	}
	frame, err := ReadFrame(&encoded)
	if err != nil || frame.Tool.Artifact == nil || frame.Tool.Artifact.ID != result.Artifact.ID || len(frame.Tool.Artifact.Content) != 0 || frame.Tool.Validate(1024) != nil {
		t.Fatal("截图引用跨进程改变", err)
	}
	block := toolBlock(ToolCall{ID: "capture"}, *frame.Tool)
	if block.ToolResult.Artifact.ID != result.Artifact.ID {
		t.Fatal("模型上下文遗漏截图引用")
	}
	jsonPixels, err := json.Marshal(result)
	if err != nil || bytes.Contains(jsonPixels, []byte(`"Content"`)) {
		t.Fatal("截图内容进入持久 JSON", err)
	}
}
func TestScreenshotReferenceRejectsInvalidPayload(t *testing.T) {
	original := screenshotResult(t)
	for _, mutate := range []func(*ToolResult){func(r *ToolResult) { r.IsError = true }, func(r *ToolResult) { r.Artifact.ID = "" }, func(r *ToolResult) { r.Artifact.Path = "/etc/private" }, func(r *ToolResult) { r.Artifact.BotID = "" }, func(r *ToolResult) { r.Artifact.Width++ }, func(r *ToolResult) { r.Artifact.SHA256 = "bad" }, func(r *ToolResult) { r.Artifact.Bytes++ }, func(r *ToolResult) { r.Artifact.Content = []byte("not PNG") }, func(r *ToolResult) { r.Artifact.Unavailable = true }} {
		value := original
		ref := *original.Artifact
		value.Artifact = &ref
		mutate(&value)
		if value.Validate(1024) == nil {
			t.Fatal("接受无效截图引用")
		}
	}
}
