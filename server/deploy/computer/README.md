# 远程电脑专用镜像

本镜像用于一用户一容器的远程电脑。基础系统固定为 **Debian 13（Trixie）**，桌面为 **XFCE**，窗口管理器为 **xfwm4**，显示协议为 **X11**，每个 Bot 对外桌面固定 **1280×800、24 位色深**。基础镜像使用官方多架构摘要，支持构建 `linux/amd64` 和 `linux/arm64`。

## 默认工具

### 语言和运行时

| 工具      | 配置                                              |
| --------- | ------------------------------------------------- |
| Python    | 3.13，提供 `python`、`python3`、pip、venv 和 uv   |
| Node.js   | 24.22.2，包含 npm；另装 pnpm、bun                 |
| Go        | 1.27.4，使用固定工具链，不自动下载其他版本        |
| Rust      | Debian 软件源提供的 `rustc`、`cargo`              |
| Perl、gcc | 同时提供编译所需的 make、标准库头文件、pkg-config |

### 常用命令行工具

| 类别           | 工具                                                   |
| -------------- | ------------------------------------------------------ |
| 代码管理       | git、GitHub CLI（`gh`）                                |
| 下载和文本处理 | curl、wget、jq、ripgrep（`rg`）                        |
| 终端会话       | tmux                                                   |
| 远程连接和加密 | ssh、scp、openssl；不安装 SSH 服务端                   |
| 多媒体和文档   | ffmpeg、pdftotext（由 `poppler-utils` 提供）           |
| 数据库客户端   | PostgreSQL 客户端 `psql`；不在用户电脑内启动数据库服务 |

### 桌面软件

- Google Chrome Stable，使用 Google 官方安装包。
- LibreOffice Writer、Calc、Impress、Draw，包含中文界面支持。
- XFCE 终端、Thunar 文件管理器及 Mousepad 文本编辑器。
- 中文字体、Emoji 字体及中文 UTF-8 locale。

非 Debian 工具的精确版本、amd64/arm64 下载地址和 SHA-256 记录在 [tools.lock.json](tools.lock.json)，构建时验证摘要后安装。Debian 包来自 Trixie 及其安全更新源，不把整套软件冻结在已过期的安全更新上。构建结果保存 `/usr/share/tombot/packages.tsv` 和 `tools-versions.json`；实际部署固定**最终镜像摘要**，不能仅依赖可变标签。pnpm 使用官方原生发布包，不依赖安装脚本临时下载二进制；工具账户凭据、网站 Cookie 和 SSH 私钥不写入镜像。

## 构建与验收

在 `server` 目录执行：

```sh
make computer-image
make test-computer-image
```

默认构建当前机器的 amd64 或 arm64 架构，只加载到本地 Docker，不推送镜像。构建上下文只含镜像配置与本次编译的 `computer-agent`、`computer-task`、`computer-probe`、`bot-agent`，不发送整个仓库、用户文件或本地环境配置。也可以指定目标：

```sh
make computer-image IMAGE_ARGS="--architecture amd64 --tag tombot-computer:trixie"
make test-computer-image IMAGE_TEST_ARGS="--image tombot-computer:trixie"
```

构建需要本地 Go、Docker Buildx，以及访问官方 Debian、Node.js、Go、Google 和 GitHub 下载源。项目程序仍按 `go.mod` 构建；用户电脑内的 Go 工具链为 1.27，两者用途不同。联网构建有超时和有限重试，摘要不匹配、版本不符或下载失败均中止，不切换为缺少工具的镜像。

验收脚本创建全新临时卷和容器：禁网、只读根目录、使用下述默认拒绝的专用 seccomp 配置、不可提权、限定 CPU/内存/进程，仅添加与电脑代理相同的六项 capability。不挂载 Docker socket、宿主 cgroup 或已有用户卷。测试验证工具版本、Python 虚拟环境、Go/Rust/C 编译运行、LibreOffice 转 PDF 与读取、Chrome 真实页面执行及沙箱状态、两个独立图形桌面、真实 RFB 输入、共享文件和工具、跨桌面连接拒绝、固定截图尺寸以及定向关闭。结束后删除全部测试容器和临时卷，输出截图、工具清单和 Chrome 沙箱报告。

### 当前持久目录布局验证

