//go:build linux

package computeragent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/processgroup"
	"io.yueyang/tom/internal/taskprocess"
)

func (scope *kernelScope) TransferFile(ctx context.Context, reference processgroup.Reference, request computer.FileRequest, membership, revocation *os.File) (computer.FileReceipt, error) {
	scope.mu.Lock()
	valid := !scope.closed && !scope.revoked && !scope.drained && scope.directory != nil && scope.directory.session == request.Session
	scope.mu.Unlock()
	if !valid || reference.Validate() != nil || reference.Session != request.Session {
		return computer.FileReceipt{}, errors.New("电脑文件传输授权无效")
	}
	return scope.transfers.transfer(ctx, request, func(ctx context.Context, action, relative string, size int, digest string, content []byte) ([]byte, error) {
		arguments := []string{"-I", "-B", "/usr/lib/tombot/files.py", action, relative}
		if action == "write" {
			arguments = append(arguments, strconv.Itoa(size), digest)
		}
		command, sealed, err := prepareTask(taskprocess.Request{Version: 2, OperationID: request.OperationID, Group: reference, Role: "shell", ProcessCommand: computer.ProcessCommand{Program: "/usr/bin/python3", Arguments: arguments, WorkingDirectory: "/workspace"}}, membership, revocation)
		if err != nil {
			return nil, err
		}
		output := &boundedFileOutput{}
		command.Stdin, command.Stdout, command.Stderr = bytes.NewReader(content), output, io.Discard
		command.WaitDelay = time.Second
		if err = ctx.Err(); err == nil {
			err = command.Start()
		}
		closeError := sealed.Close()
		if err != nil {
			return nil, errors.Join(errors.New("电脑文件程序启动失败"), closeError)
		}
		killed := make(chan error, 1)
		stop := context.AfterFunc(ctx, func() { killed <- command.Process.Kill() })
		waitError := command.Wait()
		if !stop() {
			if killError := <-killed; killError != nil && !errors.Is(killError, os.ErrProcessDone) {
				waitError = errors.Join(waitError, killError)
			}
		}
		if waitError != nil || closeError != nil || ctx.Err() != nil {
			clear(output.Bytes())
			return nil, errors.New("电脑文件程序未确认完成")
		}
		return output.Bytes(), nil
	})
}

type boundedFileOutput struct{ buffer bytes.Buffer }

func (output *boundedFileOutput) Bytes() []byte { return output.buffer.Bytes() }

func (output *boundedFileOutput) Write(content []byte) (int, error) {
	if len(content) > computer.MaximumFileBytes-output.buffer.Len() {
		return 0, errors.New("电脑文件输出超过限制")
	}
	return output.buffer.Write(content)
}
