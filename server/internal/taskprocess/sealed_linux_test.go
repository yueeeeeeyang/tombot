//go:build linux

package taskprocess

import (
	"os"
	"reflect"
	"testing"

	"golang.org/x/sys/unix"
)

func TestKernelSealedRequest(t *testing.T) {
	request := validRequest()
	file, err := Seal(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	})
	actual, err := readSealed(file)
	if err != nil || !reflect.DeepEqual(actual, request) {
		t.Fatalf("内核封存内容未往返：%v", err)
	}
	if _, err := file.WriteAt([]byte("x"), 0); err == nil {
		t.Fatal("封存后仍可改写")
	}
	if err := file.Truncate(0); err == nil {
		t.Fatal("封存后仍可截断")
	}
	if err := file.Truncate(maximumRequestBytes); err == nil {
		t.Fatal("封存后仍可扩展")
	}
	if mapped, err := unix.Mmap(int(file.Fd()), 0, 4096, unix.PROT_WRITE, unix.MAP_SHARED); err == nil {
		if err := unix.Munmap(mapped); err != nil {
			t.Error(err)
		}
		t.Fatal("封存后仍可共享写映射")
	}

	fd, err := unix.MemfdCreate("unsealed-test", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		t.Fatal(err)
	}
	unsealed := os.NewFile(uintptr(fd), "unsealed-test")
	t.Cleanup(func() {
		if err := unsealed.Close(); err != nil {
			t.Error(err)
		}
	})
	encoded, err := encode(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = unsealed.Write(encoded); err != nil {
		t.Fatal(err)
	}
	if _, err := readSealed(unsealed); err == nil {
		t.Fatal("可改写内容被接受")
	}
	ordinary, err := os.CreateTemp(t.TempDir(), "ordinary")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ordinary.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err = ordinary.Write(encoded); err != nil {
		t.Fatal(err)
	}
	if _, err := readSealed(ordinary); err == nil {
		t.Fatal("普通文件被当作封存内容")
	}
}