2026 年 11 月 12 日构建并验收本地 `tombot-computer:storage-layout-20261112`，linux/arm64 镜像 ID 为 `sha256:129ce0dd86d80a702276742a15e161db1ac0d3e41f15196a93c5149190ba4f7b`，带布局 2 标签。真实 Docker 临时卷使用两个 `volume-subpath`，验证同一磁盘、业务文件与工具共享、两个 Bot 无权读取或改写浏览器目录，以及移除并重建容器后两类内容保留。此处保存的是测试内容，不是实际账号凭据；Chrome profile 生产监督和登录态联合恢复仍待后续接通。

全部工具、编译、PDF、Chrome 沙箱、双 1280×800 XFCE 桌面、独立输入、权限拒绝和定向关闭回归通过。日志为 `/private/tmp/tombot-storage-layout-image-verify.log`，截图及工具、沙箱报告为 `/private/tmp/tombot-storage-layout-image-evidence`，两张桌面截图已复核。另以正式 Go 布局函数在受限 Linux 容器验证实际 UID 30000 和 GID 20000，见 `/private/tmp/tombot-storage-layout-linux.log`。这些测试未挂载宿主 cgroup、loop 或已有用户卷，不证明实际硬配额和完整开户已验收。

### 前期验证记录

2026 年 11 月 12 日，探针代理协议及持久回收监督已加入最终 **linux/arm64** 镜像 `tombot-computer:probe-supervisor-final-20261112`，ID 为 `sha256:3d7e6960f34fd1c724ef50b5608887e155a3d5b0ca207e8484b5e4a927899dcb`。正式入口验证同代次未关闭探针在监听前被拒绝，错误日志包含电脑标识；实际探针运行器拒绝伪造入组入口，双实例拒绝、正常停服与监听释放回归通过。

完整镜像验收通过全部工具、编译/PDF、Chrome 沙箱、双 XFCE 桌面、真实输入、共享文件与工具、跨桌面拒绝和定向关闭；两张 1280×800 中文截图已复核。产物为 `/private/tmp/tombot-probe-supervisor-final-image-verification-20261112`，临时容器已回收。服务端全量检查、Linux 双架构构建与专属静态检查通过；本轮未实际构建运行 amd64 镜像。

