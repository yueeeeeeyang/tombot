# Docker 启动引导

`tomnode bootstrap` 和节点启动链共用本模块，自动准备 Docker 与宿主磁盘工具。支持 Debian/Ubuntu 的 APT 和 RHEL/Rocky/AlmaLinux/CentOS Stream/Fedora 的 DNF，不按发行版版本号或系统软件补丁版本设置白名单。需要 Linux root、amd64/arm64、systemd、cgroup v2 和可用签名软件源。离线电脑镜像与宿主发行版独立。

## 清单与安装

[版本 2 清单](../../deploy/runtime/linux.json)只包含版本号、总时限及磁盘余量，格式见 [JSON Schema](../../deploy/docker-install.schema.json)。旧版本字段明确拒绝，使用 `tomnode repair` 备份后迁移；不保留启动时忽略旧字段的兼容层，不涉及数据库迁移。

1. 核验发行家族、架构、权限、systemd、cgroup 控制器与磁盘余量，取得节点独占锁。
2. 已有 Docker 必须具备所需引擎代际，未安装时先检查冲突容器软件；不自动卸载 Podman、旧 Docker 或其他运行时。
3. 通过 dpkg/rpm 查询实际原生架构、版本和包状态。APT 检查 e2fsprogs、util-linux、mount，RPM 检查 e2fsprogs、util-linux。基础版本和安全更新均可复用；查询异常、损坏、半配置、待移除或错误架构仍失败。仅安装缺失项，不固定完整系统包版本。
4. 缺少 Docker 时，按实际发行版构造官方 HTTPS 软件源，验证固定官方公钥指纹。APT 设置 Signed-By，RPM 开启 gpgcheck 和 sslverify。不同的既有 Docker 源明确报错，不覆盖、不运行远程安装脚本。引擎、CLI、containerd 由该发行版的官方 stable 仓库解析。
5. 重新核验实际软件包及 `storagehost` 定义的六个程序：所有者、父目录、权限、链接目标与执行结果均必须有效。版本命令成功不能替代实际挂载、配额及内核能力验收。
6. 启动 Docker，经固定 Unix socket 检查 Linux、cgroup v2、CPU/内存/进程配额、seccomp，以及服务实际 API 最小/最大范围必须包含 1.47。不再限定某个 Docker 补丁版本。
7. 状态和实际包版本原子写入 `/var/lib/tombot-node/bootstrap/state.json`；错误清除成功报告，下一次启动重新核实真实状态，不盲目重放安装。

APT 使用 `--no-remove --no-upgrade --no-install-recommends`，DNF 不使用 `--allowerasing` 或关闭签名选项。包管理器可能为缺失项解析传递依赖；安装在新节点或排空维护窗口执行。整体安装、下载与命令有明确时限、输出上限和有限重试；取消终止子进程组，错误输出不泄露敏感内容。

软件源及公钥依据 [Docker Debian/Ubuntu 安装说明](https://docs.docker.com/engine/install/)、[Docker RHEL 安装说明](https://docs.docker.com/engine/install/rhel/)。发行版识别与仓库生成集中在 `linuxhost`，不把本机架构直接拼成 RPM 的软件包架构名。

## 验证与回滚

常规测试覆盖基础包与安全更新复用、只补缺包、APT/RPM 分支、签名错误、软件源冲突、包数据库错误、工具权限与链接、API 边界、资源能力、取消和失败报告。`make test-host-tools` 在隔离 Linux 中只读核验真实包数据库与程序，使用 `TOM_HOST_TOOLS_TEST=1` 及三个 `TOM_TEST_*_VERSION` 指定独立预期证据；RPM 无独立 mount 包。`make test-docker-engine` 只读核验实际 Docker 版本、API 与能力。

完整 systemd 首装、真实 loop 挂载、配额、SELinux 与电脑验收仍须目标宿主验证。`docker_ready` 只证明引擎与工具可用，不能提前报告电脑节点就绪。

失败保留配置、身份和已安装软件，不清理容器、卷或用户磁盘；修复后重试重新检查。节点程序和配置的备份、恢复及回滚见[节点部署说明](../../deploy/NODE.md#重试升级与回滚)。软件包不自动降级或卸载，回滚前应停止接单并核验实际宿主能力。
