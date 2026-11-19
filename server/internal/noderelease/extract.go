package noderelease

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// Extract 只在调用者创建的空私有目录解包。仅六个固定普通文件可落盘，不采用归档的路径、权限或链接。
func Extract(ctx context.Context, source io.Reader, directory string) (failure error) {
	return walkArchive(ctx, source, func(h *tar.Header, entry io.Reader) error {
		mode := os.FileMode(0600)
		if h.Name == "tomnode" {
			mode = 0700
		}
		f, err := os.OpenFile(filepath.Join(directory, h.Name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		_, err = io.CopyN(f, entry, h.Size)
		return errors.Join(err, f.Sync(), f.Close())
	})
}

// walkArchive 用同一套文件名、大小和归档尾部约束检查上传流和节点解包流。
func walkArchive(ctx context.Context, source io.Reader, visit func(*tar.Header, io.Reader) error) (failure error) {
	gz, err := gzip.NewReader(source)
	if err != nil {
		return err
	}
	defer func() { failure = errors.Join(failure, gz.Close()) }()
	// 总量还须覆盖被 tar 解码器消费的扩展头和填充。
	bounded := &io.LimitedReader{R: archiveReader{ctx, gz}, N: MaxSize + 1}
	t := tar.NewReader(bounded)
	limits := map[string]int64{"tomnode": 256 << 20, "computer-image.tar": 16 << 30, "node-release.json": 64 << 10, "SHA256SUMS": 64 << 10, "node-install.example.json": 64 << 10, "安装说明.txt": 64 << 10}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		h, err := t.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg || h.Size < 1 || h.Size > limits[h.Name] {
			return errors.New("升级包包含重复、超限或不受支持的文件")
		}
		delete(limits, h.Name)
		if err := visit(h, t); err != nil {
			return err
		}
	}
	if len(limits) != 0 {
		return errors.New("升级包不完整")
	}
	// tar 末尾可能带标准零填充，读至 gzip 尾部以核实校验和。
	tail, err := io.ReadAll(io.LimitReader(bounded, 1<<16))
	if err != nil || len(tail) == 1<<16 || bounded.N == 0 {
		return errors.New("升级归档尾部无效")
	}
	for _, v := range tail {
		if v != 0 {
			return errors.New("升级归档尾部非空")
		}
	}
	return nil
}

type archiveReader struct {
	ctx    context.Context
	source io.Reader
}

func (r archiveReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.Read(b)
}
