package deployment

import (
	"testing"

	"io.yueyang/tom/deploy"
)

func TestNodeInstallTemplateUsesAutomaticDefaults(t *testing.T) {
	var options NodeInstallOptions
	if decodePreparation([]byte(deploy.NodeInstallTemplate()), &options) != nil || options != (NodeInstallOptions{}) {
		t.Fatal("默认模板不应要求手填发布参数")
	}
	for _, encoded := range []string{`{"server":{}}`, `{"docker":{}}`, `{"node_name":"甲","node_name":"乙"}`, `{"token":"禁止凭据字段"}`, `{"computer_capacity":null}`} {
		if decodePreparation([]byte(encoded), &options) == nil {
			t.Fatal("接受了过时字段、歧义或凭据")
		}
	}
}
