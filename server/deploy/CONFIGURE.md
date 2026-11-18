# 从单份参数生成部署配置

`tomnode configure` 为首次部署生成五份一致的配置，避免在服务、节点、镜像和登记文件中反复填写身份。它不安装软件、不启动服务、不连接数据库或 Docker，不生成或读取凭据，也不创建用户。输出可审阅后安装，必须继续通过 `tomnode check-deployment` 和各组件实际启动检查。

## 调用与输出

将合并后的程序安装到 `/usr/lib/tombot/tomnode`，先准备固定非 root 系统用户及主组 `tombot-api`，不要加入 Docker 组。由 Linux root 执行 `configure` 子命令，不接受额外参数：

```sh
TOM_DEPLOYMENT_REQUEST=/etc/tombot-setup/request.json \
TOM_DEPLOYMENT_OUTPUT=/etc/tombot-generated \
/usr/lib/tombot/tomnode configure
```

参数文件必须是 root 所有、私有、有限的普通文件，建议权限 0600；整个父链由 root 管理，不允许链接或组/其他用户写入。输出必须是规范绝对路径，其父目录已存在且受 root 保护，目标目录必须不存在。程序在同一父目录写入暂存文件并刷盘，然后原子发布整个目录；即使目标只是空目录也不会覆盖。并发执行只有一个能够发布成功。

| 输出文件 | 权限 | 消费者 |
| --- | --- | --- |
| `server.json` | root 0644 | 主服务 API |
| `node.json` | root 0600 | 受信节点 |
| `docker-install.json` | root 0600 | Docker 及宿主磁盘工具引导 |
| `computer-image.json` | root 0600 | 固定镜像准备 |
| `node-install.json` | root 0600 | `tomnode install` 的可选节点差异参数（名称、容量、监听和可达地址） |

目录权限为 0755；可读文件仅包含非秘密配置或私有文件引用，没有凭据值。成功回执只声明 `scope: deployment_configuration_files`、节点/镜像 ID、宿主平台和实际 API UID/GID，不声明部署就绪。

正常失败清理此次暂存目录，不暴露半套输出。强制终止或主机断电可能留下隐藏暂存目录，但消费者不会读取它；使用同一原始参数与目标重试，不会把暂存内容当作已发布配置。发布后刷盘或回执输出失败会明确报错并保留输出，不能改用新身份覆盖它。

## 参数结构

