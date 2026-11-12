# 节点入口集成验证环境

这是一组可丢弃 Linux root 测试环境，不能在生产节点执行。它运行真实节点组件和正式入口，但 Docker API 仅以本地 Unix HTTP 明确返回空资源，不执行容器创建或磁盘挂载。无需宿主 Docker socket、宿主 cgroup 或已有用户卷。

测试镜像只补齐宿主所需 `e2fsprogs`、`util-linux`、`mount`，不作为用户电脑镜像发布。`TOM_TEST_BASE_IMAGE` 可指定本机已有且经核验的 Debian 13 镜像；缺少真实工具时构造器必须失败，不能用同名脚本代替。

在 `server/` 构建独立测试二进制和正式节点入口；`GOARCH` 选择本次 Docker Linux 环境对应的 `arm64` 或 `amd64`：

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go test -c -tags=nodeintegration -o /tmp/tombot-nodeapp.test ./internal/nodeapp
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o /tmp/tomnode ./cmd/tomnode
docker build -t tombot-node-entry-test:local verification/node-entry
docker run --rm --init --network none --read-only \
  --cap-drop ALL --cap-add CHOWN --cap-add SETUID --cap-add SETGID \
  --security-opt no-new-privileges --pids-limit 128 --memory 512m --cpus 2 \
  --tmpfs /run:rw,nosuid,nodev,size=32m,mode=0755 \
  --tmpfs /tmp:rw,nosuid,nodev,size=32m,mode=1777 \
  --mount type=bind,src=/tmp/tombot-nodeapp.test,dst=/opt/nodeapp.test,readonly \
  --mount type=bind,src=/tmp/tomnode,dst=/opt/tomnode,readonly \
  --env TOM_NODE_ENTRY_TEST=1 --env TOM_NODE_BINARY=/opt/tomnode \
  --entrypoint /opt/nodeapp.test tombot-node-entry-test:local \
  -test.run '^TestLinux' -test.v -test.timeout 90s
```

三项 capability 分别用于真实控制 socket 所属组和非 root 客户端降权，不授予 SYS_ADMIN，也不开放特权模式。辅助客户端测试只由父测试启动；顶层单独出现 skip 是该辅助入口的身份约束，实际客户端由父测试以配置 UID/GID 执行。

验收覆盖控制服务启动与关闭，不覆盖 systemd 的真实安装启用、完整镜像供给、实际 ext4 配额、cgroup 进程创建和强制回收、浏览器能力或账户激活。节点控制通知与电脑 ready 必须继续分开。
