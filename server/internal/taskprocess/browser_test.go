package taskprocess

import (
	"testing"

	"io.yueyang/tom/internal/processgroup"
)

func browserRequest() BrowserRequest {
	probe := probeRequest()
	probe.Group.Kind = processgroup.BrowserService
	return BrowserRequest{Version: 1, Group: probe.Group, Deadline: probe.Deadline}
}

func TestRetiredBrowserAuthorization(t *testing.T) {
	request := browserRequest()
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*BrowserRequest){
		func(r *BrowserRequest) { r.Version++ },
		func(r *BrowserRequest) { r.Group.Kind = processgroup.ProbeService },
		func(r *BrowserRequest) { r.Group.Container.Inode = 0 },
		func(r *BrowserRequest) { r.Group.ID = "../foreign" },
		func(r *BrowserRequest) { r.Deadline.ExpiresNS = 0 },
		func(r *BrowserRequest) { r.Deadline.TimeNamespace = 0 },
	} {
		changed := request
		mutate(&changed)
		if err := changed.Validate(); err == nil {
			t.Fatal("错误浏览器身份被封存")
		}
	}
}
