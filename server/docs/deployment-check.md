# 安装前部署配置核对

`tomnode check` 只检查节点、Docker 和镜像三份配置及节点自身的引用文件，不需要本机存在 API 系统用户、API 配置、数据库连接或管理静态目录。成功返回 `scope: node_configuration`。跨主机节点使用此入口，`tomnode start` 会在安装任何宿主软件之前自动调用。

`tomnode check-deployment` 保留完整部署检查，核对 API、节点、Docker 与镜像四份配置和跨组件关系。它要求本机具有 API 系统身份及全部引用文件，只读取配置和文件元信息，不读取凭据内容、连接数据库或 Docker，不创建状态目录。

## 启动契约

一个 [systemd 节点单元](../deploy/systemd/tombot-node.service)调用 `tomnode start`：节点配置检查 → Docker 与宿主磁盘工具引导 → 固定镜像准备 → 节点控制入口。每次启动重新检查，任一步失败或取消均不进入后续阶段。API 单元独立运行；实际开户仍须通过完整能力验收。

只需安装 `/usr/lib/tombot/tomnode`，两个检查子命令均须在 Linux root 下运行。文件路径可使用默认值或环境变量覆盖：

| 环境变量 | 默认路径 |
| --- | --- |
| `TOM_NODE_CONFIG` | `/etc/tombot/node/node.json` |
| `TOM_DOCKER_INSTALL_MANIFEST` | `/etc/tombot/node/docker-install.json` |
| `TOM_IMAGE_PREPARATION_MANIFEST` | `/etc/tombot/node/computer-image.json` |
| `TOM_SERVER_CONFIG`（仅成套检查） | `/etc/tombot/server.json` |

检查有 25 秒预算，没有重试或失败放行分支。单独调用只读检查不安装软件或启动服务；统一启动单元还需要执行安装和节点恢复，因此保留宿主挂载可见性，不使用私有挂载命名空间。生成配置见[配置生成入口](../deploy/CONFIGURE.md)。

节点检查与成套检查共用节点身份、镜像平台、Docker socket、目录重叠及配置引用隔离规则，复用正式组件解析器和文件权限检查。下面的 API 账户、租约和跨组件预算要求仅由成套检查核对，未在节点本机伪造这些信息。

## 检查范围

- 复用 `serverapp`、`nodeapp`、`dockerbootstrap`、`dockernode` 的正式解析入口；配置自身的格式、容量和领域约束只有一套实现。
- 固定系统用户与主组均为 `tombot-api`，必须是非 root，身份用于 API 文件访问校验，不能属于 Docker 组。首次安装前 Docker 组尚不存在可以继续，组查询失败不能当作不存在。
- API、节点和镜像清单使用同一个节点 ID，验收镜像 ID 一致；API 节点 URL 与监听端口一致；通信身份由双方证书指纹固定。Docker 引导、节点和镜像准备统一使用 `/run/docker.sock`，镜像架构必须与当前 Linux 宿主一致。
- 节点最大租约覆盖开户与独立验收，节点请求时限覆盖 API 的节点调用预算，用户桌面额度不超过电脑代理并发容量。节点配置示例的最大租约已改为 5 分钟，覆盖 API 默认 3 分钟验收租约；原 2 分钟示例会被明确拒绝。
- 引导、镜像、节点控制、磁盘控制、用户存储、管理静态目录不能互相包含；控制端点及配置、凭据引用不能落入这些目录或复用同一路径。
- 四份配置必须属于 root，父目录无链接且不可被其他身份改写。API 配置须能被 `tombot-api` 读取，通常使用 root 所有、0644，或以 API 主组读取的 0640；其他三份使用 root 0600。
- 引用文件必须是有限的普通文件、单一硬链接、无目录链接，权限不可被组或其他用户改写。私有引用不能向组或其他用户开放，API 私有文件应属于 `tombot-api`，使用 0600 或 0400，并保证整个父链可遍历。32 字节加密文件校验容量，其他引用沿用实际读取器的容量上限。
- 管理静态目录和已有状态目录检查父链。尚不存在的状态目录允许交给实际组件创建；默认 `/run/tombot` 由节点单元以 0711 创建，自定义控制端点父目录则须提前准备并允许 API 遍历。

成功只输出 `scope: deployment_configuration`、节点和镜像 ID、架构、API UID/GID；失败退出非零，没有成功回执。凭据格式、TLS 有效期和配对、数据库角色与迁移、静态资源完整性、宿主工具版本、Docker 真实能力仍由对应组件验证。配置文件可在检查后被 root 修改，因此消费者继续保留全部运行时检查；该回执不是永久授权或生产就绪声明。

## 部署变化与回滚

发布包格式为 7，只发布合并后的 `tomnode` 与一个节点服务单元。旧辅助程序及前置服务不再交付；升级时停止旧单元、安装新程序与单元、移除旧入口后重新加载 systemd。多节点自动接入的数据库与配置变更见[接入迁移说明](node-registration.md#升级与回滚)，不接受已移除的单节点字段。

回滚须停单排空，恢复原整套程序与单元及配置；保留用户磁盘、节点代次、失败证据与关闭屏障，不能只恢复旧二进制而保留新命令行。完整步骤见[单文件节点部署](../deploy/NODE.md)。


## 验证入口

常规 `make lint check test` 包含配置关系、权限边界、命令失败路径及统一启动顺序、独立节点无需 API 文件及 systemd 单元回归。`TestLiveDeploymentCheck` 通过 `deploymentintegration` 标签编译，需使用 `verification/deployment/Dockerfile` 的专用容器；用真实系统账户验证正常配置、root 私有 API 配置拒绝、旧租约拒绝和不可读引用拒绝。该镜像的 `forbidden` 构建阶段仅用于验证 Docker 组成员被拒绝。

测试配置中的包版本、镜像引用与凭据占位仅用于解析和元信息验证；没有执行安装、拉取、开户或连接真实服务，不能当作受测生产发布清单。真实 Ubuntu systemd 首次安装、宿主挂载/配额/cgroup 及完整开户恢复联调仍须另外验收。
