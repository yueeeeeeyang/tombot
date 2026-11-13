package deployment

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"

	"io.yueyang/tom/internal/dockerbootstrap"
	"io.yueyang/tom/internal/dockernode"
	"io.yueyang/tom/internal/nodeapp"
	"io.yueyang/tom/internal/serverapp"
)

// Check 使用 systemd 固定账户，在 Linux root 下进行只读检查；错误不包含配置内容或私有路径。
func Check(ctx context.Context, paths Paths) (Report, error) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return Report{}, errors.New("部署检查必须由 Linux root 执行")
	}
	api, err := lookupAccount()
	if err != nil {
		return Report{}, err
	}
	return check(ctx, paths, api, "linux/"+runtime.GOARCH, filesystem{base: "/", owner: 0})
}

func lookupAccount() (account, error) {
	identity, err := user.Lookup("tombot-api")
	if err != nil {
		return account{}, errors.New("API 系统账户不存在或无法查询")
	}
	group, err := user.LookupGroup("tombot-api")
	if err != nil || identity.Gid != group.Gid {
		return account{}, errors.New("API 系统账户必须以 tombot-api 作为主组")
	}
	uid, uidErr := strconv.ParseUint(identity.Uid, 10, 32)
	gid, gidErr := strconv.ParseUint(group.Gid, 10, 32)
	if uidErr != nil || gidErr != nil || uid == 0 || gid == 0 {
		return account{}, errors.New("API 系统账户标识无效或拥有 root 身份")
	}
	groups, err := identity.GroupIds()
	if err != nil {
		return account{}, errors.New("API 系统账户组无法查询")
	}
	dockerGroup, err := user.LookupGroup("docker")
	if err != nil {
		var absent user.UnknownGroupError
		if !errors.As(err, &absent) {
			return account{}, errors.New("Docker 系统组无法查询")
		}
	} else {
		for _, groupID := range groups {
			if groupID == dockerGroup.Gid {
				return account{}, errors.New("API 系统账户不得属于 Docker 系统组")
			}
		}
	}
	return account{uid: uint32(uid), gid: uint32(gid)}, nil
}

func check(ctx context.Context, paths Paths, api account, platform string, fs filesystem) (Report, error) {
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	var value configuration
	loaders := []struct {
		name  string
		rule  fileRule
		parse func([]byte) error
	}{
		{"API", fileRule{path: paths.Server, maximum: 128 << 10, api: true}, func(encoded []byte) (err error) { value.server, err = serverapp.ParseConfig(encoded); return err }},
		{"节点", fileRule{path: paths.Node, maximum: 16 << 10, private: true}, func(encoded []byte) (err error) { value.node, err = nodeapp.ParseConfig(encoded); return err }},
		{"Docker 引导", fileRule{path: paths.Docker, maximum: 64 << 10, private: true}, func(encoded []byte) error { _, err := dockerbootstrap.ParseManifest(encoded); return err }},
		{"镜像", fileRule{path: paths.Image, maximum: 16 << 10, private: true}, func(encoded []byte) (err error) {
			value.image, err = dockernode.ParseImageManifest(encoded)
			return err
		}},
	}
	for _, loader := range loaders {
		encoded, err := fs.readConfig(loader.rule, api)
		if err != nil {
			return Report{}, fmt.Errorf("%s配置检查失败：%w", loader.name, err)
		}
		if err := loader.parse(encoded); err != nil {
			return Report{}, fmt.Errorf("%s配置未通过所属组件校验", loader.name)
		}
	}
	if err := value.validateGraph(paths, api, platform); err != nil {
		return Report{}, err
	}
	for _, reference := range value.references() {
		if _, err := fs.inspect(reference, api); err != nil {
			return Report{}, err
		}
	}
	if err := fs.directory(value.server.AdminDirectory, api, false, true, false); err != nil {
		return Report{}, err
	}
	for _, directory := range []string{filepath.Dir(value.node.Control.CertificateFile), filepath.Dir(value.node.Control.PrivateKeyFile), filepath.Dir(value.server.Node.CertificateFile), filepath.Dir(value.server.Node.PrivateKeyFile)} {
		if err := fs.directory(directory, api, false, true, false); err != nil {
			return Report{}, err
		}
	}
	for _, directory := range value.directories() {
		if err := fs.directory(directory, api, false, false, true); err != nil {
			return Report{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	return Report{Scope: "deployment_configuration", NodeID: value.node.NodeID, ImageID: value.image.ImageID, Platform: platform, APIUID: api.uid, APIGID: api.gid}, nil
}
