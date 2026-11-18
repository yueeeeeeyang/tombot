package noderelease

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestOnlyFixedRegularPackageIsExtracted(t *testing.T) {
	for _, bad := range []string{"", "缺件", "重复", "../escape", "链接"} {
		t.Run(bad, func(t *testing.T) {
			var b bytes.Buffer
			z := gzip.NewWriter(&b)
			w := tar.NewWriter(z)
			names := []string{"tomnode", "node-release.json", "SHA256SUMS", "安装说明.txt", "node-install.example.json", "computer-image.tar"}
			switch bad {
			case "缺件":
				names = names[:5]
			case "重复":
				names = append(names, "tomnode")
			case "../escape":
				names = append(names, bad)
			}
			for _, n := range names {
				h := tar.Header{Name: n, Mode: 0644, Size: 1}
				if bad == "链接" && n == "tomnode" {
					h.Typeflag = tar.TypeSymlink
					h.Linkname = "../target"
					h.Size = 0
				}
				if err := w.WriteHeader(&h); err != nil {
					t.Fatal(err)
				}
				if h.Size == 1 {
					if _, err := w.Write([]byte{1}); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			if err := z.Close(); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			err := Extract(t.Context(), &b, dir)
			if (err == nil) != (bad == "") {
				t.Fatal(err)
			}
			if err == nil {
				f, err := os.Stat(filepath.Join(dir, "tomnode"))
				if err != nil || f.Mode().Perm() != 0700 {
					t.Fatal("解包程序权限无效", err)
				}
			}
		})
	}
}
