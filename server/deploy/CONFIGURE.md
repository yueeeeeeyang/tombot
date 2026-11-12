# 从单份参数生成部署配置

`tomdeploy-config` 为首次部署生成五份一致的配置，避免在服务、节点、镜像和登记文件中反复填写身份。它不安装软件、不启动服务、不连接数据库或 Docker，不生成或读取凭据，也不创建用户。输出可审阅后安装，必须继续通过 `tomdeploy-check` 和各组件实际启动检查。

## 调用与输出

将程序安装到 `/usr/lib/tombot/tomdeploy-config`，先准备固定非 root 系统用户及主组 `tombot-api`，不要加入 Docker 组。由 Linux root 执行，不接受命令参数：

```sh
TOM_DEPLOYMENT_REQUEST=/etc/tombot-setup/request.json \
TOM_DEPLOYMENT_OUTPUT=/etc/tombot-generated \
/usr/lib/tombot/tomdeploy-config
```

参数文件必须是 root 所有、私有、有限的普通文件，建议权限 0600；整个父链由 root 管理，不允许链接或组/其他用户写入。输出必须是规范绝对路径，其父目录已存在且受 root 保护，目标目录必须不存在。程序在同一父目录写入暂存文件并刷盘，然后原子发布整个目录；即使目标只是空目录也不会覆盖。并发执行只有一个能够发布成功。

| 输出文件 | 权限 | 消费者 |
| --- | --- | --- |
| `server.json` | root 0644 | API 与本机登记、管理员部署命令 |
| `node.json` | root 0600 | 受信节点 |
| `docker-install.json` | root 0600 | Docker 及宿主磁盘工具引导 |
| `computer-image.json` | root 0600 | 固定镜像准备 |
| `node-registration.json` | root 0644 | `tomnode-register` 的 `register` 操作 |

目录权限为 0755；可读文件仅包含非秘密配置或私有文件引用，没有凭据值。成功回执只声明 `scope: deployment_configuration_files`、节点/镜像 ID、宿主平台和实际 API UID/GID，不声明部署就绪。

正常失败清理此次暂存目录，不暴露半套输出。强制终止或主机断电可能留下隐藏暂存目录，但消费者不会读取它；使用同一原始参数与目标重试，不会把暂存内容当作已发布配置。发布后刷盘或回执输出失败会明确报错并保留输出，不能改用新身份覆盖它。

## 参数结构

下面是字段示意，节点 ID、镜像 ID、固定镜像引用与受测 Docker 清单须由此次部署提供，示意内容不能直接启动服务：

```json
{
  "version": 1,
  "node_id": "本次部署的节点 ULID",
  "node_name": "主电脑节点",
  "computer_capacity": 4,
  "image_id": "本次部署的镜像 ULID",
  "image": {
    "reference": "仓库/镜像@sha256:已验收的镜像摘要"
  },
  "docker": {
    "engine_package_version": "受测引擎包版本",
    "cli_package_version": "受测客户端包版本",
    "containerd_package_version": "受测容器运行时包版本",
    "e2fsprogs_package_version": "受测磁盘工具包版本",
    "util_linux_package_version": "受测基础工具包版本",
    "mount_package_version": "受测挂载工具包版本",
    "allowed_engine_versions": ["受测引擎补丁版本"],
    "repository_fingerprint": "官方仓库签名指纹",
    "timeout_seconds": 1800,
    "minimum_free_bytes": 1073741824
  },
  "server": {
    "address": "127.0.0.1:8443",
    "admin_origin": "https://admin.example.test",
    "models": {
      "origins": [{"url": "https://model.example.test"}]
    }
  }
}
```

节点和镜像 ID 只在顶层提供一次；程序绑定 API、节点、镜像与登记文件。ID 应保存在部署参数中，重试、登记和恢复都沿用原值。镜像平台使用当前宿主 `linux/amd64` 或 `linux/arm64`，API UID/GID 读取本机真实账户，不接受通过覆盖参数改绑身份。

`docker` 是完整的受测安装清单，格式沿用 `docker-install.schema.json`。生成器不选择最新版本、不联网查找软件包，不把测试夹具当作生产清单。`image` 可选 `authentication_file`、`call_timeout_seconds`（默认 30）与 `timeout_seconds`（默认 600）；认证字段只能引用私有文件，不能写入认证内容。

`server` 使用正式服务配置字段，继承 `serverapp.DefaultConfig` 的运行预算。下列文件引用有明确默认值，可通过同名字段改为其他受保护位置：

| 引用 | 默认位置 |
| --- | --- |
| 管理端 | `/opt/tombot/admin` |
| TLS 证书 | `/etc/tombot/tls/certificate.pem` |
| TLS 私钥 | `/etc/tombot/private/tls.key` |
| 身份、CSRF、活动模型加密文件 | `/etc/tombot/private/identity.key`、`csrf.key`、`model.key` |
| 四类数据库连接 | `/etc/tombot/private/identity.connection`、`control.connection`、`tenant.connection`、`dispatch.connection` |

默认活动模型密钥标识为 `current`。显式提供 `model_key_files` 或 `database.dsn_files` 时整体替换对应映射，不混入默认项；空映射、缺少必要数据库角色或活动模型密钥都会失败。生成器只保存引用，部署机密渠道仍须准备实际内容和正确归属。

可选 `node` 对象覆盖随包 `node.example.json` 的运行参数；节点基线直接嵌入程序，与示例保持同源。覆盖后仍经过节点正式校验和跨配置检查，不能缩短租约到无法接受默认 3 分钟验收，也不能改写节点身份或 API 调用身份。`computer_capacity` 必须小于节点 `runtime.maximum_egress_computers`，为独立验收临时电脑保留一个出网名额。默认节点上限为 8，因此容量最多设为 7；更大部署必须显式调整节点容量并完成实际资源验收。

输入为单一 UTF-8 JSON 对象，最大 256 KiB，拒绝重复键、空值、未知字段、大小写别名、过深嵌套及尾随对象。最终服务、节点、镜像与 Docker 配置分别复用各自正式解析器，并核对身份、目录、端点、预算和登记文件冲突。模型来源在配置解析时即使用实际网关规则检查，空来源、嵌入用户信息的 URL、重复来源或无效网络预算均拒绝。生成阶段不检查引用文件是否存在，以便先审阅配置、再准备凭据；安装前检查会验证实际文件和权限，不能据此跳过。

## 安装、变更与回滚

输出中默认路径对应发布包提供的 systemd 单元。确认参数后，将五份文件按表中所有者和权限安装到 `/etc/tombot/`，准备私有引用、管理端构建产物、PostgreSQL 及受测宿主环境。先执行迁移，再用 `TOM_SERVER_CONFIG` 和 `TOM_NODE_REGISTRATION_REQUEST=/etc/tombot/node-registration.json` 调用不带参数的 `tomnode-register`。启动 `tomd.service` 时，安装前检查会在 Docker 自动引导之前执行。

生成新配置不修改正在运行的配置或外部状态。更新时使用新的输出目录，停止接单并排空后成套安装，重新执行检查；不得用新 ID 规避原部署的失败或未知状态。失败回滚沿用发布包的完整版本与一致恢复点，不删除控制记录，不回退历史迁移。

发布格式版本 3 强制包含两个架构的生成程序和本说明，新核验器不接受格式 1、2。本变更不修改数据库模型或现有用户数据。配置生成成功、权限检查通过和发布包完整是不同范围的证据，真实 Ubuntu 首次安装、电脑能力、开户与恢复仍须分别验收。
