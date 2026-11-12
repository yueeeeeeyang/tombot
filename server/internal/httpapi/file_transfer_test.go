package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"strings"
	"testing"

	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/testkit"
	"io.yueyang/tom/internal/workspace"
)

func uploadBody(t *testing.T, filename string, content []byte, extra bool) ([]byte, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": filename}))
	part, err := writer.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = part.Write(content); err != nil {
		t.Fatal(err)
	}
	if extra {
		if err = writer.WriteField("unexpected", "extra"); err != nil {
			t.Fatal(err)
		}
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes(), writer.FormDataContentType()
}

func TestHTTPFileUploadDownloadRangesAndRawPreview(t *testing.T) {
	server, setup := fixture(t, true)
	password := testkit.Random(t)
	if err := server.Auth.Setup(password, setup); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	session, err := server.Auth.Login(ctx, password, "文件传输测试", "macos", nil)
	if err != nil {
		t.Fatal(err)
	}
	bot, err := server.Bots.Create(ctx, service.BotInput{Template: "writer"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	headers := map[string]string{"Authorization": "Bearer " + session.DeviceToken}
	// 共享上传必须显式选择目的地，拒绝模糊或重复参数，避免将私有文件发入共享目录。
	sharedBody, sharedType := uploadBody(t, "共享资料.txt", []byte("共享资料正文"), false)
	for _, query := range []string{"", "scope=bot", "scope=shared&scope=shared", "scope=shared&bot_id=" + bot.ID, "bot_id=" + bot.ID + "&extra=1", "scope=shared"} {
		req := httptest.NewRequest("POST", "/api/v1/fs/upload?"+query, bytes.NewReader(sharedBody))
		req.Header.Set("Content-Type", sharedType)
		req.Header.Set("Authorization", headers["Authorization"])
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if query != "scope=shared" {
			if response.Code != 400 {
				t.Fatal("未拒绝无效上传范围", query, response.Code)
			}
			continue
		}
		var published workspace.WriteResult
		if err = json.Unmarshal(response.Body.Bytes(), &published); err != nil || response.Code != 201 || !strings.HasPrefix(published.Path, "shared/inbox/") {
			t.Fatal("共享上传未发布到指定目录", response.Code, err)
		}
		download := url.Values{"scope": {"shared"}, "path": {published.Path}}
		saved := request(t, handler, "GET", "/api/v1/fs/download?"+download.Encode(), nil, headers)
		if saved.Code != 200 || saved.Body.String() != "共享资料正文" {
			t.Fatal("无法取回共享上传", saved.Code)
		}
	}
	upload := func(filename string, content []byte, extra, truncated, authorized bool) *httptest.ResponseRecorder {
		body, contentType := uploadBody(t, filename, content, extra)
		if truncated {
			body = body[:len(body)-8]
		}
		req := httptest.NewRequest("POST", "/api/v1/fs/upload?bot_id="+bot.ID, bytes.NewReader(body))
		req.Header.Set("Content-Type", contentType)
		if authorized {
			req.Header.Set("Authorization", headers["Authorization"])
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	if response := upload("report.txt", []byte("报告"), false, false, false); response.Code != 401 {
		t.Fatal("未鉴权上传被接受", response.Code)
	}
	for _, mode := range []string{"truncated", "extra", "path", "backslash"} {
		filename := "report.bin"
		if mode == "path" {
			filename = "../escape.bin"
		}
		if mode == "backslash" {
			filename = "dir\\escape.bin"
		}
		if response := upload(filename, []byte{0, 1, 2}, mode == "extra", mode == "truncated", true); response.Code != 400 {
			t.Fatal("无效上传被接受", mode, response.Code)
		}
	}
	body, contentType := uploadBody(t, "encoded.txt", []byte("=41=42=43"), false)
	body = bytes.Replace(body, []byte("Content-Disposition:"), []byte("Content-Transfer-Encoding: quoted-printable\r\nContent-Disposition:"), 1)
	encodedRequest := httptest.NewRequest("POST", "/api/v1/fs/upload?bot_id="+bot.ID, bytes.NewReader(body))
	encodedRequest.Header.Set("Content-Type", contentType)
	encodedRequest.Header.Set("Authorization", headers["Authorization"])
	encodedResponse := httptest.NewRecorder()
	handler.ServeHTTP(encodedResponse, encodedRequest)
	if encodedResponse.Code != 400 {
		t.Fatal("传输编码上传被隐式解码接受", encodedResponse.Code)
	}
	listing, err := server.Files.Tree(ctx, bot.ID, "bot", "inbox", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Entries) != 0 {
		t.Fatal("无效 multipart 已产生文件", listing)
	}
	content := []byte{0, 1, 2, 255, 128, 10}
	response := upload("中文.bin", content, false, false, true)
	if response.Code != 201 {
		t.Fatal("上传失败", response.Code, response.Body.String())
	}
	var uploaded workspace.WriteResult
	if err = json.Unmarshal(response.Body.Bytes(), &uploaded); err != nil {
		t.Fatal(err)
	}
	query := url.Values{"bot_id": {bot.ID}, "path": {uploaded.Path}}.Encode()
	response = request(t, handler, "GET", "/api/v1/fs/download?"+query, nil, headers)
	if response.Code != 200 || !bytes.Equal(response.Body.Bytes(), content) || response.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatal("下载内容不完整", response.Code)
	}
	disposition, parameters, err := mime.ParseMediaType(response.Header().Get("Content-Disposition"))
	if err != nil || disposition != "attachment" || parameters["filename"] != "中文.bin" {
		t.Fatal("下载文件名错误", err)
	}
	ranged := map[string]string{"Authorization": headers["Authorization"], "Range": "bytes=1-3"}
	response = request(t, handler, "GET", "/api/v1/fs/download?"+query, nil, ranged)
	if response.Code != 206 || !bytes.Equal(response.Body.Bytes(), content[1:4]) || response.Header().Get("Content-Range") != "bytes 1-3/6" {
		t.Fatal("范围下载错误", response.Code)
	}
	ranged["Range"] = "bytes=99-100"
	if response = request(t, handler, "GET", "/api/v1/fs/download?"+query, nil, ranged); response.Code != 416 {
		t.Fatal("无效下载范围未拒绝", response.Code)
	}
	if response = request(t, handler, "HEAD", "/api/v1/fs/download?"+query, nil, headers); response.Code != 200 || response.Body.Len() != 0 || response.Header().Get("Content-Length") != "6" {
		t.Fatal("下载元数据错误", response.Code)
	}
	if response = request(t, handler, "GET", "/api/v1/fs/download?"+query, nil, nil); response.Code != 401 {
		t.Fatal("下载未鉴权")
	}
	if response = request(t, handler, "GET", "/api/v1/fs/file?"+query, nil, headers); response.Code != 400 {
		t.Fatal("二进制进入文本预览")
	}
	original := "# 标题\n\t正文\n"
	response = request(t, handler, "PUT", "/api/v1/fs/file", service.FileWriteInput{BotID: bot.ID, Path: "out/report.md", Content: original}, headers)
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	response = request(t, handler, "GET", "/api/v1/fs/file?bot_id="+bot.ID+"&path=out/report.md", nil, headers)
	var preview workspace.TextFile
	if err = json.Unmarshal(response.Body.Bytes(), &preview); err != nil || preview.Content != original {
		t.Fatal("文本原文被行号改写", err)
	}
	response = request(t, handler, "PUT", "/api/v1/fs/file", service.FileWriteInput{BotID: bot.ID, Path: "out/report.md", Content: "修改后", ExpectedSHA256: &preview.SHA256}, headers)
	if response.Code != 200 {
		t.Fatal("条件更新失败", response.Code)
	}
	response = request(t, handler, "PUT", "/api/v1/fs/file", service.FileWriteInput{BotID: bot.ID, Path: "out/report.md", Content: "过期覆盖", ExpectedSHA256: &preview.SHA256}, headers)
	if response.Code != 409 {
		t.Fatal("并发编辑冲突未返回", response.Code)
	}
	escaped := strings.Repeat("\n", workspace.MaxTextBytes)
	response = request(t, handler, "PUT", "/api/v1/fs/file", service.FileWriteInput{BotID: bot.ID, Path: "out/large.txt", Content: escaped}, headers)
	if response.Code != 200 {
		t.Fatal("合法文本被 JSON 转义开销拒绝", response.Code)
	}
	response = request(t, handler, "PUT", "/api/v1/fs/file", service.FileWriteInput{BotID: bot.ID, Path: "out/large.txt", Content: escaped + "x"}, headers)
	if response.Code != 413 {
		t.Fatal("超限文本未返回 413", response.Code)
	}
	response = upload("large.txt", []byte(escaped+"x"), false, false, true)
	if response.Code != 201 {
		t.Fatal("二进制上传错误受文本容量限制", response.Code)
	}
	if err = json.Unmarshal(response.Body.Bytes(), &uploaded); err != nil {
		t.Fatal(err)
	}
	query = url.Values{"bot_id": {bot.ID}, "path": {uploaded.Path}}.Encode()
	if response = request(t, handler, "GET", "/api/v1/fs/file?"+query, nil, headers); response.Code != 413 {
		t.Fatal("超限文本预览未返回 413", response.Code)
	}
}

func TestAbortedDownloadDoesNotAppendJSON(t *testing.T) {
	server, _ := fixture(t, true)
	handler := server.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		if _, err := io.WriteString(w, "partial"); err != nil {
			t.Error(err)
		}
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}))
	upstream := httptest.NewServer(handler)
	defer upstream.Close()
	response, err := upstream.Client().Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	content, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if !errors.Is(readErr, io.ErrUnexpectedEOF) || closeErr != nil || string(content) != "partial" {
		t.Fatal("下载中断被改写为成功 JSON", string(content), readErr, closeErr)
	}
}
