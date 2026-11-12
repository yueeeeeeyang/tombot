//go:build linux

package taskprocess

import "testing"

func TestKernelBrowserSeal(t *testing.T) {
	request := browserRequest()
	file, err := SealBrowser(request)
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
	if actual, err := DecodeBrowser(encoded); err != nil || actual != request {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("x"), 0); err == nil {
		t.Fatal("已封存浏览器授权仍可修改")
	}
	if _, err := readSealed(file); err == nil {
		t.Fatal("浏览器授权被用于 Bot 任务")
	}
	if err := checkBrowserPipes(file, file); err == nil {
		t.Fatal("文件被当作浏览器私有管道")
	}
}
