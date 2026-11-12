//go:build linux

package computeragent

import (
	"os"
	"os/exec"

	"io.yueyang/tom/internal/taskprocess"
)

// prepareTask 固定唯一的受信引导入口，返回的封存文件由调用者在 Start 后关闭。
// 标准流和终端属性由具体运行器设置，用户参数不能覆盖根引导的路径、目录或环境。
func prepareTask(request taskprocess.Request, membership, revocation *os.File) (*exec.Cmd, *os.File, error) {
	sealed, err := taskprocess.Seal(request)
	if err != nil {
		return nil, nil, err
	}
	command := exec.Command("/usr/lib/tombot/computer-task")
	command.Dir = "/"
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	command.ExtraFiles = []*os.File{sealed, membership, revocation}
	return command, sealed, nil
}
