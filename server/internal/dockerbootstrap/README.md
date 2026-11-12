# Docker 启动引导

`tomdocker-bootstrap` 是受保护的 Linux 部署命令，负责 Docker、宿主磁盘工具自动安装、启动和基础能力复验。它不提供 HTTP 接口，不接受远程命令、软件源或宿主路径。首个安装适配器限定 Ubuntu 24.04 LTS、systemd、cgroup v2，支持 amd64 和 arm64；其他环境明确拒绝。用户电脑镜像仍是 Debian 13，镜像工具不能替代宿主依赖。

当前命令及两种 Linux 架构构建已完成；安装流程通过隔离文件系统和命令边界测试。尚未在 Ubuntu 实机上完成新安装验收，也尚未交付经该验收的版本清单。Docker Desktop 的只读引擎检查仅证明当地引擎能力，不证明 Linux 自动安装、电脑镜像或用户隔离已完成。

## 发布与启动

发布包把固定命令安装到 `/usr/lib/tombot/tomdocker-bootstrap`，把经验证的清单安装到 `/etc/tombot/docker-install.json`。清单格式见 [JSON Schema](../../deploy/docker-install.schema.json)，包含准确的引擎、CLI、containerd、`e2fsprogs`、`util-linux`、`mount` 软件包版本、兼容引擎版本集合、官方签名指纹、总超时和磁盘余量。三个宿主版本字段为 `e2fsprogs_package_version`、`util_linux_package_version`、`mount_package_version`，均为必填，不读取浮动的候选版本填充。

这些值必须来自实际发布验收，不得复制单元测试夹具作为生产清单。测试中的软件包版本和签名数据仅用于验证控制流程。生产清单由 root 所有，文件及所有父目录不可被普通用户写入，不允许路径中的符号链接。

[systemd 前置单元](../../deploy/systemd/tombot-docker-bootstrap.service)以 root 运行固定命令，使用 `TOM_DOCKER_INSTALL_MANIFEST` 指定受保护清单。[节点单元](../../deploy/systemd/tombot-node.service)通过 `Requires` 和 `After` 依赖引导及镜像准备；[tomd 单元](../../deploy/tomd.service)通过 `Wants` 和 `After` 拉起节点，节点维护不会连带停止 API 登录与历史查询。节点程序已接通，受测清单发布及完整部署仍待验收，仅安装模板不会自动完成这些步骤。

