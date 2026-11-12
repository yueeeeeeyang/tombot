package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/png"
	"strings"
	"testing"
)

func pngAvatar(t *testing.T, width int) string {
	t.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewNRGBA(image.Rect(0, 0, width, 128))); err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(encoded.Bytes())
}

func TestCustomAvatarAndInitialProfilePersist(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	workspace := SeedWorkspace()
	cat := &workspace.Agents[0]
	cat.CustomAvatar = pngAvatar(t, 128)
	cat.Description = "帮助整理园艺笔记，语气温柔。"
	saved, err := s.Save(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Workspace.Agents[0].CustomAvatar != saved.Workspace.Agents[0].CustomAvatar || loaded.Workspace.Agents[0].Description != cat.Description {
		t.Fatal("自定义资料未保存")
	}
	var soul, avatar string
	if err := s.db.QueryRow("SELECT soul,custom_avatar FROM bots WHERE desktop_id=?", cat.ID).Scan(&soul, &avatar); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(soul, cat.Description) || avatar != cat.CustomAvatar {
		t.Fatal("初始介绍未进入自己的角色设定")
	}
	if strings.Contains(desktopSoulFor(Agent{Description: "旧的展示介绍"}), "旧的展示介绍") {
		t.Fatal("展示介绍不应反向影响工作")
	}
	if _, err := ParseWorkspace(mustJSON(t, loaded.Workspace)); err != nil {
		t.Fatal("保存后无法重新读取", err)
	}
}

func TestCustomAvatarRejectsMalformedAndOversizedImages(t *testing.T) {
	for _, avatar := range []string{"https://example.test/a.png", "data:image/svg+xml;base64,abcd", "data:image/png;base64,broken", pngAvatar(t, 129), strings.Repeat("a", maxCustomAvatarLength+1)} {
		if _, err := normalizeCustomAvatar(avatar); err == nil {
			t.Fatal("错误头像被接受")
		}
	}
	valid := pngAvatar(t, 128)
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(valid, "data:image/png;base64,"))
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := normalizeCustomAvatar("data:image/png;base64," + base64.StdEncoding.EncodeToString(append(raw, []byte("trailing metadata")...)))
	if err != nil || canonical != valid {
		t.Fatal("未清除尾随内容", err)
	}
}

func TestUserMessageLimits(t *testing.T) {
	for _, content := range []string{"", "  ", strings.Repeat("a", 10001), strings.Repeat("🐈", 5001)} {
		if err := ValidateUserMessage(content); err == nil {
			t.Fatal("无效内容被接受")
		}
	}
	if err := ValidateUserMessage(strings.Repeat("🐈", 5000)); err != nil {
		t.Fatal(err)
	}
}
