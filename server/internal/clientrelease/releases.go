// Package clientrelease 管理桌面安装包的发布目录，数据库只登记完整且不可覆盖的版本。
package clientrelease

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/noderelease"
	"io.yueyang/tom/internal/store/postgres"
)

const Directory = "/var/lib/tombot-client-releases"
const chunkSize = 4 << 20
const maxSize = 1 << 30
const maxNotes = 4000

var platforms = map[string][]string{
	"darwin-arm64": {".dmg", ".pkg", ".zip"}, "darwin-x64": {".dmg", ".pkg", ".zip"},
	"win32-arm64": {".exe", ".msi", ".zip"}, "win32-x64": {".exe", ".msi", ".zip"},
	"linux-arm64": {".AppImage", ".deb", ".rpm", ".tar.gz", ".zip"}, "linux-x64": {".AppImage", ".deb", ".rpm", ".tar.gz", ".zip"},
}
var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})$`)
var errInput = errors.New("请检查平台、三段式版本号、升级内容和安装包格式")
var errIncomplete = errors.New("安装包分片缺失或大小不符，请重新上传")
var errDuplicate = errors.New("该平台的版本号已发布，请使用新的版本号")

type Service struct {
	DB        *postgres.Database
	Directory string
}

type details struct {
	Platform  string `json:"platform"`
	Version   string `json:"version"`
	Notes     string `json:"notes"`
	Extension string `json:"extension"`
	Size      int64  `json:"size"`
}
type manifest struct {
	details
	Chunks []string `json:"chunks"`
}
type release struct {
	ID string `json:"id"`
	manifest
}

// versionNumber 按三个数值段排序，每段占六位，结果可安全存入 bigint。
func versionNumber(version string) (int64, error) {
	segments := versionPattern.FindStringSubmatch(version)
	if segments == nil {
		return 0, errInput
	}
	var number int64
	for _, segment := range segments[1:] {
		value, err := strconv.ParseInt(segment, 10, 32)
		if err != nil {
			return 0, errInput
		}
		number = number*1_000_000 + value
	}
	return number, nil
}

// savePart 把已验证内容原子落盘，相同摘要可跨重试和发布复用。
func (s Service) savePart(digest string, part []byte) (failure error) {
	if len(part) == 0 || len(part) > chunkSize || noderelease.Digest(part) != digest {
		return errInput
	}
	file, err := os.CreateTemp(s.Directory, ".upload-")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.Remove(file.Name()); !errors.Is(err, os.ErrNotExist) {
			failure = errors.Join(failure, err)
		}
	}()
	_, err = file.Write(part)
	if err = errors.Join(err, file.Sync(), file.Close()); err != nil {
		return err
	}
	return os.Rename(file.Name(), filepath.Join(s.Directory, digest))
}

// publish 在短事务外检查分片，数据库唯一约束仲裁同平台同版本的并发发布。
func (s Service) publish(ctx context.Context, m manifest, actor, requestID string) (string, error) {
	number, err := versionNumber(m.Version)
	m.Notes = strings.TrimSpace(m.Notes)
	if err != nil || !slices.Contains(platforms[m.Platform], m.Extension) || m.Notes == "" || utf8.RuneCountInString(m.Notes) > maxNotes || m.Size <= 0 || m.Size > maxSize || int64(len(m.Chunks)) != (m.Size+chunkSize-1)/chunkSize {
		return "", errInput
	}
	for i, digest := range m.Chunks {
		if !noderelease.ValidDigest(digest) {
			return "", errInput
		}
		stat, err := os.Lstat(filepath.Join(s.Directory, digest))
		if errors.Is(err, os.ErrNotExist) {
			return "", errIncomplete
		}
		if err != nil {
			return "", err
		}
		if !stat.Mode().IsRegular() || stat.Size() != min(chunkSize, m.Size-int64(i)*chunkSize) {
			return "", errIncomplete
		}
	}
	dir, err := os.Open(s.Directory)
	if err != nil {
		return "", err
	}
	if err = errors.Join(dir.Sync(), dir.Close()); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	id := ids.New()
	err = s.DB.Platform(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `INSERT INTO tombot.client_releases(id,platform,version_number,manifest) VALUES($1,$2,$3,$4) ON CONFLICT(platform,version_number) DO NOTHING RETURNING id`, id, m.Platform, number, encoded).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return errDuplicate
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO tombot.admin_audit_events(id,actor_id,action,target_kind,target_id,request_id,result) VALUES($1,$2,'发布客户端升级','client_release',$3,$4,'succeeded')`, ids.New(), actor, id, requestID)
		return err
	})
	return id, err
}

// latest 只查本平台严格更新的版本，初次发布前及已是最新版均返回空结果。
func (s Service) latest(ctx context.Context, platform, installed string) (*release, error) {
	number, err := versionNumber(installed)
	if err != nil || platforms[platform] == nil {
		return nil, errInput
	}
	var r release
	var encoded []byte
	err = s.DB.Platform(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT id,manifest FROM tombot.client_releases WHERE platform=$1 AND version_number>$2 ORDER BY version_number DESC LIMIT 1`, platform, number).Scan(&r.ID, &encoded)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(encoded, &r.manifest); err != nil {
		return nil, err
	}
	return &r, nil
}

// list 返回每个平台目前会推送的版本，排除分片清单以限制管理响应大小。
func (s Service) list(ctx context.Context) ([]details, error) {
	result := []details{}
	err := s.DB.Platform(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT latest.manifest-'chunks' FROM unnest($1::text[]) p(platform)
            CROSS JOIN LATERAL (SELECT manifest FROM tombot.client_releases WHERE platform=p.platform
              ORDER BY version_number DESC LIMIT 1) latest ORDER BY p.platform`, slices.Sorted(maps.Keys(platforms)))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var encoded []byte
			var d details
			if err = rows.Scan(&encoded); err != nil {
				return err
			}
			if err = json.Unmarshal(encoded, &d); err != nil {
				return err
			}
			result = append(result, d)
		}
		return rows.Err()
	})
	return result, err
}
