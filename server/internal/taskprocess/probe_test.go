package taskprocess

import (
	"strings"
	"testing"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/processgroup"
)

func probeRequest() ProbeRequest {
	containerID := strings.Repeat("a", 64)
	return ProbeRequest{Version: 1, Group: processgroup.ServiceReference{Container: processgroup.ContainerReference{ContainerID: containerID, Path: "docker/" + containerID, Device: 1, Inode: 2}, ID: ids.New(), Kind: processgroup.ProbeService}, Deadline: agentchannel.Deadline{ExpiresNS: 1, TimeNamespace: 1}}
}

func TestProbeAuthorization(t *testing.T) {
	request := probeRequest()
	encoded, err := encodeProbe(request)
	if err != nil {
		t.Fatal(err)
	}
	if decoded, err := DecodeProbe(encoded); err != nil || decoded != request {
		t.Fatal("合法探针授权未完整往返", err)
	}
	for _, change := range []func(*ProbeRequest){
		func(r *ProbeRequest) { r.Version++ },
		func(r *ProbeRequest) { r.Group.Kind = processgroup.BrowserService },
		func(r *ProbeRequest) { r.Group.Container.Inode = 0 },
		func(r *ProbeRequest) { r.Group.ID = "" },
		func(r *ProbeRequest) { r.Deadline.ExpiresNS = 0 },
		func(r *ProbeRequest) { r.Deadline.TimeNamespace = 0 },
	} {
		changed := request
		change(&changed)
		if _, err := encodeProbe(changed); err == nil {
			t.Fatal("错误探针身份或期限被封存")
		}
	}
	for _, value := range []string{
		"null", "{}", string(encoded) + "{}", strings.Repeat(" ", 16<<10) + string(encoded),
		strings.Replace(string(encoded), `"version":1`, `"version":2,"version":1`, 1),
		strings.Replace(string(encoded), `"version":1`, `"Version":1`, 1),
		strings.Replace(string(encoded), `"version":1`, `"version":1,"uid":0`, 1),
		strings.Replace(string(encoded), `"version":1`, `"version":1,"program":"/bin/sh"`, 1),
	} {
		if _, err := DecodeProbe([]byte(value)); err == nil {
			t.Fatal("可覆盖执行身份或歧义授权被接受")
		}
	}
}
