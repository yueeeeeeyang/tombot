//go:build linux

package taskprocess

import (
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

const requiredSeals = unix.F_SEAL_SEAL | unix.F_SEAL_SHRINK | unix.F_SEAL_GROW | unix.F_SEAL_WRITE

// Seal 将经过校验的启动内容封存为匿名只读内容；调用者负责关闭返回的描述符。
// 封存防止检查之后内容被并发改写，不赋予持有者任何任务执行权限。
func Seal(request Request) (result *os.File, failure error) {
	encoded, err := encode(request)
	if err != nil {
		return nil, err
	}
	return sealBytes(encoded)
}

func sealBytes(encoded []byte) (result *os.File, failure error) {
	fd, err := unix.MemfdCreate("tombot-task", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, errors.New("创建任务封存内容失败")
	}
	file := os.NewFile(uintptr(fd), "task-request")
	defer func() {
		if result == nil {
			failure = errors.Join(failure, file.Close())
		}
	}()
	if count, err := file.Write(encoded); err != nil || count != len(encoded) {
		return nil, errors.New("写入任务封存内容失败")
	}
	if _, err = unix.FcntlInt(uintptr(fd), unix.F_ADD_SEALS, requiredSeals); err != nil {
		return nil, errors.New("封存任务内容失败")
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return nil, errors.New("定位任务封存内容失败")
	}
	return file, nil
}

func readSealed(file *os.File) (Request, error) {
	encoded, err := readSealedBytes(file)
	if err != nil {
		return Request{}, err
	}
	return decode(encoded)
}

func readSealedBytes(file *os.File) ([]byte, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 0 || stat.Size < 1 || stat.Size > maximumRequestBytes {
		return nil, errors.New("任务封存描述符无效")
	}
	seals, err := unix.FcntlInt(file.Fd(), unix.F_GET_SEALS, 0)
	if err != nil || seals&requiredSeals != requiredSeals {
		return nil, errors.New("任务内容尚未完整封存")
	}
	encoded := make([]byte, int(stat.Size))
	if count, err := file.ReadAt(encoded, 0); err != nil || count != len(encoded) {
		return nil, errors.New("读取任务封存内容失败")
	}
	return encoded, nil
}
