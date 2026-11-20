package identity

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

func userAvatarPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewNRGBA(image.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func TestNormalizeUserAvatar(t *testing.T) {
	original := userAvatarPNG(t, maxUserAvatarDimension, maxUserAvatarDimension)
	normalized, err := NormalizeUserAvatar(append(append([]byte{}, original...), []byte("移除尾随内容")...))
	if err != nil || !bytes.Equal(normalized, original) {
		t.Fatal("PNG 未规范化", err)
	}
	var jpegImage bytes.Buffer
	if err := jpeg.Encode(&jpegImage, image.NewRGBA(image.Rect(0, 0, 24, 24)), nil); err != nil {
		t.Fatal(err)
	}
	normalized, err = NormalizeUserAvatar(jpegImage.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := png.Decode(bytes.NewReader(normalized)); err != nil {
		t.Fatal("JPEG 未转换为 PNG", err)
	}
	for _, invalid := range [][]byte{nil, {}, []byte("<svg/>"), original[:len(original)/2], make([]byte, MaxUserAvatarBytes+1), userAvatarPNG(t, 513, 1), userAvatarPNG(t, 1, 513)} {
		_, err := NormalizeUserAvatar(invalid)
		requireError(t, err, ErrInvalidAvatar)
	}
}

func testUserProfile(t *testing.T, fixture identityFixture) {
	ctx, service := context.Background(), fixture.service
	save := func(ctx context.Context, principal Principal, name string, avatar []byte, requestID string) error {
		_, err := service.UpdateProfile(ctx, principal, ProfileUpdate{DisplayName: &name, Avatar: avatar}, requestID)
		return err
	}
	owner, other := fixture.active(t), fixture.active(t)
	login := func(account testAccount) Session {
		request := loginRequest(account)
		request.Scope, request.Platform = "web", "web"
		session, err := service.Login(ctx, request, attempt())
		if err != nil {
			t.Fatal(err)
		}
		return session
	}
	session, foreign := login(owner), login(other)
	desktop := fixture.login(t, owner)
	avatar := userAvatarPNG(t, 64, 64)
	if err := save(ctx, session.Principal, "  新昵称  ", avatar, attempt().RequestID); err != nil {
		t.Fatal(err)
	}
	principal, err := service.Authenticate(ctx, string(session.AccessToken), "web")
	if err != nil || principal.DisplayName != "新昵称" || principal.Username != owner.username {
		t.Fatal("资料没有持久保存", err)
	}
	stored, err := service.UserAvatar(ctx, session.Principal)
	if err != nil || !bytes.Equal(stored, avatar) {
		t.Fatal("头像没有保存", err)
	}
	stored, err = service.UserAvatar(ctx, desktop.Principal)
	if err != nil || !bytes.Equal(stored, avatar) {
		t.Fatal("桌面会话未读取到本人头像", err)
	}
	forgedDesktop := desktop.Principal
	forgedDesktop.UserID = other.id
	_, err = service.UserAvatar(ctx, forgedDesktop)
	requireError(t, err, ErrInvalidSession)
	for _, invalid := range []string{"", "  ", strings.Repeat("字", 121), "昵称\n换行", "昵称\x00", string([]byte{0xff})} {
		requireError(t, save(ctx, session.Principal, invalid, nil, attempt().RequestID), ErrInvalidInput)
	}
	requireError(t, save(ctx, session.Principal, "不应保存", []byte("bad"), attempt().RequestID), ErrInvalidAvatar)
	principal, err = service.Authenticate(ctx, string(session.AccessToken), "web")
	if err != nil || principal.DisplayName != "新昵称" {
		t.Fatal("头像失败却更新了昵称", err)
	}
	if err := save(ctx, session.Principal, strings.Repeat("字", 120), nil, attempt().RequestID); err != nil {
		t.Fatal(err)
	}
	stored, err = service.UserAvatar(ctx, session.Principal)
	if err != nil || !bytes.Equal(stored, avatar) {
		t.Fatal("只改昵称丢失头像", err)
	}
	profile, err := service.UpdateProfile(ctx, session.Principal, ProfileUpdate{Avatar: avatar}, attempt().RequestID)
	if err != nil || profile.DisplayName != strings.Repeat("字", 120) {
		t.Fatal("只更换头像覆盖了最新昵称", err)
	}
	_, err = service.UpdateProfile(ctx, session.Principal, ProfileUpdate{}, attempt().RequestID)
	requireError(t, err, ErrInvalidInput)
	// 两种局部更新同时提交，也不能互相覆盖另一字段。
	avatar = userAvatarPNG(t, 96, 96)
	concurrentName := "并发保存昵称"
	results := make(chan error, 2)
	for _, update := range []ProfileUpdate{{DisplayName: &concurrentName}, {Avatar: avatar}} {
		go func(update ProfileUpdate) {
			_, err := service.UpdateProfile(ctx, session.Principal, update, attempt().RequestID)
			results <- err
		}(update)
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	principal, err = service.Authenticate(ctx, string(session.AccessToken), "web")
	if err != nil || principal.DisplayName != concurrentName {
		t.Fatal("并发头像更新覆盖昵称", err)
	}
	stored, err = service.UserAvatar(ctx, session.Principal)
	if err != nil || !bytes.Equal(stored, avatar) {
		t.Fatal("并发昵称更新覆盖头像", err)
	}
	stored, err = service.UserAvatar(ctx, foreign.Principal)
	if err != nil || stored != nil {
		t.Fatal("读取到其他用户头像", err)
	}
	forged := session.Principal
	forged.UserID = other.id
	requireError(t, save(ctx, forged, "越权", nil, attempt().RequestID), ErrInvalidSession)
	_, err = service.UserAvatar(ctx, forged)
	requireError(t, err, ErrInvalidSession)
	forged = session.Principal
	forged.Scope = "admin"
	requireError(t, save(ctx, forged, "管理员", nil, attempt().RequestID), ErrInvalidSession)
	_, err = service.UserAvatar(ctx, forged)
	requireError(t, err, ErrInvalidSession)
	next := randomValue(t)
	requireError(t, service.ChangePassword(ctx, session.Principal, randomValue(t), next, "", attempt()), ErrInvalidCredentials)
	if err := service.ChangePassword(ctx, session.Principal, owner.password, next, "", attempt()); err != nil {
		t.Fatal(err)
	}
	for _, current := range []Session{session, desktop} {
		_, err := service.Authenticate(ctx, string(current.AccessToken), current.Principal.Scope)
		requireError(t, err, ErrInvalidSession)
		_, err = service.UserAvatar(ctx, current.Principal)
		requireError(t, err, ErrInvalidSession)
	}
	requireError(t, save(ctx, session.Principal, "撤销后写入", nil, attempt().RequestID), ErrInvalidSession)
	_, err = service.UserAvatar(ctx, session.Principal)
	requireError(t, err, ErrInvalidSession)
	owner.password = next
	restored := login(owner)
	stored, err = service.UserAvatar(ctx, restored.Principal)
	if err != nil || !bytes.Equal(stored, avatar) {
		t.Fatal("重新登录后头像丢失", err)
	}
}