代理新增 `probe` 动作和版本 1 的私有 `probe.json` 消费屏障，配置仍为版本 3。宿主节点探针调用、持久版本与服务组最终回收尚待接通，真实服务成功入组及全链路能力仍未验收。不能据此标记电脑就绪；节点、代理、探针和镜像须成套发布或回滚，保留关闭屏障，详见[代理探针监督](../../internal/computeragent/README.md#容器基础探针监督)及[实施进度](../../docs/multi-user-progress.md)。

### 先前固定探针程序阶段记录

2026 年 11 月 11 日新增固定能力探针后，已构建并完整验收 **linux/arm64** 镜像 `tombot-computer:probe-20261111`，ID 为 `sha256:efa17827ede965dd4fc1a64955c1419aed0711d0611037824fc38ac240041480`。镜像内四个 Go 程序均来自本次纯 Go 编译，没有替换原有运行容器或推送镜像。Linux amd64/arm64 全包构建及相关静态检查通过，本轮未构建运行 amd64 镜像。

镜像验收覆盖全部工具、编译与 PDF、Chrome 沙箱、双 XFCE 桌面、真实输入、共享文件及工具、跨桌面拒绝和定向关闭，并验证没有封存授权及实际服务组的 root/非 root 探针都以 125 退出且不输出成功证据。两张 1280×800 中文截图已复核，产物为 `/private/tmp/tombot-probe-image-verification-20261111`，临时容器已回收。

固定探针的真实 PTY、文件读写同步与回收、文件系统数值和网络检查，以及任务引导回归分别在独立受限容器通过；全量 `make lint check test` 通过。探针尚未接入代理协议、持久服务监督和节点就绪组合，不代表真实成功入组与整组回收、用户卷硬容量、代理出网或共享浏览器联合验收完成。接口、发布与回滚要求见[能力探针](../../internal/computerprobe/README.md)，详细日志见[实施进度](../../docs/multi-user-progress.md)。

### 先前桌面输入阶段记录

2026 年 11 月 11 日已重建并验收本地 `tombot-computer:trixie`，架构为 **linux/arm64**，最终镜像 ID 为 `sha256:9e9262872685734b181dc611d04deb81569f82a68cee02b3fef8a411a5cdf46a`。包含独立 Bot Agent、多桌面监督、版本 2 任务引导、版本 3 代理配置、受管终端、环回出网转接及本轮新增的桌面截图和完整键鼠手势。amd64 全包编译通过，本轮未实际构建及运行 amd64 镜像。

完整镜像验收通过全部预装工具、Python 环境、Go/Rust/C 编译、LibreOffice 转 PDF、Chrome 沙箱、双 XFCE 桌面、真实远程输入、共享文件及工具、跨桌面拒绝与定向关闭。两张 1280×800 中文桌面截图已复核，报告位于 `/private/tmp/tombot-desktop-io-image-verification-20261111`；临时容器已确认清理。

`make lint check test` 通过格式、静态检查、构建、22 项 Python 测试和全部 Go 竞态检查，相关 Linux 专属静态检查通过。代理完整集成测试验证既有终端、图形、独立 Agent 行为，并新增两个真实 XFCE 桌面的 PNG 分块、中文输入、鼠标独立性和单桌面撤销。最终镜像入口验证出网端点缺失时返回 502、双实例拒绝、配置校验及退出释放监听。本次不覆盖生产业务 cgroup 回收、宿主出网端点组合、共享浏览器窗口网关、主服务开户或客户端联合验收。此前阶段记录见[实施进度](../../docs/multi-user-progress.md)。

## 图形会话与共享目录

`computer-agent` 是容器正式入口。镜像不内置账户配置，也不自动启动一个供全部 Bot 共用的 root 桌面；缺少节点提供的代理配置时明确失败。每个 Bot 由控制层分配独立 Linux UID、同一用户内的公共文件 GID、显示编号和桌面 cgroup，再以降权身份启动 `/usr/lib/tombot/desktop.py`。

服务端已通过[桌面身份模块](../../internal/computerdesktop/README.md)在创建机器人时登记稳定身份：显示编号为 1–4095，UID 为 100000 加显示编号，同一用户容器的公共 GID 为 20000。身份记录与机器人共同提交；节点与代理现已接通图形启动、就绪查询、截图及完整输入手势，Bot Agent 独立进程与分块中继已接通。公开用户桌面网关、模型视觉工具及真实 cgroup 联合验收仍须完成。

| 路径                                          | 用途与权限                                                                                                        |
| --------------------------------------------- | ----------------------------------------------------------------------------------------------------------------- |
| `/workspace`                                  | 用户持久业务目录；所有 Bot 共用 HOME、文件与安装工具，公共组可读写，目录使用 setgid                               |
| `/workspace/tools`                            | 用户自行安装的共享工具及包管理目录；镜像内系统工具位于只读系统目录                                                |
| `/workspace/.tmp`                             | 共享业务临时文件，允许执行编译产物；不保存平台认证材料                                                            |
| `/run/bot-desktops`                           | 控制程序建立，root 所有、0711；Bot 不可列出目录或创建别人的桌面                                                   |
| `/run/bot-desktops/<session_id>`              | 本桌面 UID 所有、0700；每次执行授权使用新目录，包含独立 Xauthority、ICEauthority、D-Bus、图形配置、日志及就绪记录 |
| `/run/bot-desktops/<session_id>/desktop.sock` | 0600 的私有 RFB Unix socket；由已认证网关接入                                                                     |
| `/run/tombot-control`                         | 原有 root 私有控制目录；与业务共享目录分开                                                                        |

Python 项目通过 venv 或 uv 在共享工作目录建立环境；不覆盖 Debian 管理的系统 Python。npm 全局前缀、pnpm、bun、uv、Go 和 Cargo 的用户工具目录均指向 `/workspace/tools`，不同 Bot 可以读取和调用；图形配置与认证目录不承担业务文件隔离。

上述路径与图形环境定义在[共享环境模板](../../internal/computer/desktop-environment.json)，同一文件嵌入 Go 任务引导并打包给 Python 桌面启动器，避免终端与 XFCE 的工具路径、UID、显示编号或会话总线不一致。任务请求使用版本 2，UID/GID 取自已授权桌面，降权后才进入业务工作目录；普通新文件使用 `umask 0007`。控制层准备 setgid 共享目录时必须保留公共组身份并复验权限；不能假设 root 在缺少 FSETID capability 时仍能保留任意组的 setgid 位。

共享环境同时声明 `http_proxy`、`https_proxy`、`HTTP_PROXY` 和 `HTTPS_PROXY`，固定指向电脑内的 `http://127.0.0.1:3128`；`NO_PROXY`/`no_proxy` 只包含环回目标。正式容器采用无路由网络，由版本 3 代理转接到 root 私有宿主端点；不遵循代理的程序不能直接联网。Chrome 共享浏览器服务必须显式设置代理，SSH 等工具需要其自身支持的代理方式；不启用通用直连旁路。具体边界见[出网模块](../../internal/egressproxy/README.md)。

镜像布局标识为 `io.tombot.computer.storage-layout=2`。节点预先在同一块 ext4 磁盘创建 `workspace` 与 `browser` 子目录，通过同一 Docker 卷的两个子路径分别挂载到 `/workspace` 和 `/var/lib/tombot-browser`。前者为 root:20000、2770，全部 Bot 共享；后者为 30000:20000、0700，只供用户级 Chrome 服务访问，Bot 通过窗口服务共享登录状态。磁盘根目录及 `lost+found` 不进入容器；业务进程不能通过共享父目录替换 profile。

正式代理入口在接单前调用镜像内 `prepare.py`，以独立 root UID/公共 GID 子进程核验两处挂载、所属身份、权限及同一文件系统的不同目录，再准备工具目录、`/run/bot-desktops` 以及 root:root、1777 的 X11/ICE socket 目录。缺少浏览器挂载、旧布局或权限漂移明确失败，既不递归改权限也不转换旧目录。业务工具目录创建中断遗留的空目录允许继续授权，随后复验 setgid 与实际所有权。新布局需与存储记录版本 3 的节点成套部署，详见[存储与回滚说明](../../internal/dockernode/storage.md)。共享 Chrome 监督已接入节点和代理，完整浏览器工具与能力探针仍待接通；目录验证本身不授予完整浏览器能力。

节点已对齐 `/tmp` 1777、`/run` 0755、共享内存容量和专用 seccomp，并在对账时拒绝漂移；运行空间与共享内存容量必须由节点配置明确提供。代理现已创建每次执行的私有会话目录，终端启动、读写与尺寸操作已接入节点协议。XFCE 消费相同的身份文件；图形启动通过固定引导程序进入当前执行组，代理另核验私有 socket 的对端、实际分辨率和核心进程生存状态；主服务、屏幕输入和真实 cgroup 执行仍须联合验收。

桌面使用 TigerVNC 的 `Xtigervnc` 作为 X11 显示服务器，同时提供远程画面。X11 TCP 和 RFB TCP 均关闭，不发布 5900/6000 系列端口；X11 使用随机生成且不进入命令行的认证材料。RFB 仅允许通过私有 Unix socket 访问，socket 上不重复设置静态密码，公网鉴权与桌面控制权由上游网关承担。客户端不能请求动态调整分辨率。各会话具有独立 XFCE、xfwm4、D-Bus 和图形认证；业务文件不按 Bot 强制隔离。

会话不接管容器总入口，不自行创建 UID 或扩大权限；重复启动会被锁和已存在的状态拒绝。重启时由控制层确认旧桌面 cgroup 清空，再为下一代次创建空白运行目录；不能复用未清理的认证与就绪文件。图形核心进程异常时返回失败并撤销就绪标记。停止会话会关闭受监督的进程组；用户进程可能自行创建新组，因此完整任务后代回收仍必须使用节点的桌面 cgroup，不能以本镜像的进程组清理替代。XFCE 不自动启动 GPG/SSH 凭据代理，避免争用共享 HOME 中的单例；SSH/scp 客户端保留。

## 浏览器与接入边界

Google Chrome 已列为必装桌面软件。远程电脑的目标是让用户直接操作真实 XFCE 桌面中的原生 Chrome、终端和 Thunar，不再开发浏览器窗口客户端。用户级共享 Chrome 服务与持久配置目前用于自动化；登录、退出和账号切换影响同一用户所有 Bot。原生 Chrome 同时满足独立桌面和完整共享账号状态的方案尚待单独设计验证，不能让多个 Chrome 实例共写 `user-data-dir`，也不能把独立配置目录当作共享登录态。镜像安装原生软件不代表远程画面、人工接管或原生 Chrome 的多桌面展示已经实现，详细边界见[改造方案](../../docs/multi-user-refactor.md#共享登录态与独立浏览器窗口)。

Chrome 保留原生沙箱，不添加 `--no-sandbox`。容器必须使用本目录的 [seccomp.json](seccomp.json)，即 `--security-opt seccomp=/绝对路径/seccomp.json`，同时保留 `no-new-privileges` 及现有六项 capability。配置沿用固定提交的 Docker 默认拒绝策略，仅额外允许 `clone`、`setns`、`unshare`、`chroot`，使非 root Chrome 能在自身用户命名空间中完成沙箱初始化；内核仍检查命名空间内的权限，普通 Bot 不能创建外层挂载命名空间。不授予 `SYS_ADMIN` 或 `SYS_CHROOT` capability，不使用 privileged。单元测试校验除此之外的规则与上游基础策略一致；来源摘要记录在 [seccomp-source.json](seccomp-source.json)，附带 [Apache-2.0 许可](seccomp.LICENSE)。

使用原始 Docker 默认 profile 时，实际 Chrome 启动会因命名空间调用被拒绝而失败；镜像内放置 JSON 文件不会自动让 Docker 应用它。因此节点必须与镜像一起发布运行配置。节点现已直接嵌入同一份 JSON 并交给 Docker API，无需现场再选择策略文件。浏览器验收通过私有 CDP 管道执行页面脚本，并断言 `chrome://sandbox` 中 PID、网络命名空间及 Seccomp-BPF 均已启用。主服务接通后仍须验收共享登录态、出网和完整窗口交互，不能用单浏览器沙箱报告代替这些行为。[Playwright 官方 Docker 文档](https://playwright.dev/docs/docker)说明了非 root 浏览器的命名空间 seccomp 配置需求；这里另外允许 `chroot`，以适配本项目去除 `SYS_CHROOT` capability 后的受限容器。

## 升级、部署与回滚

工具版本升级先更新锁定清单，重新构建并执行验收，再发布新镜像摘要。已运行容器的系统根目录只读，系统包更新通过更换镜像完成；用户文件和自行安装的工具保留在持久卷中。

切换镜像前停止新任务、排空所有桌面及共享浏览器、备份用户卷，再停止旧容器并启动新实例；同一卷一次只允许一个电脑实例写入。回滚使用上一份经过验收的镜像摘要，必要时恢复与其匹配的卷快照。不迁移旧单用户数据库，不删除或改写历史迁移脚本，不在本轮构建中改动其他 Docker 工作负载。

官方依据：[Debian Trixie](https://www.debian.org/releases/trixie/)、[TigerVNC Xvnc 参数](https://tigervnc.org/doc/Xvnc.html)、[Go 发布](https://go.dev/dl/)、[Node.js 24 下载](https://nodejs.org/download/release/latest-v24.x/)、[uv 安装](https://docs.astral.sh/uv/getting-started/installation/)、[bun 安装](https://bun.com/docs/installation)、[Google Chrome 安装](https://support.google.com/chrome/answer/95346?co=GENIE.Platform%3DDesktop&hl=en)。

## Bot Agent 运行位置

同一用户的 Bot 继续共用本容器，每个 Bot 需要独立的非 root Agent 进程和桌面。镜像的 `computer-agent` 是电脑基础设施监督器；新增 `/usr/lib/tombot/bot-agent` 承载模型与工具循环，使用私有标准流连接受限中继。入口拒绝 root、命令行覆盖和普通文件/终端作为协议通道；非 Linux 平台不能运行。

两个真实降权进程已验证独立循环，一个中断后另一个继续运行；信号取消能够唤醒等待回包的读取。测试启动器直接降权，没有代替生产任务引导与 cgroup 验收。Worker 分发、电脑代理监督及持久模型/操作中继已接通模块接口，Worker 不再调用同进程循环；现有主入口、业务工具与真实 cgroup 联合验收仍待完成。供应商配置和凭据继续只在服务端保存，不能让容器不可用时回退到宿主执行。完整协议及限制见 [Bot Agent 模块](../../internal/botagent/README.md)。
