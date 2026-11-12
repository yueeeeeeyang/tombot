//go:build linux

package taskprocess

import (
	"testing"
)

func TestKernelProbeSeal(t *testing.T) {
	request := probeRequest()
	file, err := SealProbe(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	})
	encoded, err := readSealedBytes(file)
	if err != nil {
		t.Fatal(err)
	}
	if actual, err := DecodeProbe(encoded); err != nil || actual != request {
		t.Fatal("真实封存探针授权不符", err)
	}
	if _, err := file.WriteAt([]byte("x"), 0); err == nil {
		t.Fatal("探针授权封存后仍可改写")
	}
	if _, err := readSealed(file); err == nil {
		t.Fatal("探针授权被当作 Bot 任务授权")
	}
	if _, err := CheckProbe(request); err == nil {
		t.Fatal("测试进程被误当成已入组探针")
	}
}
