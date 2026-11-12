//go:build linux

package computeragent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"
)

func TestManagedAgentInputReceiptAndUnknownWrite(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := errors.Join(reader.Close(), writer.Close()); err != nil {
			t.Error(err)
		}
	}()
	agent := &managedAgent{input: writer}
	ctx := context.Background()
	if offset, err := agent.write(ctx, 0, []byte("abc")); err != nil || offset != 3 {
		t.Fatal(offset, err)
	}
	if offset, err := agent.write(ctx, 0, []byte("abc")); err != nil || offset != 3 {
		t.Fatal("相同写入未复用回执", offset, err)
	}
	if _, err := agent.write(ctx, 0, []byte("xyz")); err == nil {
		t.Fatal("同一偏移替换了输入内容")
	}
	if _, err := agent.write(ctx, 5, []byte("x")); err == nil {
		t.Fatal("接受输入跳跃")
	}
	buffer := make([]byte, 3)
	if _, err := io.ReadFull(reader, buffer); err != nil || string(buffer) != "abc" {
		t.Fatal(string(buffer), err)
	}
	if err := reader.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if count, err := reader.Read(buffer); count != 0 || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("重复输入真正执行了两次", count, err)
	}
	// 填满没有消费者的管道；任一部分写入或超时后都拒绝后续输入。
	blocked, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	for range 64 {
		offset, err := agent.write(blocked, agent.writeOffset, bytes.Repeat([]byte("x"), 32<<10))
		if err != nil {
			break
		}
		if offset == 0 {
			t.Fatal("空写入回执")
		}
	}
	if !agent.writeFailed {
		t.Fatal("阻塞输入没有保留未知状态")
	}
	if _, err := agent.write(ctx, agent.writeOffset, []byte("later")); err == nil {
		t.Fatal("未知输入后继续写入")
	}
}

func TestManagedAgentOutputReplayAndDrain(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	}()
	done := make(chan struct{})
	close(done)
	agent := &managedAgent{output: reader, done: done, exitCode: 0}
	if _, err := writer.Write([]byte("abcdef")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	first, err := agent.read(context.Background(), 0)
	if err != nil || string(first.Bytes) != "abcdef" || first.Offset != 6 {
		t.Fatal(first, err)
	}
	repeat, err := agent.read(context.Background(), 0)
	if err != nil || !bytes.Equal(first.Bytes, repeat.Bytes) {
		t.Fatal("未确认输出块不能复查", err)
	}
	if _, err := agent.read(context.Background(), 3); err == nil {
		t.Fatal("接受了输出中间偏移")
	}
	last, err := agent.read(context.Background(), 6)
	if err != nil || !last.Exited || last.ExitCode != 0 || len(last.Bytes) != 0 {
		t.Fatal("未确认尾部排空与退出", last, err)
	}
}

func TestAgentStreamFailureStillKillsScope(t *testing.T) {
	input, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}
	kill, err := os.CreateTemp(t.TempDir(), "scope-kill-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := kill.Close(); err != nil {
			t.Error(err)
		}
	}()
	scope := &kernelScope{files: []*os.File{nil, nil, kill}, agent: &agentAttempt{agent: &managedAgent{input: input, output: output}}}
	if err := scope.Revoke(); err == nil {
		t.Fatal("关闭失败被忽略")
	}
	var marked [1]byte
	if _, err := kill.ReadAt(marked[:], 0); err != nil || marked[0] != '1' {
		t.Fatal("流关闭失败阻止了整组终止", err)
	}
}
