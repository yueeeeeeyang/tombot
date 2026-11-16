package browserprobe

import (
	"io.yueyang/tom/internal/infra/ids"
	"net/http/httptest"
	"testing"
)

func TestEvidenceRequiresEveryCheckAndFreshChallenge(t *testing.T) {
	challenge := ids.New()
	e := Evidence{ChallengeID: challenge, Sandbox: true, Windows: true, SharedStorage: true, Rendering: true, Input: true, Proxy: true, Cleanup: true}
	if e.Validate(challenge) != nil {
		t.Fatal("完整证据未通过")
	}
	for _, field := range []*bool{&e.Sandbox, &e.Windows, &e.SharedStorage, &e.Rendering, &e.Input, &e.Proxy, &e.Cleanup} {
		*field = false
		if e.Validate(challenge) == nil {
			t.Fatal("部分证据通过")
		}
		*field = true
	}
	if e.Validate(ids.New()) == nil || e.Validate("invalid") == nil {
		t.Fatal("挑战校验失效")
	}
}

func TestDiagnosticPageStrictlyBoundsInput(t *testing.T) {
	for _, suffix := range []string{"/" + ids.New(), "/bad", "/bad?x=1", "/", "/../", "/" + ids.New() + "?x=1"} {
		r := httptest.NewRequest("GET", Origin+suffix, nil)
		w := httptest.NewRecorder()
		handled, err := Serve(w, r)
		if !handled || err != nil {
			t.Fatal(handled, err)
		}
		valid := len(suffix) == 27
		if (w.Code == 200) != valid {
			t.Fatal(suffix, w.Code)
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("诊断页被缓存")
		}
	}
	r := httptest.NewRequest("GET", "http://example.test/", nil)
	if handled, _ := Serve(httptest.NewRecorder(), r); handled {
		t.Fatal("拦截了普通页面")
	}
}
