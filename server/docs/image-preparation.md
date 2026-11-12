# 固定电脑镜像准备

`tomimage-prepare` 在 Linux 宿主以 root 执行，负责将部署指定的一个固定摘要镜像准备到本机 Docker，并复验平台和代理入口。API/Worker 不读取仓库认证、不加入 Docker 组，也不发起镜像拉取。命令没有创建容器、删除镜像、修改标签、导入归档、设置节点或用户电脑就绪的接口。

## 部署入口

节点和镜像身份先按[目录登记](node-registration.md)生成并保留。镜像准备清单使用相同的 `node_id`、`image_id` 与固定仓库摘要；本命令不连接 PostgreSQL，后续完整节点激活还须核对这两类记录。`make build` 和 `make release` 已包含本命令，安装与宿主架构匹配的程序到 `/usr/lib/tombot/tomimage-prepare`。

清单格式见 [JSON Schema](../deploy/computer-image.schema.json)。下面是字段示意，中文占位必须替换为发布时的真实值，不能直接安装为生产清单：

```json
{
  "version": 1,
  "node_id": "已登记的节点 ULID",
  "image_id": "已登记的镜像 ULID",
  "reference": "实际仓库及镜像路径@sha256:实际的64位小写十六进制摘要",
  "platform": "linux/arm64",
  "docker_socket": "/run/docker.sock",
  "state_directory": "/var/lib/tombot-node/images",
  "call_timeout_seconds": 30,
  "timeout_seconds": 3600
}
```

清单和可选认证文件为 root 所有的普通文件，权限 0600，拒绝硬链接、符号链接、重复键、大小写别名、未知字段、空值及尾随内容。所有路径须为规范绝对路径，父目录均由 root 管理且不可被普通用户写入。平台必须与程序实际运行宿主相同，仅接受 Linux amd64/arm64。状态目录由部署建立，不能与用户工作区或节点生命周期记录混用。Docker socket 必须由 root 持有且权限为 0600 或 0660；镜像准备和运行节点共用此检查，拒绝向所有用户开放的管理 socket。

```sh
TOM_IMAGE_PREPARATION_MANIFEST=/etc/tombot/computer-image.json \
/usr/lib/tombot/tomimage-prepare
```

不接受命令行参数。[镜像准备单元](../deploy/systemd/tombot-image-prepare.service)依赖 Docker 引导并位于独立节点之前；[节点单元](../deploy/systemd/tombot-node.service)依赖该准备步骤，[API 单元](../deploy/tomd.service)再依赖节点。模板使用 `StateDirectory=tombot-node/images` 创建私有记录目录，不自动生成真实镜像摘要或登记身份。[独立节点程序](../internal/nodeapp/README.md)已接通，完整节点激活仍待联合验收，本次没有安装、启用或重启宿主服务。

## 拉取和复验

同一镜像 ID 使用持久文件锁串行处理，总时限也包含等待锁。每次先刷盘 `preparing` 意图，再检查实际引擎；即使上次记录成功，也不能跳过本次检查。只有明确返回镜像不存在时，才发送一次指定摘要与平台的拉取请求；未知引擎错误不触发拉取，下载失败不在同一次调用里自动重试。

