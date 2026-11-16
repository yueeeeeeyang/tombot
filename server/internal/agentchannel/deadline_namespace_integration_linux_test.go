//go:build linux && timensintegration

package agentchannel

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

type clockReply struct {
	PID         int    `json:"pid"`
	Namespace   uint64 `json:"namespace"`
	Accepted    bool   `json:"accepted"`
	RemainingNS int64  `json:"remaining_ns"`
}

// TestKernelTimeNamespaceDeadline 使用真实零、正、负偏移的独立命名空间，不能用模拟时钟代替。
func TestKernelTimeNamespaceDeadline(t *testing.T) {
	if os.Getenv("TOM_TEST_TIME_NAMESPACES") != "1" {
		t.Fatal("时间命名空间集成测试须在专用 Linux 测试容器显式启用")
	}
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, offset := range []int{0, 172800, -1} {
		t.Run(strconv.Itoa(offset), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			// 只为本次子进程创建命名空间，宿主和其他容器的时钟保持不变。
			command := exec.CommandContext(ctx, "unshare", "--time", "--fork", "--monotonic", strconv.Itoa(offset), "--", program, "-test.run=^TestTimeNamespaceDeadlineChild$")
			command.Env = append(os.Environ(), "TOM_TEST_TIME_CHILD=1")
			command.Stderr = os.Stderr
			input, err := command.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			output, err := command.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := input.Close(); err != nil {
					t.Error(err)
				}
				if err := command.Wait(); err != nil {
					t.Error(err)
				}
			}()
			decoder, encoder := json.NewDecoder(output), json.NewEncoder(input)
			var started clockReply
			if err := decoder.Decode(&started); err != nil {
				t.Fatal("读取独立命名空间身份失败", err)
			}
			original := time.Now().Add(3 * time.Second)
			host, err := FromDeadline(original)
			if err != nil || host.TimeNamespace == started.Namespace {
				t.Fatal("未创建不同的实际时间命名空间", err)
			}
			check := func(stamp Deadline, accepted bool) {
				t.Helper()
				if err := encoder.Encode(stamp); err != nil {
					t.Fatal(err)
				}
				var reply clockReply
				if err := decoder.Decode(&reply); err != nil || reply.Accepted != accepted {
					t.Fatal("真实跨命名空间期限校验不符合预期", reply, err)
				}
				if accepted && (reply.RemainingNS <= 0 || reply.RemainingNS > int64(3*time.Second)) {
					t.Fatal("转换后期限被延长", reply)
				}
			}
			check(host, false)
			converted, err := FromDeadlineForProcess(original, started.PID)
			if err != nil || converted.TimeNamespace != started.Namespace {
				t.Fatal("未绑定实际接收进程的时钟", converted, err)
			}
			check(converted, true)
			source, err := openProcessClock(os.Getpid())
			if err != nil {
				t.Fatal(err)
			}
			sourceClock, readErr := readProcessClock(source)
			closeErr := source.Close()
			if readErr != nil || closeErr != nil {
				t.Fatal(readErr, closeErr)
			}
			back, err := translateDeadline(converted, int64(offset)*int64(time.Second), sourceClock)
			if err != nil {
				t.Fatal(err)
			}
			local, err := back.Local(5 * time.Second)
			if err != nil || local.After(original) {
				t.Fatal("换算延长了原始授权期限", local, original, err)
			}
			late, err := FromDeadlineForProcess(time.Now().Add(50*time.Millisecond), started.PID)
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(80 * time.Millisecond)
			check(late, false)
			forged := converted
			forged.TimeNamespace++
			check(forged, false)
			forged = converted
			forged.ExpiresNS += int64(time.Hour)
			check(forged, false)
		})
	}
}

// TestTimeNamespaceDeadlineChild 是独立命名空间中的固定接收端，只校验期限，不执行业务动作。
func TestTimeNamespaceDeadlineChild(t *testing.T) {
	if os.Getenv("TOM_TEST_TIME_CHILD") != "1" {
		return
	}
	_, namespace, err := monotonicClock()
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(os.Stdout)
	if err := encoder.Encode(clockReply{PID: os.Getpid(), Namespace: namespace}); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewScanner(os.Stdin)
	for reader.Scan() {
		var stamp Deadline
		if err := json.Unmarshal(reader.Bytes(), &stamp); err != nil {
			t.Fatal(err)
		}
		deadline, err := stamp.Local(5 * time.Second)
		reply := clockReply{Accepted: err == nil}
		if err == nil {
			reply.RemainingNS = int64(time.Until(deadline))
		}
		if err := encoder.Encode(reply); err != nil {
			t.Fatal(err)
		}
	}
	if err := reader.Err(); err != nil {
		t.Fatal(fmt.Errorf("读取固定期限测试请求失败：%w", err))
	}
}
