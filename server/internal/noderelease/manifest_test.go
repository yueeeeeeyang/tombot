package noderelease

import "testing"

func TestTaskManifestBindsArchiveAndLimits(t *testing.T) {
	sum := Digest([]byte("测试归档"))
	artifact := Digest([]byte(sum))
	valid := Manifest{Checksums: sum, Version: 1, Label: "v1", Platform: "linux/amd64", Size: 9, Chunks: []string{sum}}
	if err := valid.Validate(artifact); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Manifest){
		func(m *Manifest) { m.Platform = "windows/amd64" }, func(m *Manifest) { m.Size = ChunkSize + 1 }, func(m *Manifest) { m.Chunks = []string{artifact} }, func(m *Manifest) { m.Label = "../escape" }, func(m *Manifest) { m.Checksums = "x" }, func(m *Manifest) { m.Version = 0 },
	} {
		m := valid
		change(&m)
		if m.Validate(artifact) == nil {
			t.Fatal("不完整或不匹配的清单被接受", m)
		}
	}
}
