//go:build linux

package computeragent

import (
	"io"
	"strings"
	"testing"
)

func TestPreparationOutputIsBoundedThroughCopy(t *testing.T) {
	output := &preparationOutput{}
	if _, err := io.Copy(output, strings.NewReader(strings.Repeat("x", 2048))); err != nil || output.buffer.Len() != 2048 {
		t.Fatal("合法诊断输出无法完整保留", err)
	}
	if _, err := io.Copy(output, strings.NewReader("x")); err == nil || output.buffer.Len() != 2048 {
		t.Fatal("初始化诊断突破内容限制")
	}
}
