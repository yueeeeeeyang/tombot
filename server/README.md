# TomBot 多用户服务端

服务端使用 PostgreSQL，提供 v2 用户接口和同源 Web 管理平台。每个用户拥有一台 Docker 电脑，用户名下各 Bot 在该容器中运行独立 Agent 进程与 Linux 桌面；共享业务文件、工具和浏览器账号状态。模型由服务端统一管理。

首次部署推荐使用[服务端一键安装](deploy/SERVER.md)：在 Debian/Ubuntu 或 RHEL/Rocky/AlmaLinux/CentOS Stream/Fedora 的 systemd 主机执行 `sudo ./tomd install`，确认访问地址即可自动准备本机 PostgreSQL、证书、密钥、配置和开机启动。用新版本程序执行 `sudo ./tomd-new upgrade` 升级，`sudo tomd status` 检查状态。管理平台已内置，无需单独部署前端。

当前正在按[改造方案](docs/multi-user-refactor.md)实施。`tomd` 已切换到新的应用组合，提供认证、机器人目录、消息受理、运行查询、管理接口及后台 Worker；整体功能仍未完成。首位管理员的[本机部署命令](docs/admin-bootstrap.md)、[节点与镜像目录登记](docs/node-registration.md)、[固定镜像准备](docs/image-preparation.md)及[独立节点程序](internal/nodeapp/README.md)已接通；[节点控制心跳与过期门禁](docs/node-health.md)也已接通，[节点专用验收与镜像就绪发布](docs/node-verification.md)已接通；共享浏览器探针、真实部署验收及客户端 v2 切换尚待完成。按部署、浏览器、业务、管理平台、客户端、旧实现清理、交付验收的顺序继续；远程电脑交互功能暂缓并单独设计。不能以 API 或节点控制服务启动代替电脑与完整产品验收，详细证据见[实施进度](docs/multi-user-progress.md)。

镜像已包含[固定非 root 能力探针](internal/computerprobe/README.md)，实际终端、文件及网络检查在受限 Linux 容器通过；代理调用、持久消费、宿主服务组生命周期、重启回收与实际基础证据汇总已接通。浏览器能力仍未确认，不能完整就绪；真实内核正向及生产部署验收仍须完成。

群组文件管理、附件协议、容量限制及升级回滚说明见[群组资料库](docs/group-library.md)。

## 构建与检查

Go 版本以 [go.mod](go.mod) 为准。服务程序部署于 Linux，API 使用固定非 root 身份；开发机可以执行模块测试与交叉构建。管理平台从仓库 `admin/` 构建，通用视觉样式与 Electron 客户端共享 `ui/`。

```sh
# 在 server/ 中执行
make lint check test
make build
make release RELEASE_ARGS='--version 0.2.0-rc.1 --output /绝对路径/tombot-0.2.0-rc.1.tar.gz'

# 在 admin/ 中执行
npm ci
npm run check
```

PostgreSQL 集成测试会启动隔离的临时实例，不连接已有业务库。通过 `TOM_TEST_POSTGRES_BIN` 指定 `initdb`、`pg_ctl`、`postgres` 所在目录，或放入 `PATH`。检查需要本机临时套接字、HTTPS 监听及进程权限。

`make release` 现在从当前工作树的独立快照构建完整双架构发布包，包含管理平台、宿主程序、电脑镜像构建上下文和 systemd 单元，不再输出分散的 `bin/*-linux-*` 文件。打包后逐文件复验并原子发布，拒绝覆盖旧产物；`make verify-release RELEASE_VERIFY_ARGS='--archive /发布包路径 --sha256 受信发布摘要'` 提供只读核验。构建、安装映射和回滚边界见[发布包说明](deploy/RELEASE.md)。产物完整不代表真实部署或电脑能力验收通过。

## 新入口与部署边界

使用 `tomnode configure` 从单份参数生成服务、节点、Docker、镜像配置及节点安装参数；`tomnode check-deployment` 可核对整套部署关系。节点启动只执行 `tomnode check` 对应的本机检查，不要求数据库凭据。格式版本 7 发布包只交付合并后的节点入口；这些检查不替代真实电脑能力验收。

`TOM_SERVER_CONFIG` 指向受保护的 JSON 配置文件，配置结构、凭据文件要求和启动顺序见[应用组合说明](internal/serverapp/README.md)。`tomd` 强制 HTTPS，不创建默认账户、模型或模拟节点。管理平台位于 `/admin/`，API 位于 `/api/v2/`。

数据库由 `tomd` 在启动时自动初始化和升级，不再构建独立迁移程序。通过 `database.migration_dsn_file` 引用迁移账户的私有连接文件；迁移成功并关闭该连接后，才打开四个用途分离的最小权限业务连接并监听端口。配置、并发与失败语义及升级回滚见[应用组合说明](internal/serverapp/README.md)。

节点完整解压配套离线包后执行 `sudo ./tomnode install`，粘贴接入码并确认节点地址，自动导入镜像、生成配置和证书、安装服务并设置开机启动，见[节点离线镜像安装](deploy/NODE.md)。`tomnode template` 仅用于输出可选差异参数。`tomnode start` 按节点配置检查 → Docker 与磁盘工具准备 → 固定镜像准备 → 节点恢复顺序运行，失败即停止。`tomnode systemd` 输出内置单元；节点启动不依赖 API 文件或系统账户，API 与节点分别启动。完整电脑能力与首次 Ubuntu 安装仍须联合验收。

[自动引导](internal/dockerbootstrap/README.md)同时补齐宿主磁盘工具：清单必须提供 `e2fsprogs`、`util-linux`、`mount` 的准确包版本；已有 Docker 也会核验，缺包自动安装，版本或包状态异常明确失败。安装后和每次重启都复查实际包及六个固定工具，不用 Docker 启动成功替代宿主依赖检查。

`/health/live` 表示 API 进程可响应；`/health/ready` 只检查运行状态与四个数据库连接，返回 `scope: api`，不声明用户电脑、模型供应商或节点能力已就绪。

## 数据和回滚

从空 PostgreSQL 与全新用户卷开始，不导入旧数据，不保留 `/api/v1`、单用户登录、客户端模型配置或宿主 Agent 回退。`tomd doctor`、`tomd backup`、`tomd restore` 和旧环境变量已不再是服务入口。

历史迁移文件仍保持不可变。旧功能实现及历史文档尚在逐域替换，不代表当前入口支持这些能力。旧客户端不能连接新入口；成套切换及回滚必须遵守[应用组合说明](internal/serverapp/README.md)中的停单、排空与数据保留边界。新的 PostgreSQL 与用户卷联合备份恢复尚待实现及验收。

管理平台已内置于 `tomd`：在 `server/` 执行 `make build` 会先构建前端再编译服务端。部署只需复制 `tomd`，继续使用外部服务配置、数据库和证书，访问 `/admin/`；不再配置或复制前端目录。旧配置须移除 `admin_directory`，升级与回滚见[发布说明](deploy/RELEASE.md)。
