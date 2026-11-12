package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/workspace"
)

const fileTransferTimeout = 2 * time.Minute
const multipartOverhead = 64 << 10

// uploadReader 只有在整份 multipart 校验完成后才暴露 EOF，防止尾部坏数据在文件发布后才被发现。
type uploadReader struct {
	part  *multipart.Part
	parts *multipart.Reader
}

func (reader uploadReader) Read(buffer []byte) (int, error) {
	n, err := reader.part.Read(buffer)
	if err == io.EOF {
		next, nextErr := reader.parts.NextRawPart()
		if next != nil {
			closeErr := next.Close()
			return n, errors.Join(domain.NewError(400, "INVALID_UPLOAD", "Provide exactly one file part named file."), closeErr)
		}
		if nextErr != io.EOF {
			return n, domain.NewError(400, "INVALID_UPLOAD", "The multipart upload ended unexpectedly.")
		}
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return n, domain.NewError(413, "UPLOAD_TOO_LARGE", "The upload exceeds the allowed size.")
	}
	if err != nil && err != io.EOF {
		return n, domain.NewError(400, "INVALID_UPLOAD", "The multipart upload ended unexpectedly.")
	}
	return n, err
}

func (s *Server) uploadFile(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	botInbox := len(query["bot_id"]) == 1 && query.Get("bot_id") != ""
	sharedInbox := len(query["scope"]) == 1 && query.Get("scope") == "shared"
	if len(query) != 1 || !(botInbox || sharedInbox) {
		s.fail(w, r, domain.NewError(400, "INVALID_INPUT", "Choose exactly one upload destination: a bot_id or scope=shared."))
		return
	}
	controller := http.NewResponseController(w)
	if err := controller.SetReadDeadline(time.Now().Add(fileTransferTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		s.fail(w, r, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), fileTransferTimeout)
	defer cancel()
	r.Body = http.MaxBytesReader(w, r.Body, workspace.MaxUploadBytes+multipartOverhead)
	parts, err := r.MultipartReader()
	if err != nil {
		s.fail(w, r, domain.NewError(400, "INVALID_UPLOAD", "Provide a multipart file upload."))
		return
	}
	// 保留传输编码头，避免 multipart 隐式解码后绕过原始文件校验。
	part, err := parts.NextRawPart()
	if err != nil {
		s.fail(w, r, domain.NewError(400, "INVALID_UPLOAD", "Provide one file part named file."))
		return
	}
	disposition, parameters, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
	if err != nil || disposition != "form-data" || parameters["name"] != "file" || parameters["filename"] == "" || part.Header.Get("Content-Transfer-Encoding") != "" {
		s.fail(w, r, domain.NewError(400, "INVALID_UPLOAD", "Provide one unencoded file part named file."))
		return
	}
	// 使用原始 filename 参数，不使用会去掉路径前缀的 Part.FileName，避免掩盖路径攻击。
	result, err := s.Files.Upload(ctx, query.Get("bot_id"), parameters["filename"], uploadReader{part, parts}, actor(r))
	if err != nil {
		if result.Path != "" {
			s.Logger.Error("上传文件已发布，但收尾失败", "bot_id", query.Get("bot_id"), "request_id", r.Context().Value(requestKey))
		}
		s.fail(w, r, err)
		return
	}
	s.respond(w, r, 201, result)
}

type downloadReader struct {
	io.ReadSeeker
	ctx     context.Context
	failure error
}

func (reader *downloadReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		reader.failure = err
		return 0, err
	}
	n, err := reader.ReadSeeker.Read(buffer)
	if err != nil && err != io.EOF {
		reader.failure = err
	}
	return n, err
}
func (s *Server) downloadFile(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	ctx, cancel := context.WithTimeout(r.Context(), fileTransferTimeout)
	defer cancel()
	file, err := s.Files.Download(ctx, query.Get("bot_id"), query.Get("scope"), query.Get("path"), actor(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer func() {
		if err := file.File.Close(); err != nil {
			s.Logger.Error("下载文件句柄关闭失败", "request_id", r.Context().Value(requestKey))
		}
	}()
	controller := http.NewResponseController(w)
	if err = controller.SetWriteDeadline(time.Now().Add(fileTransferTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": file.Name}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("ETag", fmt.Sprintf(`W/"%x-%x"`, file.Size, file.ModifiedAt.UnixNano()))
	reader := &downloadReader{ReadSeeker: file.File, ctx: ctx}
	http.ServeContent(w, r, file.Name, file.ModifiedAt, reader)
	if reader.failure != nil && r.Context().Err() == nil {
		s.Logger.Warn("文件下载中断", "request_id", r.Context().Value(requestKey))
		panic(http.ErrAbortHandler)
	}
}
