package dockernode

import (
	"testing"

	"io.yueyang/tom/internal/computer"
)

func TestContainerInheritedImageLabels(t *testing.T) {
	for _, scenario := range []string{"inherited", "layout_changed", "layout_missing", "unexpected_identity", "node_changed"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			resource := f.node.resource(f.specification)
			// 重现 Docker 将镜像标签合并到容器、而卷只保留资源身份标签的行为。
			configuration := f.node.configuration(resource)
			configuration.Labels[computer.StorageLayoutLabel] = computer.StorageLayoutVersion
			configuration.Labels["org.opencontainers.image.title"] = "电脑镜像"
			switch scenario {
			case "layout_changed":
				configuration.Labels[computer.StorageLayoutLabel] = "1"
			case "layout_missing":
				delete(configuration.Labels, computer.StorageLayoutLabel)
			case "unexpected_identity":
				configuration.Labels["io.tombot.unrecognized"] = "unexpected"
			case "node_changed":
				configuration.Labels["io.tombot.node"] = "changed"
			}
			if _, err := f.engine.createContainer(t.Context(), resource.Name, configuration); err != nil {
				t.Fatal(err)
			}
			current, err := f.engine.inspectContainer(t.Context(), resource.Name)
			if err != nil {
				t.Fatal(err)
			}
			vol, err := f.engine.createVolume(t.Context(), resource.Name, resource.WorkspacePath, resource.Labels)
			if err != nil {
				t.Fatal(err)
			}
			workspace, err := f.storage.Ensure(t.Context(), resource)
			if err != nil {
				t.Fatal(err)
			}
			err = f.node.validateSnapshot(resource, &record{}, snapshot{container: current, volume: vol, workspace: workspace, hasContainer: true, hasVolume: true})
			if scenario == "inherited" {
				if err != nil {
					t.Fatal("镜像自带布局标签导致容器在启动前被误拒绝", err)
				}
			} else {
				requireFailure(t, err, "container_identity_mismatch")
			}
			if _, exists := resource.Labels[computer.StorageLayoutLabel]; exists {
				t.Fatal("容器标签污染了卷身份")
			}
		})
	}
}