拉取采用固定 Engine API v1.47 的 `fromImage` 和 `platform` 参数，通过本机 Unix socket 连接，不读取 Docker CLI 配置或环境代理。按 Docker 协议，关闭 HTTP 连接会取消对应拉取；本实现用总体上下文约束下载，连接与响应头使用较短时限。[Docker Engine API 定义](https://raw.githubusercontent.com/moby/moby/v27.5.1/docs/api/v1.47.yaml)

进度流单帧最多 64 KiB、总量最多 64 MiB；截断、格式错误、流内错误、读取失败和超限都返回失败。原始进度、认证和错误正文不写入业务日志。HTTP 200 或进度中的完成文案均不能证明镜像可用，必须重新检查：仓库摘要包含请求的精确引用、Docker 镜像 ID 格式正确、平台匹配，以及固定 `/usr/lib/tombot/computer-agent` 入口、空附加命令和 `/workspace` 工作目录。仓库摘要与 Docker 镜像 ID 需分别核对，不要求两者数值相同；仅有本地构建 ID、没有对应仓库摘要记录的镜像不能通过准备。

镜像还必须具有 `io.tombot.computer.storage-layout=2` 标签，与同一持久盘的业务及浏览器子目录挂载契约一致；缺失标签或旧版本不能通过准备。

开户 `Ensure` 也在创建磁盘、卷和容器之前检查实际本地镜像及同一入口契约，不依赖准备记录中的成功标记。镜像缺失不会由开户隐式拉取。镜像元数据不证明预装工具、桌面、Chrome 沙箱、磁盘配额、进程回收、联网或共享浏览器已通过运行检查；这些仍由完整能力探针和发布验收负责。

## 私有仓库

需要认证时，在清单中增加 `authentication_file`，其值只保存受保护文件的路径。认证文件使用 Docker 认证 JSON 的 `serveraddress` 加以下两种方式之一：`username` 与 `password`，或 `identitytoken`。两种方式互斥，拒绝未知字段和空值。认证文件由部署的机密渠道供应，不把真实内容写入仓库、清单示例或命令参数。

`serveraddress` 必须与镜像仓库完全一致，不包含协议；Docker Hub 的隐式仓库使用 `index.docker.io`。认证以 base64url JSON 放入本机 Docker 请求的 `X-Registry-Auth` 头，只在确实需要拉取时读取，不使用凭据助手或用户已有登录配置。[Docker 认证协议](https://raw.githubusercontent.com/moby/moby/v27.5.1/docs/api/v1.47.yaml)

## 回执、恢复与回滚

标准输出只有成功 JSON 回执，包含 `scope: image`、`state: available`、节点和镜像身份、仓库摘要、平台及实际 Docker 镜像 ID。它没有 `ready` 字段。退出码 0 表示本次本地镜像复验及持久记录完成；错误或回执输出失败返回非零，不能据此推断没有下载内容。

状态存于独立目录中的 `<image_id>.image.json`，版本 1，阶段为 `preparing`、`available`、`failed`。相同镜像身份不能改绑另一节点、摘要或平台，已记录的实际镜像 ID 不能悄悄替换。更新镜像必须登记并部署新 ID 和新摘要。文件原子替换，并同步文件与父目录；损坏、权限漂移或链接替身均失败且不被覆盖。

中断后保留相同身份重新调用：先检查实际引擎，如果上次下载已完成则直接复验，不重复下载；本地仍缺失才开启新的有限时拉取。命令不删除镜像或其他资源，失败可能留下已缓存层。磁盘同步可能受底层存储阻塞，不能因总体时限已到就伪造成功；systemd 的启动与停止时限另提供进程监督。远端认证、仓库不可用、磁盘空间不足或引擎出错时均阻断准备。

此项不改变数据库结构、历史迁移和电脑镜像内容。服务模板、新二进制、真实发布清单和登记身份须成套部署；回滚前停单排空，保留用户卷、目录与准备记录，恢复配套程序和配置。不能通过删除记录、改摘要、退回浮动标签或跳过镜像检查恢复接单。首次准备不等于完整生产上线，真实镜像发布和独立节点激活仍须联合验收。


## 验证证据

`make lint check test` 覆盖固定摘要与平台、镜像契约、输入边界、各阶段故障、取消、未知拉取结果对账、原身份保护、文件损坏和认证范围。`linux && imageintegration` 测试使用专用禁网、只读、无附加 capability 容器，在真实受保护目录和 Unix HTTP 上验证八个并发准备只拉取一次、下载连接取消、错误父目录/链接/权限/平台及记录拒绝。Docker 响应为明确测试替身，不据此声称远程仓库下载已验收。

`make test-image-preparation` 是独立只读验收入口，必须指定 `TOM_TEST_DOCKER_SOCKET` 和 `TOM_TEST_COMPUTER_IMAGE`，后者为本机已有的电脑镜像固定摘要。测试明确禁止拉取，通过实际 Docker API 验证正式镜像元数据和重复准备。当前 arm64 镜像 `tombot-computer@sha256:3d7e6960f34fd1c724ef50b5608887e155a3d5b0ca207e8484b5e4a927899dcb` 已通过此项；没有推送、修改或删除镜像，没有运行生产准备单元。真实远程仓库首次下载、私有仓库认证、Ubuntu systemd 首次部署及完整节点激活仍待联合验收。
