package browser

import "testing"

func TestOriginCanonicalizationAndIsolation(t *testing.T) {
	for address, want := range map[string]string{
		"https://EXAMPLE.com.:443/a?query=value#part": "https://example.com",
		"http://example.com:080/a":                    "http://example.com",
		"https://example.com:8443/a":                  "https://example.com:8443",
		"https://例子.测试/a":                             "https://xn--fsqu00a.xn--0zwm56d",
		"https://例子.测试。/a":                            "https://xn--fsqu00a.xn--0zwm56d",
		"http://[0:0:0:0:0:0:0:1]:80/a":               "http://[::1]",
		"https://[::1]:8443/a":                        "https://[::1]:8443",
	} {
		if got, err := Origin(address); err != nil || got != want {
			t.Fatalf("源规范化失败：%q %v", got, err)
		}
	}
	for _, address := range []string{"https://user@example.com", "file:///tmp/x", "https://example.com:0", "https://example.com:65536", "https://[fe80::1%25eth0]/", "https://exa mple.com"} {
		if _, err := Origin(address); err == nil {
			t.Fatal("无效源被接受")
		}
	}
}
