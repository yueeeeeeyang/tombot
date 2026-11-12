package modelgateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"math/big"
	"strings"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/modelregistry"
	"io.yueyang/tom/internal/provider"
)

var _ modelregistry.Verifier = (*Gateway)(nil)

// Verify 以无用户内容的请求验证文本、工具结构及声明的视觉能力，不执行模型返回的工具。
// 探测可能产生供应商用量；每项只调用一次，任何失败或未知结果均阻止策略发布。
func (gateway *Gateway) Verify(parent context.Context, configuration modelregistry.Configuration, credential []byte) error {
	ctx, cancel := context.WithTimeout(parent, gateway.options.RequestTimeout)
	defer cancel()
	nonce := ids.New()
	input := Input{Messages: []provider.Message{{Role: "user", Blocks: []domain.MessageBlock{{Kind: "text", Text: "Return exactly this verification value, with no other text: " + nonce}}}}}
	reply, err := gateway.textProbe(ctx, configuration, credential, input)
	if err != nil {
		return err
	}
	if strings.TrimSpace(reply) != nonce {
		return ErrVerification
	}
	if configuration.SupportsTools {
		input.Messages[0].Blocks[0].Text = "Call verify_capability once with the nonce " + nonce + ". Do not call any other tool."
		input.Tools = []provider.ToolSchema{{Name: "verify_capability", Description: "Return a verification nonce. This tool has no external effects.", InputSchema: json.RawMessage(`{"type":"object","properties":{"nonce":{"type":"string"}},"required":["nonce"],"additionalProperties":false}`)}}
		input.ToolChoice = "required"
		count := 0
		_, err = gateway.Call(ctx, configuration, credential, input, func(event provider.Event) error {
			if event.Kind != "tool_use_end" {
				return nil
			}
			count++
			if count != 1 || event.ToolName != "verify_capability" || len(event.InputDelta) > 1024 {
				return ErrVerification
			}
			var result struct {
				Nonce string `json:"nonce"`
			}
			decoder := json.NewDecoder(strings.NewReader(event.InputDelta))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&result) != nil || result.Nonce != nonce {
				return ErrVerification
			}
			var trailing any
			if decoder.Decode(&trailing) != io.EOF {
				return ErrVerification
			}
			return nil
		})
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrVerification
		}
	}
	if configuration.SupportsVision {
		artifact, expected, err := visionChallenge()
		if err != nil {
			return err
		}
		input = Input{Messages: []provider.Message{{Role: "user", Blocks: []domain.MessageBlock{{Kind: "text", Text: "Read the six vertical color bands in the image from left to right. Reply only with their English color names separated by commas. Use red, green, blue, yellow, cyan, or magenta."}, {Kind: "artifact", Artifact: artifact}}}}}
		reply, err = gateway.textProbe(ctx, configuration, credential, input)
		if err != nil {
			return err
		}
		parts := strings.Split(strings.TrimSpace(reply), ",")
		for index := range parts {
			parts[index] = strings.ToLower(strings.TrimSpace(parts[index]))
		}
		if strings.Join(parts, ",") != expected {
			return ErrVerification
		}
	}
	return nil
}

func (gateway *Gateway) textProbe(ctx context.Context, configuration modelregistry.Configuration, credential []byte, input Input) (string, error) {
	var reply strings.Builder
	_, err := gateway.Call(ctx, configuration, credential, input, func(event provider.Event) error {
		if event.Kind == "tool_use_start" {
			return ErrVerification
		}
		if event.Kind == "text_delta" {
			if reply.Len()+len(event.Text) > 4096 {
				return ErrVerification
			}
			reply.WriteString(event.Text)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return reply.String(), nil
}

func visionChallenge() (*domain.ArtifactRef, string, error) {
	type band struct {
		name  string
		color color.RGBA
	}
	bands := []band{{"red", color.RGBA{255, 0, 0, 255}}, {"green", color.RGBA{0, 255, 0, 255}}, {"blue", color.RGBA{0, 0, 255, 255}}, {"yellow", color.RGBA{255, 255, 0, 255}}, {"cyan", color.RGBA{0, 255, 255, 255}}, {"magenta", color.RGBA{255, 0, 255, 255}}}
	for index := len(bands) - 1; index > 0; index-- {
		selected, err := rand.Int(rand.Reader, big.NewInt(int64(index+1)))
		if err != nil {
			return nil, "", ErrVerification
		}
		other := int(selected.Int64())
		bands[index], bands[other] = bands[other], bands[index]
	}
	canvas := image.NewRGBA(image.Rect(0, 0, 600, 200))
	names := make([]string, len(bands))
	for index, band := range bands {
		draw.Draw(canvas, image.Rect(index*100, 0, (index+1)*100, 200), image.NewUniform(band.color), image.Point{}, draw.Src)
		names[index] = band.name
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, canvas); err != nil {
		return nil, "", ErrVerification
	}
	return &domain.ArtifactRef{AttachmentInput: domain.AttachmentInput{Path: "capability.png"}, Bytes: encoded.Len(), MediaType: "image/png", Width: 600, Height: 200, Content: encoded.Bytes()}, strings.Join(names, ","), nil
}
