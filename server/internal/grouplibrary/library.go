// Package grouplibrary 管理群文件快照；调用方必须使用租户事务，写入前串行化同一用户的容量变更。
package grouplibrary

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"
	"io.yueyang/tom/internal/store/postgres"
)

const (
	MaxFileBytes = 16 << 20
	MaxUserBytes = 256 << 20
	MaxUserFiles = 1024
	PageSize     = 20
)

var (
	ErrInput    = errors.New("文件参数无效：名称不能包含路径或控制字符，单个文件最多 16 MB")
	ErrNotFound = errors.New("群组或文件不存在，或已被删除")
	ErrConflict = errors.New("文件已被修改，请刷新资料库后重试")
	ErrCapacity = errors.New("资料库存储已满：每位用户最多 256 MB、1024 个文件")
)

type File struct {
	ID        string    `json:"id"`
	GroupID   string    `json:"group_id"`
	Name      string    `json:"name"`
	MediaType string    `json:"media_type"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	Revision  int64     `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
}
type Page struct {
	Files        []File `json:"files"`
	Next         string `json:"next"`
	UsedBytes    int64  `json:"used_bytes"`
	MaxBytes     int64  `json:"max_bytes"`
	MaxFileBytes int64  `json:"max_file_bytes"`
}
type Upload struct {
	ClientRequestID string `json:"client_request_id"`
	Name            string `json:"name"`
	Content         []byte `json:"content_base64"`
}
type Content struct {
	File  File   `json:"file"`
	Bytes []byte `json:"content_base64"`
}

func validID(id string) bool { _, err := ulid.ParseStrict(id); return err == nil }
func validName(name string) bool {
	return utf8.ValidString(name) && len(name) > 0 && len(name) <= 240 && name == strings.TrimSpace(name) && name != "." && name != ".." && !strings.ContainsAny(name, "/\\") && !strings.ContainsFunc(name, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) })
}

// CheckGroup 对读取和写入应用相同的群归属及归档检查，避免通过已关闭群获取文件。
func CheckGroup(ctx context.Context, tx *sql.Tx, user, group string) error {
	if !validID(group) {
		return ErrInput
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.groups WHERE user_id=$1 AND id=$2 AND NOT is_archived)`, user, group).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	return nil
}

const columns = `id,group_id,name,media_type,size,sha256,revision,created_at`

type scanner interface{ Scan(...any) error }

func scan(row scanner) (File, error) {
	var file File
	err := row.Scan(&file.ID, &file.GroupID, &file.Name, &file.MediaType, &file.Size, &file.SHA256, &file.Revision, &file.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return File{}, ErrNotFound
	}
	return file, err
}

// List 只读取目录，游标按不可变标识排序；查询为字面子串，不解释通配符。
func List(ctx context.Context, tx *sql.Tx, user, group, before, query string) (Page, error) {
	page := Page{Files: []File{}, MaxBytes: MaxUserBytes, MaxFileBytes: MaxFileBytes}
	if before != "" && !validID(before) || !utf8.ValidString(query) || len(query) > 240 || strings.ContainsFunc(query, unicode.IsControl) {
		return page, ErrInput
	}
	if err := CheckGroup(ctx, tx, user, group); err != nil {
		return page, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(sum(size),0) FROM tombot.group_files WHERE user_id=$1 AND NOT is_deleted`, user).Scan(&page.UsedBytes); err != nil {
		return page, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+columns+` FROM tombot.group_files WHERE user_id=$1 AND group_id=$2 AND NOT is_deleted AND ($3='' OR id<$3) AND strpos(lower(name),lower($4))>0 ORDER BY id DESC LIMIT $5`, user, group, before, query, PageSize+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		file, err := scan(rows)
		if err != nil {
			return page, err
		}
		page.Files = append(page.Files, file)
	}
	if err = rows.Err(); err != nil {
		return page, err
	}
	if len(page.Files) > PageSize {
		page.Files = page.Files[:PageSize]
		page.Next = page.Files[PageSize-1].ID
	}
	return page, nil
}

// Read 校验群与文件绑定；正文单独存储，列表不会加载二进制。
func Read(ctx context.Context, tx *sql.Tx, user, group, id string) (File, error) {
	if !validID(id) {
		return File{}, ErrInput
	}
	if err := CheckGroup(ctx, tx, user, group); err != nil {
		return File{}, err
	}
	return scan(tx.QueryRowContext(ctx, `SELECT `+columns+` FROM tombot.group_files WHERE user_id=$1 AND group_id=$2 AND id=$3 AND NOT is_deleted`, user, group, id))
}

// Download 在调用方的一致性事务中读取有效文件，找不到文件时返回 ErrNotFound。
func Download(ctx context.Context, tx *sql.Tx, user, group, id string) (Content, error) {
	file, err := Read(ctx, tx, user, group, id)
	if err != nil {
		return Content{}, err
	}
	result := Content{File: file}
	err = tx.QueryRowContext(ctx, `SELECT content FROM tombot.group_file_contents WHERE user_id=$1 AND file_id=$2`, user, id).Scan(&result.Bytes)
	return result, err
}