下面是字段示意，节点 ID、镜像 ID、固定镜像引用与Docker 安装预算须由此次部署提供，示意内容不能直接启动服务：

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
    "version": 2,
    "timeout_seconds": 1800,
    "minimum_free_bytes": 1073741824
  },
  "server": {
    "address": "127.0.0.1:8443",
    "admin_origin": "https://admin.example.test",
    "models": {
      "concurrency": 16
    }
  },
  "control": {
    "listen": "节点公网地址:9443",
    "allow_public_listen": true,
    "api_certificate_fingerprint": "API 客户端证书叶证书的 SHA-256"
  }
}
```

`control` 描述 API 与节点之间的控制通道。客户端指纹由部署计算并填入。新增 `enrollment` 字段用于节点自动接入，其结构为公开连接信息（服务端 URL、HTTPS 指纹、节点可达 URL），用于生成完整运行配置。生成器不接收凭据码。

`allow_public_listen` 省略时为 `true`：云主机常以公网或运营商私有地址承载控制端口，私网限制容易被误判为不可用。需要只监听私网时显式写 `false`，节点会拒绝公开地址。无论取值如何，跨主机部署都应用安全组只放行 API 的出口地址，控制通道本身始终是双向证书指纹固定的 mTLS。

节点和镜像 ID 只在顶层提供一次；程序绑定 节点与镜像文件。ID 应保存在部署参数中，重试、登记和恢复都沿用原值。镜像平台使用当前宿主 `linux/amd64` 或 `linux/arm64`，API UID/GID 从部署参数读取的认证账户确定，不接受通过覆盖参数改绑身份。控制通道身份不再依赖 UID/GID，而是双方固定证书指纹。

`docker` 使用版本 2 安装预算，格式见 `docker-install.schema.json`；软件包和签名源由实际宿主发行版选择，生成配置时不执行安装。`image` 可选 `authentication_file`、`call_timeout_seconds`（默认 30）与 `timeout_seconds`（默认 600）；认证字段只能引用私有文件，不能写入认证内容。

`server` 使用正式服务配置字段，继承 `serverapp.DefaultConfig` 的运行预算。下列文件引用有明确默认值，可通过同名字段改为其他受保护位置：

| 引用 | 默认位置 |
| --- | --- |
| 管理端 | 编译进 `tomd`，无需目录配置 |
| TLS 证书 | `/etc/tombot/tls/certificate.pem` |
| TLS 私钥 | `/etc/tombot/private/tls.key` |
| 身份、CSRF、活动模型加密文件 | `/etc/tombot/private/identity.key`、`csrf.key`、`model.key` |
| 四类数据库连接 | `/etc/tombot/private/identity.connection`、`control.connection`、`tenant.connection`、`dispatch.connection` |

默认活动模型密钥标识为 `current`。显式提供 `model_key_files` 或 `database.dsn_files` 时整体替换对应映射，不混入默认项；空映射、缺少必要数据库角色或活动模型密钥都会失败。生成器只保存引用，部署机密渠道仍须准备实际内容和正确归属。

`database.migration_dsn_file` 默认引用 `/etc/tombot/private/migration.connection`，可在 `server.database` 中覆盖；显式空值或非规范路径会失败。安装前检查将此文件作为 API 必须可读的私有引用，要求仅所有者可读写。它与四份业务连接必须指向同一数据库，内容只由 `tomd` 启动时读取。先准备迁移身份、四个最小权限组及其独立登录身份，再启动 `tomd` 自动建表或升级；无需独立迁移二进制。迁移失败不开放业务端口，部署前仍须安排备份和维护窗口。

可选 `node` 对象覆盖随包 `node.example.json` 的运行参数；节点基线直接嵌入程序，与示例保持同源。覆盖后仍经过节点正式校验和跨配置检查。节点最大租约须覆盖服务端验收租约，默认 5 分钟，节点调用上限须覆盖服务端调用预算，默认 60 秒。节点身份和 API 调用身份由安装输入决定。`computer_capacity` 必须小于节点 `runtime.maximum_egress_computers`，为独立验收临时电脑保留一个出网名额。默认节点上限为 8，因此容量最多设为 7；更大部署必须显式调整节点容量并完成实际资源验收。

输入为单一 UTF-8 JSON 对象，最大 256 KiB，拒绝重复键、空值、未知字段、大小写别名、过深嵌套及尾随对象。最终服务、节点、镜像与 Docker 配置分别复用各自正式解析器，并核对身份、目录、端点、预算和登记文件冲突。模型网络范围与请求预算在配置解析时使用实际网关规则检查，无效拒绝网段或网络预算均拒绝；不再设置域名或公网 IP 白名单，旧 `models.origins` 必须删除。生成阶段不检查引用文件是否存在，以便先审阅配置、再准备凭据；安装前检查会验证实际文件和权限，不能据此跳过。

## 安装、变更与回滚

API 侧安装 `server.json`、准备私有引用与 PostgreSQL 后启动服务，并将 `admin_origin` 配置为节点可达的 HTTPS 来源。管理平台生成接入码后，节点完整解压同版本离线包，执行 `sudo ./tomnode install` 并粘贴即可。生成器输出的 `node-install.json` 仅含可选节点差异，可显式传给安装命令；服务端和证书信息通过接入码及已鉴权引导接口提供，详见[节点离线镜像安装](NODE.md)。

其余三份节点配置保留为完整配置审阅与手工部署输出；引用原参数指定的节点证书，不能与自动安装的新证书混用。`check-deployment` 检查成套输出时须将三个节点配置环境变量显式指向输出位置；运行命令的新默认位置为 `/etc/tombot/node/`。自动安装后的配置和证书均位于该目录。配置生成仍需本机 `tombot-api` 身份，独立节点的 `install` 不需要。

生成新配置不修改正在运行的配置或外部状态。更新时使用新的输出目录，停止接单并排空后成套安装，重新执行检查；不得用新 ID 规避原部署的失败或未知状态。失败回滚沿用发布包的完整版本与一致恢复点，不删除控制记录，不回退历史迁移。

发布格式版本 8 包含双架构的统一 `tomnode`，生成、检查和环境准备均为子命令；节点服务模板和单份安装模板也嵌入二进制。新核验器不接受格式 1–7，升级与回滚见[节点离线镜像安装](NODE.md)。多节点接入需新增数据库迁移并移除服务端的静态单节点字段，见[接入迁移说明](../docs/node-registration.md#升级与回滚)。配置生成成功、权限检查通过和发布包完整是不同范围的证据，真实 Ubuntu 首次安装、电脑能力、开户与恢复仍须分别验收。

`enrollment` 仅包含 `url`、`certificate_fingerprints`、`node_url` 三项公开连接参数，凭据码不经过配置生成器。旧参数中的 `enrollment_token_file` 已移除；首次安装使用新版模板和程序，回滚须恢复匹配版本。

管理平台已编入 `tomd`，配置生成器不再输出 `admin_directory`，输入中存在该字段会拒绝。旧配置升级时删除该字段，回滚旧程序需恢复原字段和配套前端目录；本次无数据库变更。
