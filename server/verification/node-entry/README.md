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

## 真实旧包升级回归

在装有 Docker 的本机运行。指定两个已发布的 Linux amd64 节点程序归档、候选程序归档和已解压的兼容镜像目录。验证镜像需包含 Python 3.9 以上、OpenSSL、tar 和 sha256sum。

```sh
python3 server/verification/node-entry/component_upgrade.py \
  --old "$OLD_NODE_PACKAGE" --previous "$PREVIOUS_NODE_PACKAGE" \
  --candidate "$CANDIDATE_NODE_PACKAGE" --image "$EXTRACTED_IMAGE_DIR" \
  --linux-image "$LOCAL_AMD64_TEST_IMAGE"
```

脚本在无外网、无宿主 Docker 套接字的临时容器执行真实旧版的首次安装和升级，再运行候选 CLI。发布输入只读挂载。测试在宿主启动处注入失败，在维护提交处杀死升级进程，核对回滚后的原安装摘要、证书、操作 ID 和重复停机次数。若两个包的 Bot 相同，可用 `--bot` 指定另一份真实 amd64 的 bot-agent，构造仅 Bot 有差异的测试包。Bot 用非 root 身份运行，在切换和回退期间检查原进程是否存活。

systemd 调用和 HTTPS 控制回执由本地测试端点提供，便于精确触发故障。签名授权、租约、原围栏和诊断 SQL 由 `internal/nodeenrollment` 和 `internal/nodefleet` 的 PostgreSQL 集成测试覆盖。演练未启动业务电脑容器，也不验证宿主挂载和 systemd 启动时间。