// Save 以客户端标识去重；响应丢失后可以重试，改名和删除不会改变初次上传的摘要。
func Save(ctx context.Context, tx *sql.Tx, user, group string, upload Upload) (File, error) {
	if !validID(upload.ClientRequestID) || !validName(upload.Name) || upload.Content == nil || len(upload.Content) > MaxFileBytes {
		return File{}, ErrInput
	}
	if err := CheckGroup(ctx, tx, user, group); err != nil {
		return File{}, err
	}
	mediaType := http.DetectContentType(upload.Content)
	if mediaType == "image/png" || mediaType == "image/jpeg" || mediaType == "image/gif" {
		dimensions, _, err := image.DecodeConfig(bytes.NewReader(upload.Content))
		if err != nil || dimensions.Width > 8192 || dimensions.Height > 8192 || int64(dimensions.Width)*int64(dimensions.Height) > 20_000_000 {
			return File{}, ErrInput
		}
	}
	hash := sha256.Sum256(upload.Content)
	encoded, err := json.Marshal([]string{group, upload.Name, hex.EncodeToString(hash[:])})
	if err != nil {
		return File{}, err
	}
	digest := sha256.Sum256(encoded)
	var prior []byte
	var deleted bool
	err = tx.QueryRowContext(ctx, `SELECT creation_digest,is_deleted FROM tombot.group_files WHERE user_id=$1 AND id=$2`, user, upload.ClientRequestID).Scan(&prior, &deleted)
	if err == nil {
		if deleted || !bytes.Equal(prior, digest[:]) {
			return File{}, ErrConflict
		}
		return Read(ctx, tx, user, group, upload.ClientRequestID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return File{}, err
	}
	var used int64
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(sum(size),0),count(*) FROM tombot.group_files WHERE user_id=$1 AND NOT is_deleted`, user).Scan(&used, &count); err != nil {
		return File{}, err
	}
	if count >= MaxUserFiles || used+int64(len(upload.Content)) > MaxUserBytes {
		return File{}, ErrCapacity
	}
	file, err := scan(tx.QueryRowContext(ctx, `INSERT INTO tombot.group_files(user_id,id,group_id,name,media_type,size,sha256,creation_digest) VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING `+columns, user, upload.ClientRequestID, group, upload.Name, mediaType, len(upload.Content), hex.EncodeToString(hash[:]), digest[:]))
	if err != nil {
		return File{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO tombot.group_file_contents(user_id,file_id,content) VALUES($1,$2,$3)`, user, file.ID, upload.Content); err != nil {
		return File{}, err
	}
	return file, changed(ctx, tx, user, file)
}

// Rename 只改显示名，文件内容及历史消息中的附件快照保持不变。
func Rename(ctx context.Context, tx *sql.Tx, user, group, id string, revision int64, name string) (File, error) {
	if revision < 1 || !validName(name) {
		return File{}, ErrInput
	}
	file, err := Read(ctx, tx, user, group, id)
	if err != nil {
		return File{}, err
	}
	if file.Revision == revision+1 && file.Name == name {
		return file, nil
	}
	if file.Revision != revision {
		return File{}, ErrConflict
	}
	file, err = scan(tx.QueryRowContext(ctx, `UPDATE tombot.group_files SET name=$4,revision=revision+1,updated_at=clock_timestamp() WHERE user_id=$1 AND group_id=$2 AND id=$3 AND revision=$5 AND NOT is_deleted RETURNING `+columns, user, group, id, name, revision))
	if err != nil {
		return File{}, err
	}
	return file, changed(ctx, tx, user, file)
}

// Delete 原子释放正文及配额，保留墓碑以阻止旧上传重试复活文件。
func Delete(ctx context.Context, tx *sql.Tx, user, group, id string, revision int64) error {
	if revision < 1 || !validID(id) {
		return ErrInput
	}
	if err := CheckGroup(ctx, tx, user, group); err != nil {
		return err
	}
	var deleted bool
	var actual int64
	err := tx.QueryRowContext(ctx, `SELECT revision,is_deleted FROM tombot.group_files WHERE user_id=$1 AND group_id=$2 AND id=$3`, user, group, id).Scan(&actual, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if deleted && actual == revision+1 {
		return nil
	}
	if deleted || actual != revision {
		return ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM tombot.group_file_contents WHERE user_id=$1 AND file_id=$2`, user, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE tombot.group_files SET is_deleted=true,revision=revision+1,updated_at=clock_timestamp() WHERE user_id=$1 AND id=$2`, user, id); err != nil {
		return err
	}
	return changed(ctx, tx, user, File{ID: id, GroupID: group, Revision: revision + 1})
}
func changed(ctx context.Context, tx *sql.Tx, user string, file File) error {
	payload, err := json.Marshal(map[string]any{"group_id": file.GroupID, "file_id": file.ID, "revision": file.Revision})
	if err != nil {
		return err
	}
	_, err = postgres.AppendEvent(ctx, tx, user, "group.library.changed", file.GroupID, payload)
	return err
}
