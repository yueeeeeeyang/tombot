package nodefleet

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/noderelease"
)

// withUpload 按管理员串行保护暂存文件。浏览器重试可续传同一文件，每个账号最多保留一个未完成包。
func (s Service) withUpload(owner string, action func(string) error) (failure error) {
	if (computer.NodeQuery{NodeID: owner}).Validate() != nil {
		return ErrInput
	}
	dir := filepath.Join(s.Directory, ".upload-"+owner)
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	lock, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { failure = errors.Join(failure, lock.Close()) }()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); errors.Is(err, syscall.EWOULDBLOCK) {
		return ErrUploadBusy
	} else if err != nil {
		return err
	}
	return action(dir)
}

type uploadProgress struct {
	Next int64 `json:"next"`
}

func (s Service) begin(owner, artifact string, size int64) (progress uploadProgress, err error) {
	if !noderelease.ValidDigest(artifact) || size <= 0 || size > noderelease.MaxSize {
		return progress, ErrInput
	}
	name := artifact + "." + strconv.FormatInt(size, 10)
	err = s.withUpload(owner, func(dir string) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.Name() != name {
				if err = os.Remove(filepath.Join(dir, e.Name())); err != nil {
					return err
				}
				continue
			}
			info, err := e.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() || info.Size() > size {
				return ErrConflict
			}
			progress.Next = info.Size() / noderelease.ChunkSize
			if info.Size() == size {
				progress.Next = (size + noderelease.ChunkSize - 1) / noderelease.ChunkSize
			}
			return nil
		}
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		return f.Close()
	})
	return progress, err
}

func activeUpload(dir, artifact string) (string, int64, error) {
	if !noderelease.ValidDigest(artifact) {
		return "", 0, ErrInput
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", 0, err
	}
	if len(entries) != 1 {
		return "", 0, ErrConflict
	}
	id, length, ok := strings.Cut(entries[0].Name(), ".")
	size, err := strconv.ParseInt(length, 10, 64)
	if !ok || id != artifact || err != nil || size <= 0 || size > noderelease.MaxSize {
		return "", 0, ErrConflict
	}
	return filepath.Join(dir, entries[0].Name()), size, nil
}

func (s Service) savePart(owner, artifact, ordinal, digest string, part []byte) error {
	index, err := strconv.ParseInt(ordinal, 10, 32)
	if err != nil || index < 0 || !noderelease.ValidDigest(digest) || noderelease.Digest(part) != digest {
		return ErrInput
	}
	return s.withUpload(owner, func(dir string) (failure error) {
		path, size, err := activeUpload(dir, artifact)
		if err != nil {
			return err
		}
		offset := index * noderelease.ChunkSize
		if offset >= size || int64(len(part)) != min(size-offset, noderelease.ChunkSize) {
			return ErrInput
		}
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		defer func() { failure = errors.Join(failure, f.Close()) }()
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if info.Size() < offset {
			return ErrConflict
		}
		if info.Size() >= offset+int64(len(part)) {
			existing := make([]byte, len(part))
			if _, err = f.ReadAt(existing, offset); err != nil {
				return err
			}
			if !bytes.Equal(existing, part) {
				return ErrConflict
			}
			return nil
		}
		if _, err = f.WriteAt(part, offset); err != nil {
			return err
		}
		return f.Sync()
	})
}

// verifyUpload 在登记前实际读取归档的所有文件及压缩分片。任何缺件、损坏或架构不符都不能进入版本列表。
func verifyUpload(ctx context.Context, path, artifact string, size int64) (m noderelease.Manifest, failure error) {
	f, err := os.Open(path)
	if err != nil {
		return m, err
	}
	defer func() { failure = errors.Join(failure, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return m, err
	}
	if info.Size() != size {
		return m, ErrConflict
	}
	m, err = noderelease.Inspect(ctx, f)
	if err != nil {
		return m, err
	}
	if _, err = f.Seek(0, 0); err != nil {
		return m, err
	}
	m.Size = size
	if m.Label == "" {
		m.Label = "sha256-" + artifact[:12]
	}
	part := make([]byte, noderelease.ChunkSize)
	for remaining := size; remaining > 0; remaining -= noderelease.ChunkSize {
		if ctx.Err() != nil {
			return m, ctx.Err()
		}
		b := part[:min(remaining, noderelease.ChunkSize)]
		if _, err = io.ReadFull(f, b); err != nil {
			return m, err
		}
		m.Chunks = append(m.Chunks, noderelease.Digest(b))
	}
	return m, m.Validate(artifact)
}

func (s Service) publish(ctx context.Context, owner, artifact string) (r Release, err error) {
	if !noderelease.ValidDigest(artifact) {
		return r, ErrInput
	}
	err = s.withUpload(owner, func(dir string) error {
		path, size, err := activeUpload(dir, artifact)
		if errors.Is(err, ErrConflict) {
			// 最后一次响应丢失时可直接重取已提交的版本。
			return s.DB.Platform(ctx, func(tx *sql.Tx) error {
				return tx.QueryRowContext(ctx, `SELECT r.id,r.label,r.platform FROM tombot.node_releases r JOIN tombot.node_release_archives a USING(artifact) WHERE artifact=$1 ORDER BY created_at DESC,id LIMIT 1`, artifact).Scan(&r.ID, &r.Label, &r.Platform)
			})
		}
		if err != nil {
			return err
		}
		m, err := verifyUpload(ctx, path, artifact, size)
		if err != nil {
			if ctx.Err() == nil && errors.Is(err, noderelease.ErrPackage) {
				return errors.Join(err, os.Remove(path))
			}
			return err
		}
		encoded, err := json.Marshal(m)
		if err != nil {
			return err
		}
		return s.DB.Platform(ctx, func(tx *sql.Tx) error {
			r = Release{artifact, m.Label, m.Platform}
			err := tx.QueryRowContext(ctx, `SELECT id,label,platform FROM tombot.node_releases WHERE artifact=$1 ORDER BY created_at DESC,id LIMIT 1`, artifact).Scan(&r.ID, &r.Label, &r.Platform)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if r.Platform != m.Platform {
				return noderelease.ErrPackage
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO tombot.node_releases(id,label,platform,artifact) VALUES($1,$2,$3,$4) ON CONFLICT(id) DO NOTHING`, r.ID, r.Label, r.Platform, artifact); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO tombot.node_release_archives(artifact,manifest) VALUES($1,$2) ON CONFLICT(artifact) DO NOTHING`, artifact, encoded); err != nil {
				return err
			}
			if err = os.Rename(path, s.archivePath(artifact)); err != nil {
				return err
			}
			parent, err := os.Open(s.Directory)
			if err != nil {
				return err
			}
			return errors.Join(parent.Sync(), parent.Close())
		})
	})
	return r, err
}

func (s Service) archivePath(artifact string) string {
	return filepath.Join(s.Directory, artifact+".tar.gz")
}
