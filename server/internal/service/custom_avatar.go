package service

import (
	"bytes"
	"encoding/base64"
	"image/png"
	"strconv"
	"strings"
)

const maxCustomAvatarLength = 96 * 1024

// normalizeCustomAvatar 只接受小尺寸 PNG 并重新编码，去掉元数据和尾随内容。
func normalizeCustomAvatar(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	const prefix = "data:image/png;base64,"
	if len(value) > maxCustomAvatarLength || !strings.HasPrefix(value, prefix) {
		return "", invalidWorkspace("自定义头像必须是有效的小尺寸 PNG 图片")
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(strings.TrimPrefix(value, prefix))
	if err != nil {
		return "", invalidWorkspace("自定义头像编码无效")
	}
	config, err := png.DecodeConfig(bytes.NewReader(raw))
	if err != nil || config.Width < 1 || config.Height < 1 || config.Width > 128 || config.Height > 128 {
		return "", invalidWorkspace("自定义头像最大为 128 × 128 像素")
	}
	picture, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return "", invalidWorkspace("无法读取自定义头像")
	}
	var encoded bytes.Buffer
	if err = png.Encode(&encoded, picture); err != nil {
		return "", err
	}
	result := prefix + base64.StdEncoding.EncodeToString(encoded.Bytes())
	if len(result) > maxCustomAvatarLength {
		return "", invalidWorkspace("自定义头像处理后过大")
	}
	return result, nil
}

// desktopSoulFor 只有用户创建的自定义资料进入初始设定；展示用 AI 介绍不回流。
func desktopSoulFor(agent Agent) string {
	if agent.CustomAvatar == "" || agent.Description == "" {
		return desktopSoul
	}
	profile := strconv.Quote(agent.Description)
	return desktopSoul + "\n\nInitial profile supplied by the user (quoted string): " + profile + "\nUse this as your initial role and communication preference, then refine your understanding through actual conversations and verified memories."
}