API 和 Worker 不以 root 运行，不加入 Docker 用户组，也不持有 Docker socket。Docker 官方说明守护进程的管理权限具有重要的宿主权限影响，因此安装和运行控制应留在受信部署及节点组件中。[Docker 安全文档](https://docs.docker.com/engine/security/)

## 固定引导流程

1. 校验 root 身份、发行版、架构、systemd、必要程序、cgroup v2 控制器及磁盘余量。
2. 在 root 保护的 `/run/tombot-node` 中取得节点独占锁；遵守 APT 的包管理器锁和有限网络重试。
3. 若已有 Docker，验证准确版本是否属于清单允许集合；不自动升级、降级或卸载。若未安装 Docker，在安装任何软件之前检查冲突的容器软件包。
4. 查询三个宿主磁盘软件包的原生架构、完整版本和 dpkg 安装状态；若缺少软件包，更新索引并按清单一次安装。已有 Docker 也必须经过此步骤。已有版本不符、损坏、未完成配置或计划移除时失败，不以自动升级、降级、卸载或全局 `dpkg --configure -a` 掩盖未知状态。
5. 若未安装 Docker，检查其他 Docker 软件源并配置固定的官方 HTTPS 签名源；核验唯一主签名指纹及有效状态，固定安装引擎、CLI 和 containerd 版本。
6. 重新查询实际宿主软件包；检查六个磁盘程序的入口与目标目录、所有者、权限和可执行性，再执行无副作用的版本查询。接受发行版受保护的 `mkfs.ext4` → `mke2fs` 链接。工具路径与节点运行时共用 `storagehost` 契约，不执行格式化、文件分配、挂载或修复操作。
7. 启用并启动 Docker 服务，通过明确的本机 Unix socket 查询实际引擎，要求 Linux、cgroup v2、内存及进程和 CPU 配额、有效 seccomp 配置。
8. 将各阶段状态原子写入 `/var/lib/tombot-node/bootstrap/state.json`。宿主安装/复验阶段为 `installing_host_dependencies` / `verifying_host_dependencies`；成功报告的 `host_packages` 来自真实包查询。任何失败写为失败并清除成功报告，不能留下伪成功状态。

官方软件源和版本固定安装方式依据 [Docker Ubuntu 安装文档](https://docs.docker.com/engine/install/ubuntu/)。不下载并执行便利脚本；APT 更新的任何软件源错误都会中止，不能继续使用旧索引冒充安装成功。

宿主包使用系统已有的受信 Ubuntu 软件源，不引入第三方磁盘工具源。安装使用 `--no-remove`、`--no-upgrade`、`--no-install-recommends` 和三个固定版本；`--no-upgrade` 仅约束命令行上已安装的包，APT 仍可能为缺失包解析、安装或更新传递依赖，因此必须在新节点或排空维护窗口运行，不能宣称宿主所有软件完全不变。依据 [APT 参数说明](https://manpages.ubuntu.com/manpages/noble/man8/apt-get.8.html)和 [dpkg-query 状态定义](https://manpages.ubuntu.com/manpages/noble/man1/dpkg-query.1.html)处理失败；仅退出码 1 且空输出表示目标包未找到，致命或部分查询失败不能当作缺包。

命令使用绝对程序路径、固定参数和最小环境，不经过 Shell。整体安装、HTTP 下载和包管理器都有时限；取消时终止命令进程组，避免遗留安装子进程。下载和命令输出有大小上限；原始标准错误、请求正文和敏感环境不进入日志。

## 失败处理与回滚

不支持系统、权限不足、版本冲突、仓库失败、签名不符或引擎能力不足时，首次节点就绪必须阻断。修复环境后重启会先核实实际状态，不依据上一次状态文件盲目跳过步骤。

安装失败不会通过卸载既有软件、清理用户容器或删除卷自动回滚。已安装的受信软件包可能仍然存在，后续启动会重新核验；项目拥有的源文件采用精确内容检查，发现不同配置会报错。引导升级应在排空节点及备份验证后按受测清单部署，回滚同样需要匹配的受测版本与镜像，不得在未知状态下恢复接单。

本次清单契约新增三个必填字段，没有旧格式默认值或兼容层；发布时必须同时更新二进制和完整清单，不涉及业务数据迁移。软件包安装结果未知时本次引导失败；下次引导先检查实际状态，若已完整安装则不重放 APT。包数据库中断、半安装或版本漂移不能凭上次成功记录继续接单。回滚不得用缺少工具检查的旧程序绕过失败，也不自动删除已有工具；只能部署已验收且与实际宿主版本匹配的整套清单和程序。

`docker_ready` 仅表示 Docker 基础检查及宿主工具检查完成，不证明内核允许 loop 挂载或配额可用。节点还必须验证存储磁盘配额、容器网络隔离、固定镜像、真实终端、Chromium 沙箱、文件能力及任务进程回收，然后才可在数据库标记节点就绪和接受用户开户。

## 验证入口

常规测试纳入 `make lint check test`，覆盖首次安装、重复启动、签名和软件源冲突、APT 失败、服务启动失败、版本及资源能力不足、目录权限与符号链接、锁超时、状态写入和进程取消。实际命令输出超过上限的回归测试防止复制路径绕过输出限制。

宿主包回归覆盖已有 Docker 补装、持有/移除残留/半配置、错误架构与版本、查询部分失败、安装后未落盘、版本漂移、结果未知后的恢复、不重复安装、工具缺失/权限漂移/坏链接及取消。`make test-host-tools` 是 Linux 专用只读检查，显式提供 `TOM_HOST_TOOLS_TEST=1`、`TOM_TEST_E2FSPROGS_VERSION`、`TOM_TEST_UTIL_LINUX_VERSION`、`TOM_TEST_MOUNT_VERSION`；只在隔离 Ubuntu 24.04 环境检查真实 dpkg 和工具，不能据此宣称 systemd 自动安装、挂载或整节点已验收。

`make test-docker-engine` 是显式只读引擎检查，必须提供 `TOM_TEST_DOCKER_BIN`、`TOM_TEST_DOCKER_SOCKET` 和 `TOM_TEST_DOCKER_VERSION`。检查使用临时 CLI 配置，不读取用户登录配置，不创建或删除任何现有容器。当前已在本机 Docker 29.7.2、Linux、cgroup v2 上通过此检查。
