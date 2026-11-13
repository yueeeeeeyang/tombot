# 多用户重构实施进度

更新日期：2026 年 11 月 12 日。目标以 [多用户服务端改造方案](multi-user-refactor.md) 为准，整体改造尚未完成。

## 当前实施顺序与暂缓范围

按用户要求，依次完成：生产部署链路、共享浏览器、业务功能补齐、管理平台补齐、客户端切换、旧实现清理、最终交付验收。生产部署的真实宿主安装、挂载与开户验收仍未完成，门禁保持关闭。

用户通过远程桌面示例明确：访问 Bot 的真实 Linux 桌面，直接操作原生 Chrome、XFCE 终端和 Thunar，支持用户接管；不开发自有浏览器窗口，也不能把仅有浏览器工具调用当作远程电脑。远程电脑仍按此前安排后续单独设计。每用户一个容器、每 Bot 独立 Agent 与桌面，以及共享文件、工具和浏览器账号的要求保留。

## 当前进展：Agent 本地浏览器执行

`computer_browser` 已注册到容器内 Agent，支持页面管理、导航、输入、截图和对话框。服务端只登记与确认操作，注册对象没有执行方法；Chrome 动作通过当前会话的本机 Unix 入口执行，没有新增浏览器逐动作节点 RPC。协议 3 增加本地授权、结果报告与 32 KiB 图片分块，未知结果不重放，截图与操作回执原子保存。

窗口入口现校验当前运行 Agent 所在的 `shell` 子组，并保留 UID/GID、完整会话、租约及原实例复验。所有 Bot 仍共享账号状态，各自只能操作所属页面。任务完成前拒绝遗留等待、在途或未知动作。此项不修改 SQL 迁移和持久记录结构；组件须排空后成套升级与回滚。

本轮新增协议、工具参数、归属、图片分块及 PostgreSQL 回归。真实容器测试发现并修复导航后立即截图的渲染页激活时序问题，只对明确的页面尚未活动错误有界重取只读画面，副作用不重放；另修复启动完成与取消通知调度之间可能漏撤销的竞争，确定性回归的修复前证据为 `/private/tmp/tombot-agent-local-cancel-race-before.log`。

最终 `make lint check test` 全部通过，含 33 项 Python、格式、静态检查、全包构建与全部 Go 竞态测试，见 `/private/tmp/tombot-agent-local-verified-checks.log`。Linux amd64 全包构建、arm64 带浏览器集成标签静态检查通过。最终镜像内的四包 Linux 竞态、实际降权 Agent 进程协议及真实 Chrome 本地客户端测试通过，见 `/private/tmp/tombot-agent-local-final-linux.log`：两个 Bot 独立创建、导航、截图、检查和关闭所属页面，模型及账本使用明确测试替身；真正执行浏览器动作的是镜像内 `bot-agent`，服务端执行入口调用即失败。另验证共享站点状态与跨 Bot 页面拒绝。这里的 cgroup 正向授权仍是测试夹具，不替代生产宿主验收。

本地 arm64 镜像为 `tombot-computer:agent-local-browser-20261112`，ID `sha256:5220d275629f831b84538e0cacdbc70d6affc1f07e8d900ab0d2dbcdca0e4338`。工具与编译、PDF、Chrome 沙箱、双 XFCE 桌面、远程输入、共享文件和浏览器目录重建保留均通过，见 `/private/tmp/tombot-agent-local-image-final-build.log`、`/private/tmp/tombot-agent-local-image-verify.log`；产物为 `/private/tmp/tombot-agent-local-image-evidence`。临时容器和卷已确认回收，未推送镜像、部署或更新旧开发发布包。协议 3 的升级、回滚和性能影响见[发布说明](../deploy/RELEASE.md)。

原生远程桌面、上传下载、完整能力探针、生产宿主与开户验收，以及后续业务、管理平台、客户端、旧实现清理和最终验收仍未完成，生产就绪门禁继续关闭。

## 前一阶段：页面对话框与执行方向调整

本轮实测复现了 Chrome 输入动作等待页面对话框答复、导致顺序调用者无法进入答复步骤的问题，失败证据为 `/private/tmp/tombot-browser-protocol-dialog.log`。已修复为显式动作状态：遇到对话框先返回 `waiting_dialog`，调用方随后答复并查询 `completed`；重复动作只查询同一回执，在途新动作拒绝，未知动作不重发。执行和对话框等待分别累计预算，连续对话框不重置额度；超时封闭当前窗口，撤销和关闭等待在途动作回收，其他 Bot 不受影响。

本地 Unix 浏览器接口升级为 `/v2`，旧 `/v1` 拒绝。新增必填 `browser.limits.dialog_timeout_ms`，范围 1000–600000，部署示例为 300000。未新增 SQL，旧授权缺少预算时拒绝，不添加兼容默认值；成套部署、阶段环境重建与回滚要求见[发布说明](../deploy/RELEASE.md)。首次组合测试因使用旧镜像代理、不识别新授权字段而明确失败，见 `/private/tmp/tombot-browser-local-controller.log`；重新构建匹配镜像后完整组合验证通过，未弱化该检查。

`make lint check test` 与 Linux amd64 全包构建通过，包含 33 项 Python 测试、格式、静态检查、构建及全部 Go 竞态测试，见 `/private/tmp/tombot-browser-local-checks.log`。当前代理入口、真实 Chrome、双桌面与节点绑定组合通过，见 `/private/tmp/tombot-browser-local-controller-final.log`；真实 Chrome 的对话框生命周期和六个相关包的 Linux 竞态通过，见 `/private/tmp/tombot-browser-local-race.log`。预算单测使用虚拟时间验证暂停、恢复和累计额度，真实 Chrome 回归另验证原请求结束后继续等待、超时拒绝答复及单 Bot 撤销。

本地 arm64 镜像为 `tombot-computer:local-browser-dialog-20261112`，ID `sha256:96bcdf362cbb97f67b33f91dd4b78147a84c9a28e4b37ebb19764849e6d7c686`。构建与完整镜像验收通过，日志为 `/private/tmp/tombot-browser-local-build.log`、`/private/tmp/tombot-browser-local-image-verify.log`，产物为 `/private/tmp/tombot-browser-local-image-evidence`。镜像包含新的本地浏览器服务行为，尚不包含完成后的 Agent 本地工具接线；未推送、部署或更新开发发布包。原生 Chrome 多桌面展示、用户接管、生产宿主正向验收及后续业务阶段仍未完成，生产就绪门禁保持关闭。

## 当前调整：直接操作真实应用

已移除自研 GTK 窗口包、镜像依赖、打包清单、专属心跳与桌面启动接线；与该实现绑定的 GUI 测试随废弃功能删除，图形会话退出监督改为验证实际桌面进程的零码及异常退出，其他 RFB、真实 Chrome、权限和故障回归保留。尚未完成的自研文件选择与上传增量已撤回，未留在发布链路中。

桌面基础与共享 Chrome 自动化服务继续保留。后者当前使用无头模式，原生 Chrome 如何在多个独立桌面上同时展示并实时共享完整账号状态尚未解决，须在远程电脑设计中验证，不用 Cookie 同步、共写 profile 或自研网页画布替代。历史 GTK 镜像和日志仅证明已取消的实现，不能作为最新目标的验收证据。

本轮 `make lint check test` 通过：33 项 Python 测试、格式、静态检查、全包构建及全部 Go 竞态测试，日志为 `/private/tmp/tombot-native-desktop-scope-checks.log`。Linux amd64 全包构建、arm64 集成测试编译通过；六个相关包的 Linux 竞态通过，日志为 `/private/tmp/tombot-native-desktop-linux.log`。取消的是自研界面的专属测试；保留的 Chrome 键盘行为另新增真实浏览器回归，覆盖中文替换、Ctrl+A、Tab、Enter 按钮激活及重复回执不再次触发。

新基础镜像为 `tombot-computer:native-apps-base-20261112`，arm64，ID `sha256:f71a4b67ad6d42d7c9e034984de711b245ed3f8ee097a01d4be823d1322a6a7c`。构建与完整镜像验收通过，日志分别为 `/private/tmp/tombot-native-apps-build.log`、`/private/tmp/tombot-native-apps-image-verify.log`，产物目录为 `/private/tmp/tombot-native-apps-image-evidence`。实际双 XFCE、桌面退出与跨身份拒绝、共享 Chrome 及账号持久化、页面回收、Chrome 原生键盘事件和节点 Unix 绑定测试通过，见 `/private/tmp/tombot-native-desktop-controller-final.log`。旧浏览器测试夹具使用独立 GID，在正式共享卷权限下启动失败；夹具已改用与正式服务一致的公共 GID，没有放宽卷权限或 Chrome 沙箱。

临时容器与卷已核验回收。新镜像仅验证原生应用与桌面基础，不代表原生 Chrome 跨桌面呈现或用户远程控制已实现；未推送、部署或更新开发发布包，生产就绪门禁继续关闭。

## 代理内部直接调用共享 Chrome

新增代理内部的 Go 调用入口，复用原共享 Chrome 执行所属页面列表、创建、动作、截图、对话框回复和关闭；不依赖自研浏览器界面。入口在桌面锁内只取得原会话绑定，网页动作在锁外运行，避免授权复验等待自身持有的锁。

调用前后均核验完整会话及原实例。撤销取消在途请求，迟到页面或截图结果被丢弃；动作开始后的取消、超时和授权失效统一返回结果未知，不同时返回可被误解为未执行的拒绝错误。页面回收等待网关和直接调用均退出；忽略取消的调用超过清理期限时触发独立致命故障，不提前释放页面或自动重放动作。

最终 `make lint check test` 与 Linux amd64 全包构建通过，包含 33 项 Python 测试、格式、静态检查、构建与全部 Go 竞态测试，日志为 `/private/tmp/tombot-browser-direct-final-checks.log`。Linux 六个相关包的竞态通过，包含无需关闭竞态检测的阻塞调用回收超时和回执复验期间取消回归，见 `/private/tmp/tombot-browser-direct-final-linux.log`。实际 Chrome 的中文输入、截图变化、跨 Bot 拒绝、原实例绑定及撤销屏障验证通过，见 `/private/tmp/tombot-browser-direct-final-controller.log`；测试使用当前源码生成的 arm64 测试程序与既有基础镜像。

这些 Go 入口尚未接入 root 控制协议、节点 API 和容器 Agent 的浏览器工具；上传下载、对话框异步处理、截图分块与联合能力探针仍待完成，不能当作完整浏览器交付。本次未修改 SQL、持久记录或线上协议版本，未重新构建镜像、更新开发发布包或部署。当前 `native-apps-base-20261112` 镜像中的代理不包含这些新入口，成套发布和回滚要求见[代理说明](../internal/computeragent/README.md#代理内部的直接浏览器调用)。

远程电脑设计同步明确：用户接管时，同一 Bot 的桌面键鼠和浏览器自动化都停止接收新操作；在途操作先确认结束或报告结果未知。原生应用画面、用户接管和共享账号的多桌面原生 Chrome 呈现仍按既定安排单独设计，生产就绪门禁保持关闭。

## 已取消的自研窗口方案（历史验证）

以下保留变更前的验证记录，仅供追溯，不属于当前交付方案。


新增固定 GTK 3/PyGObject 客户端，提供地址栏、前进后退、刷新、新建关闭页面、所属页面分页、画面、中文输入、基本组合键、左键、双击、滚轮和网页对话框。每个 Bot 的客户端由正式桌面启动器放入自己的 `browser` 子组，XFCE 保持在 `shell` 子组；只有 RFB 与实际窗口进程身份、界面心跳均通过才报告桌面就绪。两个进程均纳入退出监督和回收，没有新增 SQL 或持久记录版本。

通信先验证 root Unix 对端，普通动作与对话框回复使用独立有界队列，未知写回执冻结窗口且不重发。客户端回归覆盖迟到列表、未知创建、畸形响应、操作序号、传输总期限和已排队操作拒绝。真实浏览器验收还修复 Enter 缺少字符事件、无法触发网页按钮默认行为的问题。

最终镜像自身的双桌面测试通过八轮交替中文替换、地址栏、页面新建关闭、历史导航和刷新、独立对话框、鼠标坐标及双击计数，同时核对本地窗口标题和共享账号保留。验收使用 Agent 的完整 RFB 键盘链路；诊断中发现混用 XTEST/RFB 会触发动态键位切换，未保留无效的延时尝试，也未把外部输入源混用标为已支持。日志为 `/private/tmp/tombot-window-client-final-controller.log`，实际窗口截图为 `/private/tmp/tombot-window-client-evidence/window-client-1.png`、`window-client-2.png`，两张均已查看。

`make lint check test` 通过 49 项 Python、格式、静态检查、全包构建和全部 Go 竞态测试，见 `/private/tmp/tombot-window-client-checks.log`。Linux amd64 全包构建、arm64 集成标签静态检查，以及六个相关包的 Linux 竞态与节点 Unix 绑定通过，见 `/private/tmp/tombot-window-client-linux.log`。

最终本地 arm64 镜像为 `tombot-computer:window-client-verified-20261112`，ID `sha256:e03d3bfadd1dda49706d1ce93fc12ee5845163c39cd1a334605e9c44e0f5f89d`。构建与完整镜像验收日志为 `/private/tmp/tombot-window-client-final-build.log`、`/private/tmp/tombot-window-client-image-verify.log`；工具与编译、PDF、Chrome 沙箱、双 XFCE、共享文件和浏览器目录重建保留全部通过，产物为 `/private/tmp/tombot-window-client-image-evidence`。没有推送、部署或更新旧开发包；当前成套升级和回滚要求见[原生应用与远程桌面边界](../internal/computeragent/README.md#原生应用与远程桌面边界)。

上传下载、右键/拖动等其余交互、完整浏览器能力探针及生产宿主正向验收仍未完成，账户就绪继续关闭；此次受限容器的资源组成功路径仍使用明确夹具，不能替代生产 cgroup 入组证明。后续业务、管理平台、客户端切换与旧实现清理尚未完成；公开远程电脑交互继续暂缓。

## 会话窗口入口与独立页面回收（上一阶段）

正式代理资源工厂已在发放执行授权前创建每会话 `browser.sock`，入口固定原 Chrome 实例及完整 Bot 执行身份。每次请求核验内核 UID/GID、当前 `browser` 子组、租约和原共享服务；跨 Bot、同 UID 的其他运行或 Shell、过期实例均拒绝。会话入口不会在共享服务重建后自动连接新实例，窗口列表继续由同一 Chrome 按所属会话过滤。

撤销立即关闭连接，并独立回收原实例中的所属页面，避免页面脚本在任务资源回收或状态刷盘阻塞时继续运行。页面回收仅发送一次，有独立清理期限与看门狗；未知结果、阻塞或入口被替换均保留失败证据并通知容器退出。任务组与页面均确认回收后才移除原 socket、释放桌面。正常停止单个 Bot 不结束其他页面或共享 Chrome；全容器停服先处理桌面和探针，最后关闭 Chrome。

新增生产接线、实例与会话生命周期回归、真实跨 UID Unix 请求和阻塞回收测试。真实 Chrome 组合验证两个窗口入口只返回各自页面、仅撤销甲即可清除其页面、乙仍能截图、共享账号及正常重启保留，见 `/private/tmp/tombot-window-retirement-controller.log`。资源组和 Chrome 启动器仍使用明确夹具，不能把这些结果视为生产 cgroup 正向入组验收。

最终 `make lint check test` 通过 33 项 Python、格式、静态检查、全包构建和全部 Go 竞态测试，见 `/private/tmp/tombot-window-retirement-checks.log`。Linux amd64 构建和 arm64 带集成标签静态检查通过；六个相关包的 Linux 竞态、实际节点控制绑定及代理入口测试通过，见 `/private/tmp/tombot-window-retirement-linux.log`、上述控制器日志。节点二进制启动、通知、重启与损坏记录拒绝复验见 `/private/tmp/tombot-window-session-entry.log`。

最终本地 arm64 镜像为 `tombot-computer:window-retirement-20261112`，ID `sha256:2e79a59a1220ae8d705ec602ce81cdeee8ed3b243dc089a6a5d752366f111b2d`。工具与编译、PDF、Chrome 沙箱、双 XFCE 桌面、共享文件和浏览器目录重建保留全部通过，两张 1280×800 截图已复核；构建和验收日志为 `/private/tmp/tombot-window-retirement-image-build.log`、`/private/tmp/tombot-window-retirement-image-verify.log`，产物为 `/private/tmp/tombot-window-retirement-image-evidence`。临时测试容器与卷已确认回收，没有推送镜像、部署或更新旧开发包。

代理退出预算现在至少为调用预算加两份清理预算；节点、代理、镜像和配置须配套升级与回滚，详见[代理窗口监督](../internal/computeragent/README.md#会话窗口入口与回收)及[发布说明](../deploy/RELEASE.md)。所有持久记录格式和 SQL 迁移保持不变。桌面窗口客户端、上传下载与完整能力探针仍未接通，账户就绪继续关闭；生产宿主、后续业务、管理平台、客户端切换和旧实现清理尚待完成，公开远程电脑交互继续暂缓。

## 代理接入等待（上一阶段）

节点在出网准备完成后、浏览器启动之前检查原容器的代理控制入口。新增无副作用 `health` 动作，仅确认完整实例身份、当前配置版本和 `agent_control` 范围；不创建桌面、启动浏览器或修改就绪状态。停服、致命故障、携带描述符及伪造回执均拒绝。

必填 `agent_startup` 配置提供总时间、单次时间、次数与退避上限；API 预算同时覆盖等待、浏览器和探针。只等待连接建立前的 socket 不存在或连接被拒绝，其他错误直接失败；整个等待固定原 Docker 和内核父组，不延长租约、不重发浏览器启动。实际 Unix 回归还覆盖关闭描述符成功不能丢失暂态错误分类，资源关闭失败不能被重试掩盖。

`make lint check test` 全部通过，包含 33 项 Python、格式、静态检查、全包构建与全部 Go 竞态测试，见 `/private/tmp/tombot-agent-startup-checks.log`。Linux amd64 构建、arm64 带标签静态检查和实际 Unix 延迟监听、错误对端、浏览器/探针绑定通过；六个相关包的 Linux 竞态检查通过，见 `/private/tmp/tombot-agent-startup-linux.log`。正式节点启停、通知、恢复拒绝及代理入口检查分别见 `/private/tmp/tombot-agent-startup-entry.log`、`/private/tmp/tombot-agent-startup-controller.log`。

本地 arm64 镜像 `tombot-computer:agent-startup-20261112`，ID `sha256:6f299b82d38da70d93bae2b1095e60df88708f241968d7f3b8b1e8b5f3d59f29`，通过工具与编译、PDF、Chrome 沙箱、双 XFCE 桌面、共享文件及浏览器目录重建保留；两张 1280×800 截图已复核。构建与完整验收日志为 `/private/tmp/tombot-agent-startup-image-build.log`、`/private/tmp/tombot-agent-startup-image-verify.log`，产物为 `/private/tmp/tombot-agent-startup-image-evidence`。该镜像还通过受管 Chrome 代理、账号共享与正常重启保留测试，见上述控制器验收日志。

节点配置版本 2、代理配置版本 4 和所有持久记录格式不变，无 SQL 迁移。新增配置和控制动作必须与新镜像配套部署，不能混用旧镜像或旧开发包，升级与回滚规则已写入发布说明。当前只交付源码与本地验收镜像，未推送、部署或更新开发发布包；临时容器与卷已确认回收。测试资源组与 Docker 归属为明确替身，不能替代真实宿主 cgroup、磁盘配额、首次安装及开户验收。窗口网关、每桌面客户端、上传下载与联合能力探针仍待接通，账户就绪门禁继续关闭。

## 宿主持久生命周期（上一阶段）

[宿主浏览器监督](../internal/dockernode/README.md#宿主共享浏览器生命周期)已接入 `Runtime` 的对账、删除、换代和退出，以及正式节点启动恢复。每用户独立保存 `<user_id>.browser.json` 版本 1；服务组创建、请求发送和活动确认分别先后持久化，已有活动实例只查询原授权，未知启动不自动重放。实际 Docker 与内核父组在调用前、对端认证及回包后复验。

关闭先封闭宿主意图，再要求代理确认同授权关闭；失联或错误回执时复验并隔离原容器，确认实际停止及原服务组回收后才保存隔离关闭。该次调用仍返回异常，不把强制隔离当作 Chrome 正常保存账号状态。身份漂移、停止未确认、清理或刷盘失败均保留屏障，不重新启动。旧代理控制目录只有在宿主已关闭且最高消费版本覆盖原浏览器记录时才回收，跨容器代次保留最高版本。

节点配置升级为版本 2，新增必填浏览器与窗口预算；生命周期记录升级为版本 4，阻止旧节点忽略浏览器状态。代理配置仍为版本 4，存储版本 3、桌面版本 2 和 SQL 迁移 `0001`–`0046` 不变。按用户约定不转换旧数据；新旧配置与状态禁止混用，成套升级、排空与回滚要求已同步[发布说明](../deploy/RELEASE.md)。

新增回归发现出网恢复会把浏览器文件误认为用户生命周期记录，已在保留损坏记录拒绝规则的前提下修正恢复分工。修复前后证据分别为 `/private/tmp/tombot-node-browser-recovery-before.log`、`/private/tmp/tombot-node-browser-recovery-after.log`。并发对账只启动一次、未知关闭隔离、全部持久阶段失败、回包后父组漂移、跨代次版本和旧目录回收均有回归；正式节点二进制验证浏览器恢复失败在监听前退出、保留证据及退出后释放锁，见 `/private/tmp/tombot-node-browser-entry.log`。

最终 `make lint check test` 全部通过，包含 33 项 Python、格式、静态检查、全包构建及全部 Go 竞态测试，见 `/private/tmp/tombot-node-browser-final-checks.log`。Linux amd64 构建与 arm64 带集成标签静态检查通过；Linux 代理、共享浏览器、通道、节点与节点入口竞态及三类真实 Unix 绑定通过，见 `/private/tmp/tombot-node-browser-linux-verified.log`。该次运行的部署包因测试容器缺少修改异 UID 临时文件的权限而失败；只补充夹具所需权限后单独复验部署包通过，见 `/private/tmp/tombot-node-browser-deployment-linux.log`，没有放宽生产校验或测试断言。首次浏览器协议夹具误用 `/tmp` 父目录，随后改为独立受保护 `/run` 临时目录，通过正式 FileStore 校验。

本轮不更新用户电脑镜像或发布新开发包，仍沿用上轮代理配置版本 4 镜像进行协议测试；没有创建实际用户电脑或操作宿主 cgroup。资源组及 Docker 为明确测试替身，不能替代生产 cgroup、磁盘挂载、首次安装与开户验收。下一步仍须补齐容器启动到代理控制监听可用的有界等待、窗口网关、每桌面客户端、上传下载及联合能力探针；生产与浏览器就绪门禁保持关闭。其余业务、管理平台、客户端及清理阶段继续按既定顺序推进，公开远程电脑交互继续暂缓。

## 代理持久监督与控制协议（上一阶段）

[共享浏览器监督](../internal/computeragent/README.md#共享浏览器监督与控制协议)已接入正式 `computer-agent.Run`。新增 `browser_start`、`browser_status`、`browser_close` 三个受保护动作及版本 1 的私有 `browser.json`；先持久消费授权，再启动 Chrome，初始化成功后保存活动状态，实际回收完成后保存关闭记录。完整授权包含身份、单调版本、服务实例、原始期限和运行预算；重复或迟到启动、改绑授权、伪造回执均拒绝，未知结果不自动重放。

取消、停服、异常退出和持久化阻塞使用独立撤销与容器看门狗；同代次未关闭记录在监听前拒绝恢复。正常停服等待探针、共享浏览器及桌面控制器全部回收后才释放持久目录。每次窗口授权复验当前桌面许可和实际容器，单 Bot 的窗口生命周期与正式网关仍待下一步接线。

代理部署配置升级为版本 4，防止不理解浏览器恢复记录的旧代理接管新实例。新增回归在修复前证明旧配置仍被接受，修复后节点拒绝且保留原配置，真实镜像入口也在建立监听前拒绝旧版本。节点、代理与镜像必须成套部署，旧数据不转换；用户生命周期版本 3、存储版本 3、桌面版本 2 和历史 SQL 迁移 `0001`–`0046` 均未修改。回滚边界见[发布说明](../deploy/RELEASE.md)。

完整 `make lint check test` 通过，包含 33 项 Python 测试、格式、静态检查、全包构建和全部 Go 竞态测试，日志为 `/private/tmp/tombot-browser-supervision-checks.log`。Linux amd64 全包构建及 arm64 带集成标签静态检查通过；Linux 代理、共享浏览器、控制通道和节点四个包的竞态测试，以及真实节点—代理 Unix 绑定回归通过，见 `/private/tmp/tombot-browser-supervision-linux-race.log`。新监督器另通过并发启动、提前关闭、所有持久阶段失败、取消、失控、重启和并发停服测试；实际 Unix 测试暴露并修正了控制动作白名单遗漏，保留未知动作拒绝检查。

最终本地 arm64 镜像为 `tombot-computer:browser-supervision-20261112`，ID `sha256:581db25a964a01c684844804cd1e0b0bc258821c91e9f9e6db3810978e884842`。实际入口验证配置版本、未关闭浏览器记录拒绝、重复代理拒绝和正常停服；真实 Chrome 验证固定代理、共享账号及正常重启保留，见 `/private/tmp/tombot-browser-supervision-entry-linux.log`。完整镜像工具、编译、PDF、Chrome 沙箱、双 XFCE 桌面、共享文件及重建保留验收通过，见 `/private/tmp/tombot-browser-supervision-image-verify.log`；两张 1280×800 截图已复核，产物位于 `/private/tmp/tombot-browser-supervision-image-evidence`。

本轮临时容器和卷已确认回收；未推送镜像、切换生产或发布新的成套开发包。此前 `0.2.0-dev.20261112.4` 未改写，不包含本轮监督实现。真实 Chrome 成功进程由明确的测试启动器降权，协议测试使用资源组替身，仍不能替代生产 cgroup 成功入组和回收。宿主浏览器持久版本、服务组创建与恢复、旧控制目录回收、每桌面窗口客户端、上传下载和联合能力探针尚未完成；没有放宽未知 `browser.json` 的回收拒绝。整体目标未完成，后续阶段按既定顺序推进，公开远程电脑交互继续暂缓。

## 共享浏览器受管启动与进程监督后端（上一阶段）

新增固定 `computer-task --browser` 引导，以及[代理浏览器后端](../internal/computeragent/README.md#共享-chrome-受管进程后端)。启动授权只允许独立浏览器服务实例，核验真实入组入口、封存请求、原始期限、撤销管道及固定目录后，以 UID 30000 / GID 20000 执行 Chrome。显式使用固定环回代理和私有 CDP 管道，保留沙箱；不开放调试端口，不接受任意参数或环境。

代理通过 root 私有锁保证同一 profile 只有一个受管实例；每次服务使用新的运行目录。启动只执行一次，取消、迟到启动、异常进程退出和协议失联均明确撤销并通知监督者；正常关闭先给 Chrome 有界退出时间，再确认整组清空，最后释放目录和锁。临时目录清理不删除持久 profile 或跨越业务链接。启动前撤销的通知也有独立回归，不能因尚未启动进程而遗漏完成信号。

真实 Chrome 回归发现窗口事件缓存会保留导航前标题。窗口列表现使用一次有界目标快照，复验授权后只返回本 Bot 的窗口；查询失败、缺失或异常目标、取消和授权撤销均拒绝返回旧缓存。新增单元和真实浏览器回归覆盖标题刷新及关闭期间的列表查询；原有双窗口输入、账号切换、弹窗、对话框、持久站点状态和未知动作不重放仍通过，见 `/private/tmp/tombot-browser-title-regression.log`。

最终 `make lint check test` 通过，包含 33 项 Python、格式、静态检查、全包构建和全部 Go 竞态测试，日志为 `/private/tmp/tombot-browser-process-final-checks.log`。Linux amd64 全包构建和带集成标签的相关静态检查通过；Linux 专属代理、引导和共享浏览器竞态测试通过，见 `/private/tmp/tombot-browser-process-linux-race.log`。没有新增或改写 SQL 迁移。

联合回归暴露既有探针协议夹具共用客户端与服务端上下文：客户端读到成功响应后取消自身调用，可能提前取消服务端发送收尾。两端现按真实进程使用独立上下文，原有回收和失败断言保持不变；正常协议与伪造响应分别通过 100 轮普通和竞态复验。失败证据为 `/private/tmp/tombot-browser-process-probe-fixture-before.log`，修复证据为 `/private/tmp/tombot-browser-process-verified-linux.log`、`/private/tmp/tombot-browser-process-probe-protocol-race.log`。

最终本地 arm64 镜像为 `tombot-computer:browser-process-verified-20261112`，ID `sha256:8287eddc0421a52d7b9b118df3ecdd9f4f6d16b06f5308ac5046d7ed122e7211`。完整镜像验收通过全部工具、编译、PDF、Chrome 沙箱、双 XFCE 桌面、共享文件、profile 目录权限和重建保留；两张 1280×800 截图已复核。构建及验收日志为 `/private/tmp/tombot-browser-process-verified-image-build.log`、`/private/tmp/tombot-browser-process-verified-image-verify.log`，产物为 `/private/tmp/tombot-browser-process-verified-image-evidence`。该镜像另通过真实 Chrome 固定代理、账号共享与正常重启保留、两窗口独立中文输入、单 Bot 关闭及正式引导拒绝检查，见 `/private/tmp/tombot-browser-process-verified-linux.log`；浏览器截图位于 `/private/tmp/tombot-browser-process-evidence`。

本轮测试容器与临时卷已确认回收，没有推送镜像、切换生产或运行 amd64 镜像。上一阶段开发包 `0.2.0-dev.20261112.4` 未改写，不包含本轮后端与标题修复；当前产物是源码和新的本地验收镜像，后续正式接线后再生成配套发布包。

该阶段结束时，后端尚未接入正式 `computer-agent.Run` 或宿主控制协议；代理接线现已由上方阶段补齐。宿主生命周期、每桌面的窗口客户端、窗口网关与能力探针仍须完成后才能开放共享浏览器就绪。测试中的 Chrome 成功进程由明确的夹具降权；正式引导仅完成实际拒绝路径，不能当作生产 cgroup 正向入组与回收证据。生产部署和整体重构均未完成，后续业务、管理平台、客户端与旧实现清理仍按用户指定顺序推进，公开远程电脑交互继续暂缓。

## 共享浏览器持久服务目录（上一阶段）

节点将同一用户的 ext4 磁盘划分为两个固定子目录：所有 Bot 共享的 `workspace`（root:20000、2770）和用户级 Chrome 服务持有的 `browser`（30000:20000、0700）。同一 Docker 卷以两个子路径挂载到 `/workspace`、`/var/lib/tombot-browser`，不暴露磁盘根目录；业务文件仍不按 Bot 隔离，浏览器账号仍由同一 Chrome 上下文共享。

存储只有在真实挂载、目录授权和刷盘完成后才记录就绪。挂载回包丢失或初始化中断能够恢复，已就绪目录丢失、符号链接、权限与身份漂移均失败；不重建空 profile 掩盖状态丢失，不递归改业务文件。节点对账拒绝整卷替代子目录、子目录交换及其他卷；镜像入口复验两处挂载与文件系统身份。存储记录升级为版本 3，镜像布局标签为 2；旧记录和旧镜像明确拒绝，按用户约定不迁移旧数据。节点、镜像与回滚环境须配套，详见[持久磁盘](../internal/dockernode/storage.md)和[发布说明](../deploy/RELEASE.md)。历史 SQL 迁移 `0001`–`0046` 未变。

本轮全量回归发现首次并发节点登记的唯一索引竞争：只按主键处理冲突仍会被名称或镜像引用的唯一约束中断。新增同步起跑、12 轮每轮 32 请求的回归，修复前十次运行九次复现；登记改为处理全部唯一约束后复验完整身份，真正的名称、引用和字段冲突仍回滚并报错，不重试未知事务。新回归与原有登记集成测试连续十次通过，日志为 `/private/tmp/tombot-catalog-concurrency-before.log`、`/private/tmp/tombot-catalog-concurrency-after.log`。

最终 `make lint check test` 全部通过，包含 33 项 Python 测试、格式与静态检查、全包构建和全部 Go 竞态测试，日志为 `/private/tmp/tombot-storage-layout-final-checks.log`。定向存储回归覆盖中断恢复、重挂载内容保留、旧版本和路径、权限、挂载漂移拒绝，见 `/private/tmp/tombot-storage-layout-focused.log`。受限 Linux 容器使用正式 Go 布局函数验证实际 UID 30000 / GID 20000，见 `/private/tmp/tombot-storage-layout-linux.log`。

本地 arm64 镜像 `tombot-computer:storage-layout-20261112` 完整验收通过，ID 为 `sha256:129ce0dd86d80a702276742a15e161db1ac0d3e41f15196a93c5149190ba4f7b`。真实 Docker 临时卷验证两子路径、双 Bot 共享文件和工具、Bot 无权读取或改写浏览器目录，移除并重建容器后两类测试内容保留；工具、编译、PDF、Chrome 沙箱、双 XFCE 桌面和输入等原有检查继续通过。日志与图像分别为 `/private/tmp/tombot-storage-layout-image-verify.log`、`/private/tmp/tombot-storage-layout-image-evidence`，两张 1280×800 截图已复核。临时容器和卷已确认回收，没有使用宿主 cgroup、loop 或已有业务卷。这只证明存储目录与镜像能力，未把测试文件当作真实账号状态恢复证据。

已生成并独立复验格式 3 本地双架构开发验收包 `/private/tmp/tombot-release-0.2.0-dev.20261112.4.tar.gz`，67 个文件、168432865 字节；整体 SHA-256 为 `546f1cd2972969498f153fd2311fedd4a7e0466a6cf4e406d771812f254c0957`，源码摘要为 `460871adeaa94107ef8fc76e275f4b3262c06a5946dfe90b6d647ab06eabf790`。两个架构实际完成编译和归档/ELF 核验，日志为 `/private/tmp/tombot-storage-layout-release.log`、`/private/tmp/tombot-storage-layout-release-verify.log`。

发布构建关闭 Git 元数据，程序摘要与普通本地镜像构建不同，因此另从已核验压缩包的确切上下文构建 `tombot-computer:release-0.2.0-dev.20261112.4`，实际 ID 为 `sha256:473643c7b5365e21ad99dbff838f92066a4d1ce770db781b5d9eaa361918154a`。该镜像再次通过完整验收，四个 Go 程序及 `prepare.py` 的摘要逐项与包内一致。日志为 `/private/tmp/tombot-storage-layout-release-image-build.log`、`/private/tmp/tombot-storage-layout-release-image-verify.log`、`/private/tmp/tombot-storage-layout-release-image-match.log`，图像及工具、沙箱报告为 `/private/tmp/tombot-storage-layout-release-image-evidence`。两张截图已复核，临时容器与卷已确认回收。amd64 仅编译和产物核验，未运行 amd64 镜像；产物均未推送外部或切换生产版本。

共享浏览器还需生产进程监督、profile 独占、每桌面的窗口客户端、文件交互和能力探针接线；真实 Ubuntu 安装、硬配额、cgroup 与完整开户验收仍未完成。账户就绪门禁继续关闭，整体目标继续按既定顺序推进，公开远程电脑交互暂缓。

## 部署配置生成（上一阶段）

新增[部署配置生成入口](../deploy/CONFIGURE.md) `tomdeploy-config`。Linux root 使用受保护的单份参数，绑定本机实际 API UID/GID、节点和镜像身份，生成服务、节点、Docker 安装、镜像准备与节点登记五份配置。节点基线直接嵌入随包示例，服务运行预算复用现有默认配置；生成结果继续经过各组件正式解析和跨配置核对。生成器不创建账户、不读取或产生凭据、不连接数据库或 Docker，不启动任何生产资源。

配置在同一受保护父目录写完并刷盘后原子发布；已有目录、包括空目录和并发创建的目录均拒绝覆盖。取消和正常写入失败回收暂存内容，发布后刷盘或回执失败保留原输出并明确报错。字段重复、别名、空值、非法覆盖、目录冲突及身份改绑均拒绝；显式密钥或 DSN 映射整体替换默认映射，不能用默认值掩盖空配置。节点登记容量还须为独立验收电脑保留一个出网名额。

服务配置解析已提前复用实际模型网关规则，空来源、带用户信息的模型 URL、重复来源及无效请求预算在生成与安装前检查阶段即失败，不再等到服务加载凭据、打开数据库后才发现问题。没有修改数据库模型，历史迁移 `0001`–`0046` 保持不变。生成与升级步骤、配置覆盖语义、权限、失败与回滚限制已随包说明。

完整 `make lint check test` 通过，包含格式、静态检查、构建、32 项 Python 与全部 Go 竞态测试，日志为 `/private/tmp/tombot-config-generation-checks.log`；定向回归见 `/private/tmp/tombot-config-generation-focused.log`。覆盖默认绑定、双架构、身份和租约冲突、字段歧义、空映射、并发发布、既有输出保护、取消、写入失败、链接及命令失败路径；`git diff --check` 通过。

禁网、只读 Linux 临时容器使用真实系统账户和空 tmpfs，运行正式生成命令，再由正式服务、节点、镜像与登记解析器消费输出。未准备引用时安装前检查明确失败，准备测试引用后检查通过，重复生成仍拒绝覆盖；全部配置模块回归同时通过。日志为 `/private/tmp/tombot-config-generation-linux.log`。这些引用内容只用于文件元信息验收，不是有效凭据；没有执行真实安装、拉取、数据库登记或开户，没有挂载宿主 Docker、cgroup 或业务卷，测试容器均已回收。

已生成格式 3 本地开发验收包 `/private/tmp/tombot-release-0.2.0-dev.20261112.3.tar.gz`，共 67 个文件、168402091 字节；整体 SHA-256 为 `0f2ca5fb3ecd074e75e5cc0c99d18fc28c865227de8ce5e77e587e636ae90c1a`，源码摘要为 `3f98339665b3e93a55d7e2e5f530bb7d65e1a50ffe9af61747e660c33585d6a8`。两个架构均完成实际编译及 ELF/归档核验，构建和复验日志为 `/private/tmp/tombot-config-generation-release.log`、`/private/tmp/tombot-config-generation-release-verify.log`。新核验器拒绝格式 1、2，并强制包含生成程序及参数说明。

实际压缩包的独立核验工具在无网络、无附加 capability 的 Linux 容器中通过复验；从包内提取的 arm64 生成程序通过同一生成、消费、引用检查及拒绝覆盖流程，见 `/private/tmp/tombot-config-generation-release-linux.log`。两个架构共 34 个电脑镜像上下文文件与上一验收包完全相同，本轮没有重新构建电脑镜像或声称新的镜像验收。amd64 完成编译和产物核验，未在 amd64 宿主运行；发布包未推送外部或切换生产版本。

第一阶段仍待受测 Docker/宿主版本清单、真实 Ubuntu systemd 首次安装、实际挂载/配额/cgroup 和完整开户恢复联调。共享浏览器联合能力尚未接通，不能开放开户；业务、管理平台、客户端切换、旧实现清理与最终验收继续按既定顺序推进，公开远程电脑交互暂缓。

## 安装前成套配置核对（上一阶段）

新增[安装前部署配置检查](deployment-check.md) `tomdeploy-check`，在 systemd 的 Docker 自动引导之前检查 API、节点、Docker 清单、镜像清单。复用各模块正式解析规则，核对实际非 root API 账户、节点及镜像身份、宿主架构、控制端点、租约与调用预算、桌面容量、目录关系和引用文件权限。检查只读取配置与元信息，不读取凭据内容，不连接数据库或 Docker，不创建状态，不发布节点就绪。

修复一个真实部署冲突：API 默认节点验收使用 3 分钟租约，原节点示例只接受 2 分钟，按示例部署会拒绝验收。节点示例已改为 5 分钟，并加入复现旧冲突、恰好足够与开户越界的回归。配置检查同时拒绝 API 加入 Docker 组、身份错配、存储和控制目录重叠、配置复用、链接、不可读私有引用及不安全父目录。默认引导端点统一使用 `/run/docker.sock`。

检查单元与双架构程序纳入格式版本 2 发布包，要求成套更新五个 systemd 单元；格式 1 不再被新核验器接受。升级、权限、失败语义及回滚说明已同步。没有数据库变更，历史迁移 `0001`–`0046` 保持不变，也没有修改业务数据、已有用户容器或正式电脑镜像内容。

完整 `make lint check test` 通过，包含格式、静态检查、构建、32 项 Python 与全部 Go 竞态测试，日志为 `/private/tmp/tombot-deployment-check-all.log`。新配置模块及命令正常、边界与失败回归通过；Linux amd64/arm64 发布编译、集成测试程序编译及 `git diff --check` 通过。最初受限执行中的 Unix 通知 socket 测试被沙箱阻止，随后完整检查在允许本机临时 PostgreSQL 和测试监听的环境中通过，未弱化测试。

专用禁网、只读 Linux 容器使用真实系统账户和空 tmpfs，验证实际配置检查、root 私有 API 配置拒绝、旧租约拒绝、API 不可读引用拒绝及 Docker 组成员拒绝，见 `/private/tmp/tombot-deployment-check-linux.log`。测试没有挂载宿主 Docker、cgroup 或业务卷；临时容器已回收。镜像中的清单与引用内容只是配置检查夹具，没有执行安装或开户，不能据此声明受测生产版本。

已生成本地开发验收包 `/private/tmp/tombot-release-0.2.0-dev.20261112.2.tar.gz`，格式 2，共 64 个文件、152360911 字节；整体 SHA-256 为 `50e708fb93a433eb667fa306e74c4b54d0f7af623231849fe299aa771107b847`，源码摘要为 `123b373b6416cac41b135594298b6be03a92c4bb10d505b55f301c50cecb6ba5`。构建和只读核验通过，见 `/private/tmp/tombot-deployment-release-build.log`、`/private/tmp/tombot-deployment-release-verify.log`。两个架构共 34 个电脑镜像上下文文件与上一格式 1 验收包的内容、大小、权限逐项相同，因此本轮未重复构建或宣称新的电脑镜像验收。

实际发布包在禁网、只读、无附加 capability 的 Linux 容器中使用包内独立工具再次核验通过；包内 arm64 检查程序也确认拒绝 Docker 组账户，见 `/private/tmp/tombot-deployment-release-linux.log`。产物没有推送到外部仓库或安装到生产服务；amd64 本轮完成编译和 ELF 核验，未在 amd64 宿主运行。

第一阶段仍待具体环境配置生成、受测 Docker/宿主版本清单、真实 Ubuntu systemd 首次安装、实际挂载/配额/cgroup 和完整开户恢复联调；共享浏览器能力仍未接通，不能开放开户。后续阶段继续按用户顺序推进，公开远程电脑交互暂缓。

## 完整双架构发布包（上一阶段）

`make release` 已从分散交叉编译改为[完整发布包构建与核验](../deploy/RELEASE.md)。构建器复制当前工作树中声明的源码到独立快照，使用锁文件重新安装管理端依赖并构建，随后编译 Linux amd64/arm64 全部宿主和电脑程序；不复用旧 `dist` 或旧二进制。包内包含管理平台、systemd 单元、清单格式、节点配置参考、自包含电脑镜像上下文和独立只读核验工具，共 61 个文件。

归档清单记录源码与每个文件的摘要、容量和权限，逐项校验必要集合及 ELF 架构。打包与复验通过后才原子创建目标，拒绝覆盖旧产物。核验要求来自受信渠道的整体 SHA-256，拒绝缺件、篡改、重复路径、附加文件、链接、扩展头、错误权限及展开超限，不解包或执行安装。格式版本、构建时限、部署映射、旧构建布局移除及回滚要求已写入说明；没有修改数据库迁移或节点协议。

已真实生成开发验收包 `/private/tmp/tombot-release-0.2.0-dev.20261112.1.tar.gz`，大小 136488344 字节，整体 SHA-256 为 `c2b95b0beb21fea58e2414863270a16a32035fd69b350b966886d4b949eeaeb7`，源码摘要为 `368e489efd7ed46a7d612677694d2e9c4545b32b8cc1c80652e86029725ada65`。构建和正式核验入口通过，见 `/private/tmp/tombot-release-bundle-build-final.log`、`/private/tmp/tombot-release-bundle-verify.log`。此包是本地开发验收产物，没有发布到外部仓库或切换生产版本。

完整 `make lint check test` 通过，包含格式、静态检查、构建、32 项 Python 及全部 Go 竞态测试，见 `/private/tmp/tombot-release-bundle-checks-final.log`；管理平台 `npm run check` 全部通过，包含 23 项测试，见 `/private/tmp/tombot-release-admin-checks.log`。新增发布测试验证正常归档、必要组件、数量容量、来源快照、旧产物拒绝、原子发布、进程超时、构建失败及损坏内容。`git diff --check` 通过。

实际压缩包解入独立临时目录后，在禁网、只读、无附加 capability 的 Linux 容器执行包内核验工具，并运行全部 11 个 arm64 程序的非法启动拒绝路径，见 `/private/tmp/tombot-release-bundle-linux.log`。amd64 已编译并校验 ELF 及归档内容，本轮未在 amd64 运行；入口拒绝检查也不代表正式服务已经启动成功。

直接使用包内 arm64 电脑上下文构建镜像 `tombot-computer:release-0.2.0-dev.20261112.1`，实际镜像 ID 为 `sha256:c00e140d5248bbc46780d3870a06f20787611a6bdbeb2e4ffb22920619a72313`，四个电脑程序与发布清单摘要逐一一致。完整镜像验收通过工具、编译/PDF、Chrome 沙箱、双 XFCE 桌面、共享文件、权限拒绝和定向关闭；两张 1280×800 中文桌面截图已复核。日志为 `/private/tmp/tombot-release-image-build.log`、`/private/tmp/tombot-release-image-verification.log`，证据目录为 `/private/tmp/tombot-release-image-verification-20261112`。测试容器均已回收，没有挂载用户数据或宿主 cgroup，没有替换既有容器。

发布回执只声明 `scope: release_artifacts`。具体部署参数成套核对、受测 Docker/宿主包清单、真实 Ubuntu systemd 首次安装、实际挂载/配额/cgroup 和完整开户恢复联调仍待完成；共享浏览器能力尚未接通，不能开放开户。后续阶段继续按既定顺序推进，公开远程电脑交互暂缓。

## 周期验收元数据保留（上一阶段）

已补齐[旧代次控制目录回收](../internal/dockernode/README.md#控制元数据保留与代次回收)及[磁盘最高代次记录](../internal/dockernode/storage.md)。每个逻辑用户使用一份磁盘记录和一把跨代次稳定锁；没有创建过物理磁盘的代次也必须持久关闭。周期验收不再每轮留下永久的磁盘记录和锁，旧代、跳代、未关闭、身份或容量漂移均拒绝。

当前代次在清理后保留代理控制目录，下一代建立前回收。换代先确认执行会话、探针、出网均关闭，实际容器、卷、磁盘均不存在，再检查挂载和目录占用。清理只处理有数量上限的平面控制文件，不递归，不删除未知内容、链接、挂载或共享业务文件。宿主的最高生命周期、探针版本与每桌面关闭屏障继续保留。

用户生命周期改为版本 3，磁盘记录改为版本 2 每用户布局；删除前持久保存旧目录设备号、inode 和下一代规格、控制版本及租约。中断恢复可继续清理已缺失配置的原目录，但拒绝同名替换、旧控制者、资源改绑与令牌复用。旧布局和旧版本明确拒绝，不自动迁移；部署、性能边界、恢复及配套回滚说明已同步。没有新增数据库迁移，历史 `0001`–`0046` 保持不变，也未修改正式电脑镜像或生产资源。

完整 `make lint check test` 通过，包括格式、静态检查、构建、22 项 Python 及全部 Go 竞态测试，日志为 `/private/tmp/tombot-metadata-retention-checks.log`。定向回归覆盖连续 24 代、12 代模拟物理磁盘、并发换代、稳定锁、关闭前置条件、损坏记录、删除中断、接管与旧回包，见 `/private/tmp/tombot-metadata-retention-focused.log`。Linux amd64/arm64 全包构建和 `git diff --check` 通过。

禁网、只读、无附加 capability 的临时 Linux 容器通过真实文件、flock、目录刷盘、只读挂载表及上述回归，日志为 `/private/tmp/tombot-metadata-retention-linux.log`；只挂载测试程序，临时内容及容器已回收。Docker、loop 和资源组成功路径仍为明确替身，不能据此宣告真实生产电脑验收通过。

第一阶段仍需真实 Ubuntu systemd 首次安装、受测发布清单、实际 loop 挂载/配额/cgroup 回收及完整部署恢复联调。共享浏览器能力在第二阶段接通，未取得完整能力证据前不会开放开户；后续业务、管理平台、客户端切换、旧实现清理与最终验收继续按顺序推进。公开远程电脑交互继续暂缓。

## 独立电脑验收与镜像准入（上一阶段）

新增[节点专用验收流程](node-verification.md)，已接入正式 `tomd` 后台监督。控制租约有效后，使用独立临时电脑和正式节点生命周期进行检查；不创建业务账户、不使用用户电脑、不向 API 交付 Docker 权限。能力全部通过后先确认本代次资源清理与持久关闭，再复验实际节点实例并发布有期限的镜像准入。共享浏览器探针未接通时仍明确失败，不能绕过。

新增迁移 `0046_node_capability_verification.sql`，历史 `0001`–`0045` 未修改。每节点仅保留一个活动验收账本；控制租约中断或节点换代会失效旧证据。开户与失败重试共用双期限及镜像门禁，未验收镜像、停用镜像、过期能力和过期控制租约均拒绝。目录查询补充验收阶段、错误码、已验收镜像和期限；部署配置必须显式提供 `node.verification.image_id`。

已覆盖并发领取、旧控制者回包、锁等待后租约过期、创建/清理结果未知、有限对账与隔离、取消、排空、节点重启、失联恢复、配置改变、未来或过期能力证据等路径。复核正式节点协议后，周期检查保持相同逻辑电脑及操作身份，递增代次、尝试号和控制令牌；物理不存在仍通过幂等 `Remove` 确认关闭屏障。读取验收账本时同时拒绝业务账户/电脑归属和镜像引用不匹配，避免验收清理触及业务资源。

完整 `make lint check test` 已通过，包含 22 项 Python、全部 Go 竞态测试及真实临时 PostgreSQL；日志为 `/private/tmp/tombot-node-verification-checks-final.log`。Linux amd64/arm64 全包构建与 `git diff --check` 通过。测试中的成功能力结果为明确的节点接口替身，未启动真实验收容器或开户，不能作为生产能力证据。

该阶段仍待完成真实 Ubuntu systemd 首次安装、受测发布清单、实际 loop 挂载/配额/cgroup 回收及完整部署恢复联调。此后已补齐上方周期验收的元数据保留规则；恢复屏障继续保留，不自动删除未知记录。共享浏览器实际能力在第二阶段完成，公开远程电脑交互继续暂缓。

## 宿主磁盘工具自动引导（上一阶段）

[Docker 引导](../internal/dockerbootstrap/README.md)已补齐宿主 `e2fsprogs`、`util-linux`、`mount` 的固定版本安装与实际状态核验。[发布清单](../deploy/docker-install.schema.json)新增三个必填宿主包版本；已有 Docker 也经过依赖检查，缺包时自动安装，已装包版本或架构不符、未配置、损坏或计划移除均拒绝继续。没有自动升级/降级或全局包修复的兼容分支；APT 传递依赖可能更新，发布说明要求在新节点或排空维护窗口执行。

安装后及每次重启均复查实际 dpkg 状态、六项宿主工具入口和解析目标的权限，并执行无副作用版本查询。引导与节点运行时通过 `storagehost` 使用同一工具路径契约。安装返回成功但包仍缺失/版本漂移不能就绪；结果未知时先失败，重启根据实际已安装状态恢复，不盲目重放。报告新增实际查询得到的 `host_packages`；失败移除成功报告，保留失败阶段。本轮没有修改数据库迁移或正式电脑镜像。

完整 `make lint check test` 已通过，包含 22 项 Python 及全部 Go 竞态测试，日志为 `/private/tmp/tombot-host-dependencies-checks.log`；Linux amd64/arm64 全包构建、宿主集成标签静态检查和 `git diff --check` 通过。新增回归覆盖已有 Docker 补装、重复引导、包查询异常、架构/版本冲突、半配置、持有与移除残留、安装结果未知恢复、工具缺失/权限/链接/取消等路径。

真实 Ubuntu 24.04 arm64 只读临时容器通过 `TestLiveHostPackages`，检查实际 `e2fsprogs 1.47.0-2.4~exp1ubuntu4.1`、`util-linux/mount 2.39.3-9ubuntu6.6` 及六个工具可执行性，也验证错误版本被拒绝；日志为 `/private/tmp/tombot-host-dependencies-linux.log`。这些版本只记录此次工具接口验证，不是完成整节点验收的生产发布清单。该检查禁网、只读、移除全部 capability，不安装包、不连接 Docker socket、不挂载用户卷。

共用工具契约后的正式 Linux 节点入口回归也通过，包含启动、重启、配置和恢复记录拒绝、跨 UID 控制客户端、主进程通知和 SIGTERM，见 `/private/tmp/tombot-host-dependencies-node-linux.log`。它仍使用隔离临时容器及明确的 Docker 空资源查询替身；没有真实开户、用户容器创建或磁盘挂载。

第一阶段仍缺完整能力发布、Ubuntu systemd 首次安装、受测发布清单、真实 loop 挂载/配额/入组/回收及镜像准备到开户的恢复联调。本次不能以工具版本查询替代实际磁盘能力验收。共享浏览器及其余阶段继续按既定顺序推进；公开远程电脑交互保持暂缓。

## 控制心跳与过期准入（上一阶段）

新增[节点控制租约](node-health.md)：正式节点在生命周期锁内持久递增进程代次，通过受保护 `heartbeat` 动作返回实例及单调序号；API 使用控制连接持续观察，不获得 Docker 权限。心跳只证明控制进程可响应，不声明 Docker、镜像、浏览器或用户电脑就绪。

新增迁移 `0045_computer_node_control_lease.sql`，历史 `0001`–`0044` 未修改。数据库保护进程绑定、代次、序号与租约，拒绝旧进程和重复结果延长期限；有效期锚定节点调用前取得的数据库时刻。首次绑定、重启或租约中断后恢复均要求重新验收能力，排空状态保留，不自动发布 ready。开户、失败重试和管理节点目录使用同一有效状态规则，过期节点显示离线并拒绝新增电脑；容量预留先取得行锁再检查时限，避免锁等待期间过期的旧判断仍被采用。

心跳失败不会关闭 API 登录或历史查询；systemd API 使用 `Wants` 和 `After` 拉起节点及其引导流程，运行期节点维护不连带停止查询服务。节点控制代次和租约均保留到恢复流程，不能通过删除状态或手改 ready 绕过。部署、接口契约、数据锁影响及回滚限制已写入说明。

服务端完整 `make lint check test` 通过，包含 22 项 Python 与全部 Go 竞态检查，日志为 `/private/tmp/tombot-node-heartbeat-checks.log`。真实 PostgreSQL 测试覆盖并发乱序、重启旧回包、重复请求、失联恢复、排空、行锁等待、首次开户/失败重试门禁及前向迁移。全量回归发现桌面工具在状态调用期间丢失取消原因，已修正错误因果链且保留原有断言；原测试与新增回归连续 30 轮通过，见 `/private/tmp/tombot-desktop-cancel-regression.log`。

Linux 正式节点入口与完整跨 UID 协议验证通过，包含心跳、启动恢复、重复启动拒绝、主进程通知及退出，见 `/private/tmp/tombot-node-heartbeat-entry-linux.log`、`/private/tmp/tombot-node-heartbeat-api-linux.log`。测试使用临时禁网只读容器，Docker 空资源查询为明确替身，不挂载宿主 Docker socket、cgroup 或用户数据；没有更新正式电脑镜像或部署真实节点。Linux amd64/arm64 全包构建与节点集成标签静态检查通过。

第一阶段仍缺完整能力发布、宿主依赖自动安装与首次部署、实际挂载/配额/入组/回收及开户恢复联调。控制心跳不能代替这些证据。共享浏览器按第二阶段补齐，公开远程电脑交互继续暂缓。

## 独立节点入口（上一阶段）

新增正式 `tomnode` 命令、严格受保护配置、[完整配置示例](../deploy/node.example.json)及 [systemd 节点单元](../deploy/systemd/tombot-node.service)，详见[节点入口说明](../internal/nodeapp/README.md)。入口依次组合真实磁盘后端、节点生命周期、遗留探针恢复、执行会话恢复和出网监督，全部完成后才开放受保护控制 API。非 root API 通过既有内核对端认证调用；root 节点不持有业务数据库或模型凭据。

生产构造器不再接受外部注入的能力探针，内部汇总本次已关闭探针、实际容器和父资源组身份、宿主 ext4 挂载与容器所见实际容量。未确认的证据、旧租约和资源变化均拒绝。基础能力通过也不会推定浏览器成功，完整 `Ready()` 继续为 false；节点的 `READY=1` 只表示控制服务启动，不发布数据库健康状态或开放账户。

节点 API、执行监督或出网监督任一意外退出都会停止接单；确认全部在途操作结束后，才顺序关闭会话、出网/节点、存储记录与控制端点。超时或未知在途动作返回明确的进程退出错误，保留后续依赖和恢复记录。新增配置检查覆盖组件间预算、目录边界和嵌套 JSON 歧义；没有新增或修改数据库迁移。

服务端 `make lint check test` 全部通过，包含 22 项 Python 测试及全部 Go 竞态检查，日志为 `/private/tmp/tombot-node-entry-checks.log`。Linux amd64/arm64 全包构建和带节点集成标签的静态检查通过。Linux 节点 API 协议回归及新增“在途动作未结束必须退出”检查通过，见 `/private/tmp/tombot-node-api-linux.log`。

正式 `tomnode` 子进程在临时禁网、只读 Linux 容器通过实际受保护配置、非 root 客户端、文件锁、重复启动拒绝、正常重启、损坏恢复记录阻断、Unix 启动通知内核主进程身份及 SIGTERM 退出，见 `/private/tmp/tombot-node-entry-linux.log`。Docker 只读资源查询使用明确替身，未创建用户电脑、挂载用户卷或写宿主 cgroup。用户电脑镜像不包含宿主 ext4 工具，因此验证使用[专用测试镜像](../verification/node-entry/Dockerfile)补充真实工具；没有放宽生产校验或改变正式电脑镜像。

第一阶段仍在进行：节点健康发布、宿主完整依赖与首次安装、真实挂载/配额/入组/回收、镜像准备到开户的恢复联调仍待完成。共享浏览器联合证据在第二阶段补齐，业务、管理平台、客户端、旧实现清理和最终验收仍按顺序推进；公开远程电脑交互继续暂缓。

## 固定镜像准备（上一阶段）

新增正式部署命令 `tomimage-prepare`、受保护清单、JSON Schema 和 systemd 前置单元，见[镜像准备说明](image-preparation.md)。命令以 root 通过本机 Docker Unix API 准备一个固定摘要及宿主平台镜像，不读取个人 Docker 配置；私有仓库认证只通过独立私有文件提供。实际引擎明确报告不存在时才拉取一次，读取有界进度并检查流内错误，完成后再次核实仓库摘要、Docker 镜像 ID、平台和代理入口。只返回 `scope: image` 的本地可用回执，不发布电脑或节点就绪。

每个镜像使用稳定文件锁和独立的版本 1 `.image.json` 记录，准备、可用和失败均保留原节点、镜像及摘要身份。重启或结果未知后先复验实际引擎，不自动重放拉取、不删除既有镜像。已存在的镜像 ID、摘要、平台或节点归属不能被改绑；配置、认证、记录损坏及权限漂移均阻断操作；镜像准备与运行节点共用 root 所有且权限为 0600/0660 的 Docker socket 检查。开户 `Ensure` 在创建磁盘、卷和容器之前加上同一镜像检查，缺失镜像不隐式下载。

完整 `make lint check test` 通过，包含 22 项 Python 和全部 Go 竞态检查，日志为 `/private/tmp/tombot-image-preparation-checks.log`。常规测试覆盖身份/平台、镜像契约、各阶段失败、认证边界、未知结果恢复、进度流限制和开户前置门禁。Linux 独立测试使用真实 root 保护文件、文件锁和 Unix HTTP，验证八个并发准备只拉取一次、取消关闭下载连接、错误文件/父目录/平台/记录拒绝；日志为 `/private/tmp/tombot-image-preparation-linux.log`，Docker 响应为明确测试替身。

实际 Docker 只读验收通过已有正式 arm64 镜像 `tombot-computer@sha256:3d7e6960f34fd1c724ef50b5608887e155a3d5b0ca207e8484b5e4a927899dcb` 的真实元数据及重复准备，见 `/private/tmp/tombot-image-preparation-live.log`。验收代码明确禁止拉取，没有启动用户容器、推送、修改或删除镜像。本轮未变更数据库迁移或电脑镜像，新的部署依赖与回滚要求已同步文档。

第一阶段仍在进行：独立节点程序/系统服务、完整能力探针与节点状态发布、首次生产安装及部署恢复联调待完成。远程仓库实际下载、私有仓库认证及 Ubuntu systemd 首次部署尚未实测，不能用协议测试代替；共享浏览器及后续阶段仍按既定顺序推进，公开远程电脑交互继续暂缓。

## 宿主探针生命周期（上一阶段）

[宿主探针](../internal/dockernode/README.md#宿主基础探针)已接入 `Runtime`：在用户生命周期独占锁内，先持久保存单调版本和不可复用的服务实例，再创建真实服务组、通过受保护 Unix 通道传递三个控制入口并调用容器代理。证据必须匹配本次实例，代理与宿主均确认回收后才传给后续完整能力检查；调用前、对端认证时及最终回收后均复验 Docker 实例和实际父组。没有添加部分能力直接就绪的分支。

新增每用户 `<user_id>.probe.json` 版本 1，记录准备、已发送、回收中和已关闭状态。失败只回收原实例，不自动重发未知请求；未确认关闭时不能创建下一探针或删除容器。节点启动先逐用户处理遗留记录，再进入原有会话回收和出网恢复。探针版本跨容器代次保留，错误归属、未来代次、损坏记录、路径替身与版本倒退均明确拒绝。内核已移除整个容器父组时，必须直接核实 cgroup 路径不存在，不能只凭 Docker 返回不存在就宣布清理完成。

服务端 `make lint check test`（22 项 Python、全部 Go 竞态检查）通过，日志为 `/private/tmp/tombot-node-probe-checks.log`。测试覆盖所有持久化/创建/调用/回收失败阶段、取消、未知结果不重放、容器漂移、跨代次版本、重启恢复及检查与容器删除互斥。修正了 JSON 往返后的时间比较，按实际到期时刻核验租约，同时保留完整身份与控制版本检查。

Linux amd64/arm64 全包构建及相关带集成标签的静态检查通过。禁网、只读、无附加 capability 的临时 Linux 容器通过真实节点—代理 Unix 协议、描述符传递、双方关闭屏障及错误对端拒绝；原有双桌面授权协议回归也通过，日志为 `/private/tmp/tombot-node-probe-linux.log`。资源组常规 Linux 测试与真实内核只读“不存在”检查通过，见 `/private/tmp/tombot-node-probe-groups-linux.log`。协议成功路径仍采用明确的资源组和探针工作者替身，不能替代实际入组、工作者执行和强制回收的生产验收。

本轮没有修改数据库迁移或镜像入口，沿用下方已验收镜像；宿主新增必填 `Options.Probes`，完整能力探针接口增加已核实的基础证据参数。部署、性能、恢复及回滚要求见模块说明。第一阶段仍缺独立节点程序/系统服务、完整能力探针组合、镜像准备与正式激活联调；共享浏览器窗口、后续业务阶段及最终验收尚未完成，远程电脑公开交互继续暂缓。

## 探针代理调用与回收监督（上一阶段）

[探针监督器](../internal/computeragent/README.md#容器基础探针监督)已接入正式 `computer-agent` 入口和 `probe` 控制动作。节点侧客户端只发送一次请求，代理按独立服务实例串行检查，不占用 Bot 桌面锁；先持久消费单调版本，再启动固定非 root 程序，返回证据前必须撤销、确认整组为空、关闭入口并持久记录关闭。重复与较低版本、实例复用、部分证据及无回收确认均拒绝。

新增私有 `probe.json` 版本 1 记录，与桌面记录共用原子替换、文件和目录刷盘；不新增数据库迁移。取消和停服触发独立撤销，刷盘或执行阻塞超过清理期限会通知既有看门狗退出容器。同代次重启发现未关闭探针时在监听前失败，日志带用户和电脑标识；新代次可封闭旧状态但保留最高消费版本。协议、记录、性能边界和成套升级及排空回滚要求已同步模块说明。

最终 `make lint check test` 全部通过，含 22 项 Python 测试及全部 Go 竞态检查，日志为 `/private/tmp/tombot-probe-supervisor-final-checks.log`。新增测试覆盖并发请求只消费一次、独立 Bot 授权、全部失败阶段、取消/停服、阻塞持久化时撤销及重启边界，五轮竞态复验见 `/private/tmp/tombot-probe-supervisor-concurrency.log`。回归暴露既有致命桌面测试过早重置替身的竞态，已等待后台临界区退出，保持生产退出行为与断言不变；该用例 30 轮复验通过，见 `/private/tmp/tombot-probe-supervisor-watchdog-regression.log`。

Linux amd64/arm64 全包构建和相关静态检查通过。禁网、只读、非 root 临时容器通过全部常规代理测试，包含真实 Unix 描述符传递、重复探针和伪造响应拒绝，见 `/private/tmp/tombot-probe-supervisor-linux.log`。正式入口另用空白临时卷验证未知探针恢复拒绝及业务日志、双实例拒绝、正常停服、监听释放和实际固定探针拒绝伪造入组，见 `/private/tmp/tombot-probe-supervisor-final-entry.log`。

最终 **linux/arm64** 镜像为 `tombot-computer:probe-supervisor-final-20261112`，ID `sha256:3d7e6960f34fd1c724ef50b5608887e155a3d5b0ca207e8484b5e4a927899dcb`。完整镜像验收通过全部工具、编译/PDF、Chrome 沙箱、双 XFCE 桌面、共享文件和定向关闭，两张 1280×800 中文截图已复核。构建与验收日志为 `/private/tmp/tombot-probe-supervisor-final-image-build.log`、`/private/tmp/tombot-probe-supervisor-final-image-verification.log`，产物为 `/private/tmp/tombot-probe-supervisor-final-image-verification-20261112`。全部测试容器已回收，未推送镜像、替换业务容器或运行 amd64 镜像。

成功协议测试仍使用明确资源组替身，正式程序只完成真实拒绝路径；没有声称实际服务成功入组和内核回收验收通过。宿主节点的持久版本、服务组创建、实际实例对端绑定、探针调用及最终移除仍须接通，并结合硬容量、出网代理和共享 Chrome 后才能发布完整能力。第一阶段及整体目标均未完成，公开远程电脑交互继续暂缓。

## 固定非 root 能力探针（上一阶段）

新增镜像内固定程序 `computer-probe` 和[容器基础能力探针](../internal/computerprobe/README.md)。探针使用独立基础服务实例、保留 UID `30001` 与公共 GID `20000`，只执行 PTY、共享文件读写同步、实际文件系统容量和本地网络检查。封存授权限定 `probe` 服务组及原始单调期限，不接受命令、目录、环境或身份覆盖；复用任务引导的入组检查、撤销管道、全线程降权与描述符关闭。检查失败不返回部分成功证据。

真实 Linux 验证发现并修复两个问题：无网络容器仍可能有停用且无地址的内核隧道接口，探针现按启用状态和地址检查，继续拒绝非环回联网；任务回归夹具的业务 tmpfs 须显式允许执行，否则不能验证共享安装工具。增加接口边界及伪造入组、撤销、过期授权测试，原有权限断言保持不变。

`make lint check test` 全部通过，含 22 项 Python 测试及全部 Go 竞态测试，日志为 `/private/tmp/tombot-probe-final-checks.log`。Linux amd64/arm64 全包构建及新增 Linux 代码静态检查通过。非 root、禁网、只读临时容器验证真实 PTY、32 MiB 文件系统、读写同步、临时内容回收及网络检查；六项最小 capability 临时容器验证探针入口拒绝，并回归多线程降权、共享文件和工具、桌面私有目录拒绝。日志分别为 `/private/tmp/tombot-probe-evidence-linux.log`、`/private/tmp/tombot-probe-task-regression-linux.log`，未挂载业务卷或宿主 cgroup，测试容器已回收。

已构建并完整验收 **linux/arm64** 新镜像 `tombot-computer:probe-20261111`，ID 为 `sha256:efa17827ede965dd4fc1a64955c1419aed0711d0611037824fc38ac240041480`。固定探针随构建及双架构发布入口打包；镜像验证未授权 root/非 root 探针拒绝、全部工具、Chrome 沙箱、双 XFCE 桌面、共享文件和定向关闭，两张 1280×800 中文截图已复核。构建日志为 `/private/tmp/tombot-probe-image-build.log`，验收日志为 `/private/tmp/tombot-probe-image-verification.log`，产物为 `/private/tmp/tombot-probe-image-verification-20261111`。未推送镜像、替换现有容器或运行 amd64 镜像。

本轮没有数据库变更，也未改变已发布节点、代理配置或 Bot 任务协议。探针尚未接入代理调用、持久服务监督及节点 `Probe.Check`；真实成功入组和回收、硬容量与出网代理、共享 Chrome 仍须组合核验，不能将本轮基础证据用于宣告电脑就绪。第一阶段继续推进这些接线、独立节点入口和部署恢复联调；整体目标仍未完成，公开远程电脑交互继续暂缓。成套发布及停止接单、排空、保留记录后回滚的要求见模块说明。

## 节点与镜像目录登记（上一阶段）

新增[本机目录登记命令](node-registration.md) `tomnode-register`，使用 API 的 Linux 非 root 身份、同一服务配置及最小控制连接，提供原子登记和状态查询。节点 ID 固定来自部署配置，镜像限定固定摘要；首次节点为 `bootstrapping`，不能由登记命令标记就绪。相同登记幂等，不覆盖节点运行状态、容量预留或已停用镜像；冲突、审计失败与未知提交均明确失败，查询原身份，不创建替代记录。

新增迁移 `0044_computer_catalog_identity.sql`，移除未使用的节点 `endpoint`，保护节点 ID、镜像 ID 与引用不可修改，并撤销运行控制角色的目录删除权限。历史迁移不变，现有新架构目录状态和电脑关联保留；升级锁影响及回滚方式见部署说明。测试夹具同步采用最终字段，未保留旧端点兼容层，没有连接或修改已有业务数据库。

`make lint check test` 最终全部通过，含 22 项 Python 测试和全部 Go 竞态检查，日志为 `/private/tmp/tombot-node-catalog-final-checks.log`。真实 PostgreSQL 验证 16 次并发幂等、唯一冲突、跨记录回滚、审计失败、停用状态保留、固定身份及最小权限、前向迁移与重复迁移；正式部署组合验证账本复验及仅登记节点不能创建管理员。定向证据为 `/private/tmp/tombot-node-catalog-focused.log` 和 `/private/tmp/tombot-node-catalog-migration.log`。

全量回归发现既有代理测试在致命退出后提前重置替身的收尾竞态，已让测试等待清理临界区退出，保留全部失败断言和生产立即退出行为。该用例 100 次竞态复测通过，日志为 `/private/tmp/tombot-node-catalog-watchdog-race.log`。

Linux amd64/arm64 全包构建与相关静态检查通过；部署文件及新命令边界测试在禁网、只读、非 root、无附加 capability 的临时 arm64 容器通过，分别记录于 `/private/tmp/tombot-node-catalog-files-linux.log`、`/private/tmp/tombot-node-catalog-command-linux.log`。只挂载测试程序，不访问业务卷、宿主 cgroup 或真实用户电脑。

登记命令已加入构建和双架构发布入口。本轮没有发布镜像、启动真实节点或修改节点通信协议；镜像启用只表示允许后续供给选用，不代表已拉取或通过运行验收。第一阶段继续补独立节点服务、就绪发布、镜像准备、实际能力探针和部署恢复联调；整体目标未完成，公开远程电脑交互仍暂缓。

## 容器基础服务资源组（上一阶段）

新增[容器基础服务进程组](../internal/processgroup/README.md#容器级基础服务)，为能力探针与共享 Chrome 提供独立实例身份、受限入组/终止/事件描述符、实际进程归属核验和定向回收。它们与 Bot 会话组同级，服务不借用虚构的 Bot 任务；内核校验和回收逻辑与现有会话共用。错误用途、非规范实例、替换父目录、普通文件入口、取消及跨服务入口均不能放行。受信监督器仍须持久封闭授权，已关闭实例不得复用。

`make lint check test` 通过，包含 22 项 Python 测试与全部 Go 竞态测试，日志为 `/private/tmp/tombot-service-groups-full-checks.log`。Linux amd64/arm64 全包构建和内核测试编译/静态检查通过。新测试程序在禁网、只读、非 root、无附加 capability 的临时 arm64 Linux 容器中验证身份、取消及普通文件控制替身拒绝，日志为 `/private/tmp/tombot-service-groups-linux.log`；只挂载测试程序，没有挂载宿主 cgroup 或业务卷。

新增 `make test-process-groups` 真实内核验收入口，覆盖实际入组、父 inode 漂移、跨服务/任务入口拒绝、强制回收、Bot 及探针关闭不影响共享服务、旧描述符迟到入组拒绝。该入口要求专用 Linux 测试节点及真实可写 cgroup；当前仅完成编译，尚未执行，不能将受限容器拒绝测试当作内核正向验收。

本阶段不修改数据库迁移、已发布节点协议、生产镜像或已运行服务。现有会话四入口契约保持不变；基础服务尚未接入节点启动和容器代理，也未据此标记电脑就绪。后续成套发布时须停止接单、排空会话和基础服务后再升级或回滚，保留全部持久控制记录。第一阶段仍待独立节点服务、节点与镜像登记、实际能力探针及部署联调；共享浏览器窗口客户端和公开远程电脑交互继续按原顺序及暂缓范围处理。

## 管理员网页激活（上一阶段）

管理平台登录页新增“激活管理员账户”，接通[部署签发到网页激活](admin-bootstrap.md)流程：验证一次性材料、手动登记验证器、设置并确认密码、提交动态码，再返回登录。激活成功不隐式创建会话，提示等待新的动态码；普通用户激活材料不能通过管理员登记接口。

页面采用现有共享主题与表单样式。登记材料默认隐藏，只保留在本次页面内存；返回、离开及进入历史缓存时清除输入并取消在途请求。错误动态码允许显式重新输入，失效材料回到起始步骤；断连、超时、非完成响应及其他未知结果清除材料并引导登录核实，不自动重放。公开认证请求不携带管理 Cookie 或 CSRF，不影响其他管理会话。

`admin/npm run check` 全部通过，包含格式、ESLint、TypeScript、23 项单元测试及生产构建；两条实际浏览器流程通过，覆盖浅深色、窄屏、键盘焦点、CSP、错误材料、激活与返回登录，以及既有用户和模型页面。日志分别为 `/private/tmp/tombot-admin-activation-final-checks.log` 和 `/private/tmp/tombot-admin-activation-final-browser.log`。截图位于 `/private/tmp/tombot-admin-activation-report-20261111`，三张新增页面截图已复核，密钥字段额外遮挡。浏览器使用已安装的匹配测试目录 `/private/tmp/tombot-playwright`；没有下载或更换系统浏览器。

服务端 `make lint check test` 通过，包含 22 项 Python 测试及全部 Go 竞态测试。真实临时 PostgreSQL 与正式 HTTPS 应用验证部署签发、重复登记返回同一材料、动态码必填、激活值与动态码不可重放、首次管理登录、Cookie 安全属性和管理接口访问。日志为 `/private/tmp/tombot-admin-activation-server-checks.log`。Linux amd64/arm64 全包构建及应用组合静态检查通过。

本轮不改变数据库迁移、节点协议或电脑镜像。页面与 API 成套发布，激活失败或回包未知不能通过重建账号掩盖；回滚保留原激活消费、MFA 和审计记录，不能重置时间步。本次浏览器业务请求和 Docker 节点资源分别使用明确测试夹具，未验收真实用户容器供给。生产部署阶段下一步仍为独立节点、节点与镜像登记、真实能力探针与完整部署验证；整体目标未完成，远程电脑交互继续暂缓。

## 管理员身份分离与节点配置（当前阶段）

管理员与普通用户拆成两套身份：新增迁移 `0050_split_administrators.sql` 建立 `administrators`、凭据、设备、会话、动态验证与激活材料表，把已有管理员整体迁出用户表并删除其电脑、限额与会话，`users` 不再保留 `role` 列。管理入口只查管理员表，桌面入口只查用户表，同一账号不会出现在另一条链路。最后一名有效管理员的保护转移到 `administrators`。

管理员账号由身份模块直接开立：首位管理员在登录页创建（`GET`/`POST /api/v2/auth/admin-setup`），登录页只在没有任何管理员时展示该入口；创建时由创建者设定用户名与初始口令，动态验证器密钥一次性返回，不再需要激活材料，`tomadmin-bootstrap` 与 `administrators` 包随之移除。后续账号由已登录管理员在“管理员”页创建。管理平台“电脑节点”页可登记节点与镜像目录（`POST /api/v2/admin/computer-nodes`），节点身份与连接入口仍固定取自服务配置。

## 首位管理员命令（上一阶段，已被当前阶段取代）

> 本节记录当时的 `tomadmin-bootstrap` 入口。该命令已随管理员改为登录页直接创建而移除，现行流程见[首位管理员创建](admin-bootstrap.md)。

原方案新增[首位管理员部署入口](admin-bootstrap.md) `tomadmin-bootstrap`，使用 API 的 Linux 非 root 身份、相同服务配置和最小控制角色，提供创建、原操作查询、失败重试、隔离资源对账与激活材料签发。开户仍由 `tomd` 工作池执行，电脑能力未就绪不能激活；请求超时返回原操作，不创建第二个账户。普通部署查询不能读取其他用户的开户。

激活输出限定为当前身份拥有的 `0700` 目录内的新 `0600` 文件，先独占文件再签发、写入刷盘并复验身份。文件冲突不会废止已交付的材料，普通回执及日志不含激活值。API 与命令共用数据库归属、迁移账本检查和开户配置，未新增或改写数据库迁移，也没有改变节点协议、镜像或已运行服务。

`make lint check test` 全部通过，含 22 项 Python 测试和全部 Go 竞态测试。真实 PostgreSQL 验证幂等开户、超时查询、未就绪拒绝签发、第二次初始化拒绝、失败恢复后的查询及普通开户不可访问；文件测试覆盖链接、权限、路径替换、失败清理和回执脱敏。日志为 `/private/tmp/tombot-admin-bootstrap-full-checks.log`，定向验证为 `/private/tmp/tombot-admin-bootstrap-focused.log`。

Linux amd64/arm64 全包构建与相关静态检查通过；新增入口和文件测试在禁网、只读、无附加 capability 的真实 arm64 Linux 非 root 容器中通过，日志为 `/private/tmp/tombot-admin-bootstrap-linux.log`。只挂载新编译的测试程序，未访问业务数据库或用户卷，临时容器自动删除。

生产部署阶段继续推进独立节点程序、节点与镜像登记、真实能力探针和管理平台注册激活界面。测试中的节点和 Docker 资源是明确夹具，不能作为实际开户或内核存储、cgroup 验收证据；整个第一阶段及整体目标仍未完成。远程电脑交互功能继续暂缓。

## 桌面浏览器入口进展（上一阶段）

共享浏览器新增[每桌面的 Unix 窗口入口](../internal/sharedbrowser/README.md#每桌面的窗口入口)，固定绑定完整 Bot 执行会话。连接读取内核 UID/GID/PID，每次请求复验授权；客户端不能通过请求字段指定其他用户、Bot、执行会话或 CDP 目标。状态接口返回窗口与末次操作序号，操作、画面、页面对话框和关闭都复用共享内核。每请求独立连接，未知动作不自动重试；响应禁止缓存，错误不暴露底层协议内容。

入口显式限制连接、请求大小、请求头与读写时限。停止接单后取消调用并等待在途请求结束，关闭监听不会删除后来替换的文件。正式监督器仍须创建并核验私有 socket、实际执行子组和客户端服务端身份，不能只凭目录名或 UID 认为授权有效。

完整 `make lint check test` 通过，含 22 项 Python 测试与全部 Go 竞态检查；Linux amd64/arm64 全包构建和窗口入口 Linux 静态检查通过，日志为 `/private/tmp/tombot-browser-gateway-full-checks.log`。真实 Linux 竞态测试通过跨 UID 请求、真实 Chrome PNG、所有路由、身份字段注入拒绝、授权撤销、连接容量、请求头到期回收、在途授权退出等待和替换路径保留，并复跑共享 Chrome 全部场景；日志为 `/private/tmp/tombot-browser-gateway-linux-race.log`。测试只使用专用临时容器和容器根 cgroup，不代表生产执行子组接入已验收。

本阶段没有修改数据库迁移、代理协议或发布镜像，也没有修改已有用户卷或宿主服务。入口是正式窗口客户端的传输基础，尚未接入生产电脑代理和 XFCE 客户端；镜像实测缺少 GTK 的 Python 绑定，后续需补齐客户端、镜像依赖、用户级 Chrome 监督器以及两桌面的 GUI 联合验收。整体目标保持未完成。

## 当前共享浏览器内核进展

用户再次确认采用“共享 Chrome 内核＋每个桌面的窗口客户端”，不要求 Chrome 原生完整窗口。验收图片中的彩色页面只是测试网页，不能当作产品浏览器界面；正式客户端仍需具备地址栏、标签页、导航、弹窗与文件交互。

新增[共享浏览器内核](../internal/sharedbrowser/README.md)，通过私有 CDP 管道接管单一 Chrome 实例，使用同一常规上下文。窗口按完整 Bot 执行会话授权，分别排队；慢导航不阻塞其他 Bot。已实现创建、列表、导航、有限键鼠、真实 PNG、页面对话框、弹窗归属和定向关闭。重复末次动作复用回执，未知操作不重发，关闭回执防止旧创建请求复活页面。事件队列、窗口、回执、消息与调用时限都有明确上限。

完整 `make lint check test` 通过，含 22 项 Python 测试与全部 Go 竞态检查；Linux amd64/arm64 全包构建及共享浏览器 Linux 静态检查通过。完整日志为 `/private/tmp/tombot-shared-browser-full-checks.log`。真实 Chrome 的 Linux 竞态测试通过并发中文输入、独立截图、跨 Bot 拒绝、中文输入框和确认框、弹窗继承、慢页面并行、定向关闭、共享登录/退出/账号切换、Cookie/localStorage/IndexedDB 正常重启后保留，以及实际命名空间与 Seccomp 沙箱。另验证浏览器异常退出、启动超时、队列溢出、回执容量和未知动作不重发；日志为 `/private/tmp/tombot-shared-browser-linux-race.log`，画面和沙箱报告为 `/private/tmp/tombot-shared-browser-report-20261111`，两张页面截图已复核。

测试只运行新建的受限、禁网临时容器，使用独立非 root Chrome 和私有管道；没有修改已有用户卷、业务数据库或宿主服务。本阶段没有数据库迁移或镜像改动，沿用下节的已验收 arm64 镜像。内核尚未接入生产电脑代理、用户级服务 cgroup、profile 监督器及 XFCE 窗口客户端，下载明确禁用，上传与完整窗口交互待补齐；测试成功不代表共享浏览器产品链路已经完成。

## 当前 Agent 桌面工具与模型视觉进展

同一用户共用一个容器，每个 Bot 在其中运行独立 Agent 进程与完整桌面。正式工具集合现为 `computer_shell`、`computer_screenshot`、`computer_input`；截图与键鼠工具通过所属电脑的节点接口执行，模型配置仍由服务端统一管理。桌面工具要求视觉能力，缺失时在模型调用和计量预留前明确拒绝。输入回执仅说明发送完成，应用完成须继续观察。

新增迁移 `0043_execution_screenshots.sql`：操作取得不可改写的持久递增序号，截图与工具成功结果在同一 PostgreSQL 事务中保存。图片受用户 RLS、Bot 和运行外键约束；并发 Bot 共享用户额度。默认每用户 256 MiB、256 张，模型只装配最近 2 张，保留 7 天后分批清理已结束任务的图片。容量拒绝保存明确失败，不留下无效引用；未知写入不假装成功。历史迁移保持不变，没有连接或修改已有业务数据库。

Agent 标准流升级为版本 2，旧版本直接拒绝；消息只携带图片引用，不内嵌像素。服务端在实际模型调用前校验归属与内容并装配 PNG，OpenAI 兼容和 Anthropic 两种格式均通过真实 PNG 编码测试。引用 ID、Bot、摘要和缺失引用的篡改均被中继拒绝。桌面序号允许模型步骤和无效参数造成的空隙，旧序号与未知输入不重放；终端连续写入规则不变。

完整 `make lint check test` 通过，含 22 项 Python 测试及全部 Go 竞态检查；Linux amd64/arm64 全包构建和相关 Linux 静态检查通过。专项真实 PostgreSQL 测试覆盖原子存图、重复提交、并发额度、跨用户/跨运行拒绝、最近图片装配、清理权限与已结束任务有界回收，以及视觉能力缺失、参数失败和未知输入的完整 Worker 流程。完整日志为 `/private/tmp/tombot-vision-final-checks.log`。

最终镜像的完整电脑代理 Linux 集成测试通过，日志为 `/private/tmp/tombot-vision-final-agent-linux.log`。两个真实非 root Agent 分别在自己的 XFCE 桌面执行截图、中文输入与继续观察；补充测试核实画面确实更新后才结束，并检查共享目录中的独立任务结果，日志为 `/private/tmp/tombot-vision-observed-linux.log`，截图为 `/private/tmp/tombot-vision-observed-report-20261111`。两张最终截图已复核，分别显示各自中文命令。测试中继代替公网模型，图形成功路径使用测试启动器与容器根 cgroup，不代表生产业务 cgroup 成功入组和回收已验收。

本地最终 arm64 镜像为 `sha256:c64f91a274ea648e972c696f2a5e5aa1485e89891325fa89332ff20124af5a90`。完整镜像验收通过指定工具、编译、PDF、Chrome 沙箱、双桌面、输入隔离及共享文件；报告为 `/private/tmp/tombot-vision-final-image-report-20261111`，两张截图已复核，临时容器均已确认清理。没有更新已有用户容器、卷或宿主服务。amd64 镜像尚未实际构建运行。

配置新增截图容量、保留期限、上下文张数和桌面启动等待；模型请求默认上限提升到 16 MiB。迁移锁影响、容量、成套发布与停单排空回滚要求已同步[执行模块](../internal/execution/README.md#截图持久化与模型视觉)、[应用配置](../internal/serverapp/README.md)与[主改造方案](multi-user-refactor.md)。整体目标仍未完成：共享 Chrome 窗口服务、文件工具、公开用户桌面网关、独立节点入口与真实能力探针、客户端 v2 切换及产品端到端验收仍须继续。

## 桌面截图与输入通道（上一阶段）

本节保留上一阶段记录；其中连续序号约定、模型视觉工具待办与当前镜像版本，以本页顶部进展为准。

同一用户共用一个容器、各 Bot 独立 Agent 进程与完整桌面的约定保持不变。本轮已将桌面截图、PNG 分块读取和完整键鼠手势从[节点协议](../internal/nodeapi/README.md)接通到[电脑代理](../internal/computeragent/README.md#桌面截图与完整输入手势)及真实 X11 桌面。每次操作复验会话、控制代次和租约，私有 RFB 连接核验 UID/GID 与所属进程资源组，不接受外部显示号或连接地址。

截图只接受完整 1280×800 画面，每桌面保留最后一张不可变 PNG；图片最多 5 MiB，分块最多 32 KiB，返回总长度及摘要。截图和输入分别使用连续序号，重复输入不重新发送；中途断连保留 `uncertain` 并阻止继续输入。单次操作最多五秒，取消和撤销唤醒阻塞传输，各桌面的操作锁与缓存独立。详细错误语义、内存开销、升级及回滚要求已写入模块说明。

`make lint check test` 全部通过，含 22 项 Python 测试与全部 Go 竞态检查；Linux amd64/arm64 全包构建及相关 Linux 静态检查通过。测试覆盖全帧与残帧、像素与分块边界、中文 keysym、错误身份、序号与缓存、部分输入后不重放、取消、会话关闭和授权期限。完整日志为 `/private/tmp/tombot-desktop-io-final-checks.log`。

受限 Linux 测试通过两个真实非 root XFCE 桌面并行中文输入、独立鼠标位置、不同画面、跨 Bot 拒绝及单桌面撤销；截图已复核。节点协议通过真实跨 UID 请求及全部 18 个固定动作，代理完整集成测试也通过。日志为 `/private/tmp/tombot-desktop-io-final-linux.log`、`/private/tmp/tombot-desktop-io-node-linux.log`，图形产物为 `/private/tmp/tombot-desktop-io-verification-20261111`。节点测试使用标准 init 让受信服务保持普通进程身份，未放宽拒绝 PID 1 的生产检查。图形成功路径使用测试启动器和容器根 cgroup，不代表生产业务 cgroup 入组与回收已验证。

本地 arm64 镜像已重建为 `sha256:9e9262872685734b181dc611d04deb81569f82a68cee02b3fef8a411a5cdf46a`，打包本次代理代码。完整镜像验收通过工具、编译、PDF、Chrome 沙箱、双桌面、输入隔离及共享文件；报告为 `/private/tmp/tombot-desktop-io-image-verification-20261111`，两张最终截图已复核。没有修改已有用户卷、业务数据库或宿主服务；数据库历史迁移、配置版本 3 和记录格式保持不变，新增动作须随节点和镜像成套发布。

整体目标仍未完成：公开用户桌面网关、模型视觉工具、共享 Chrome 窗口服务、文件工具、独立节点入口、完整能力探针、客户端切换与产品端到端验收仍须继续。当前截图接口不会自动把电脑标记为完整就绪，也不把 PNG 文本伪装成模型视觉能力。amd64 镜像尚未实际构建运行。

## 宿主出网端点（上一阶段）

[节点生命周期](../internal/dockernode/README.md)已组合宿主出网端点管理和实际容器身份授权。创建/检查电脑时先建立或复验端点，再调用能力探针；删除时先记录关闭意图，结束端点连接和工作者，再停止容器与回收存储。端点绑定用户、容器 ID、实例代次及实际父 cgroup；连接时重新核对 Docker 配置与主进程，并比较对端完整父组身份。复用 [processgroup](../internal/processgroup/README.md) 定位实现，不借用虚构 Bot 会话。

节点新增稳定生命周期服务锁、显式出网策略和电脑端点容量。`NewSessions` 先完成所有旧执行回收，再逐批恢复出网；旧会话清理失败不会重新开放网络，恢复端点失败会关闭已经恢复的服务。正常停服等待在途创建和连接清理；未知退出或回收超时明确失败，不能将超时当作释放端点锁的证据。

`make lint check test` 全部通过，含 22 项 Python 测试和全部 Go 竞态检查；Linux amd64/arm64 全包构建及相关 Linux 静态检查通过。新增测试覆盖创建/关闭排序、请求期限与端点寿命分离、重复创建、容器身份变化、容量、在途创建停服、故障封闭、旧租约保持和启动恢复顺序。日志为 `/private/tmp/tombot-node-egress-final-checks.log`。

受限 Linux 容器通过真实 Unix 端点身份、稳定锁、活监听拒绝、遗留端点接管、替换文件保留与工作者退出超时测试；节点组合通过实际代理请求、环回目标拒绝、重启恢复及删除前连接关闭。日志为 `/private/tmp/tombot-egress-endpoint-linux.log`、`/private/tmp/tombot-node-egress-linux.log`。两个临时测试容器已确认自动清理。后者使用 Docker/cgroup 定位替身，不代表宿主实际容器身份、成功业务 cgroup 入组回收或公网连接已完成联合验收。

本轮不改变数据库迁移、生命周期记录版本 2、代理配置版本 3 或桌面记录版本 2；宿主节点配置新增必填出网项。没有修改用户卷、业务数据库或宿主系统服务，也未重建容器镜像；继续使用下方已验收镜像。整体重构仍未完成：独立节点程序、真实能力探针、共享 Chrome 窗口服务、其余电脑工具、客户端切换与完整产品验收仍须继续。

## 出网模块与镜像（上一阶段）

本节保留上一阶段记录；其中宿主端点与身份授权的待办，以本页顶部进展为准。

节点用户容器已改为 `network=none`，不再创建桥接网络；对账拒绝额外接口、可路由地址和网关。新增[受控出网模块](../internal/egressproxy/README.md)，提供 HTTP/CONNECT 宿主代理、每用户电脑连接额度、DNS 与地址检查、传输限制及取消回收。容器正式代理接入环回转接，共享桌面环境统一声明 HTTP/HTTPS 代理。宿主端点缺失明确返回 502，不尝试直接联网。

生命周期记录升级至版本 2，代理配置升级至版本 3，旧记录拒绝并保留，不转换旧状态；桌面记录版本 2 和数据库历史迁移保持不变。部署与回滚必须匹配节点、镜像、配置及完整状态，具体步骤已同步模块说明。没有修改已有业务库、用户卷或宿主服务。

完整 `make lint check test` 通过，包含 22 项 Python 测试和全部 Go 竞态检查；Linux amd64/arm64 全包构建及相关 Linux 静态检查通过。真实 Docker 测试通过无路由网络配置、非 root 运行和资源回收。受限 Linux 测试通过 Unix 内核身份、错误 PID/UID 拒绝、端点权限、HTTP/TLS 转发与取消回收；容器代理完整集成测试通过缺少宿主端点时明确失败、端口释放以及既有终端、图形和独立 Agent 行为。成功进程的业务 cgroup 仍使用既有测试夹具，不表示生产整组回收已完成联合验收。

检查日志为 `/private/tmp/tombot-egress-final-checks.log`，代理测试日志为 `/private/tmp/tombot-egress-agent-final.log`。最后的宿主 HTTP 日志上下文调整另通过出网竞态测试及 Linux 静态检查。最终 arm64 镜像为 `sha256:dd39d9df983c3ac230cec05542fcc9705aa1a811c896cb9a8277069ffa98d4ba`，完整工具、编译、PDF、Chrome 沙箱、双桌面、远程输入和共享文件验收通过；两张 1280×800 截图已复核。产物为 `/private/tmp/tombot-egress-final-image-verification-20261111`。最终镜像正式代理入口测试另通过，本轮临时容器已确认清理；amd64 镜像尚未实际构建运行。

**独立节点入口、宿主出网端点生命周期、实际容器对端授权、联网能力探针及共享 Chrome 代理仍待接通。** 当前模块测试不代表开户电脑已具备完整能力；后续须将这些依赖组合后继续端到端验收，整体目标保持未完成。

## 主入口组合（上一阶段）

`tomd` 已切换到 [serverapp](../internal/serverapp/README.md)，使用四种用途的 PostgreSQL 连接、认证门、管理平台、用户 API、开户工作池和容器 Agent 调度 Worker。配置与凭据文件、HTTPS、数据库角色及迁移账本在监听前校验；各用途必须指向同一明确数据库目标。仅在所有后台工作结束后关闭连接，退出超时会明确失败。

旧主入口、宿主引导、可选明文监听和 SQLite 归档子命令已退役，配套旧命令测试由新配置、TLS、权限、生命周期和组合路由测试替代；历史 SQL 文件保持不变，旧领域模块仍在逐域清理。部署模板与服务 README 已切换。没有启动或修改宿主服务、已有业务库及用户卷，也没有新增镜像构建。

完整 `make lint check test` 已通过，含 22 项 Python 测试和全部 Go 竞态检查；Linux amd64/arm64 全包构建通过。组合测试使用真实临时 PostgreSQL 与 HTTPS，覆盖两用户激活登录、机器人独立桌面身份、跨用户访问拒绝、管理通道隔离、退出撤销、就绪失联、失败启动释放连接、后台异常及退出超时。测试电脑目录是数据库夹具，不连接实际 Docker 或公网模型。完整检查日志为 `/private/tmp/tombot-serverapp-final-checks.log`。

API 健康探针明确标识 `scope: api`，不代表节点或电脑已就绪。独立节点服务、能力探针、首位管理员引导、共享浏览器窗口、其余电脑工具、客户端切换与全链路验收仍未完成，整体目标继续进行。

## 此前范围调整

以下按阶段保留实施记录；关于主入口的历史待办，以本页顶部的当前组合状态为准。

用户进一步确认：同一用户仍共用一个容器，每个 Bot 在其中运行独立 Agent 进程和完整桌面。主改造方案已同步执行位置与故障语义。模型与工具循环已提取到 `internal/botagent`，Worker 已改为向所属电脑分发独立进程，节点与电脑代理接通受限分块中继及进程监督。纯文本运行也先取得电脑授权；新 Worker 没有服务端循环回退。现有主入口尚未组合，真实业务 cgroup、完整电脑工具和产品端到端验收仍待完成。

新增[容器 Shell 工具](../internal/computertools/README.md)及执行适配，使用已记账的操作 ID 和完整桌面身份调用受管终端。命令、结果和各次节点调用均有上限；启动不重发，读取按游标排空，非零退出回灌 Agent，超时或通信失败进入未知状态并清理所属运行。沿用当前代码取消审批等待的规则，保留特权模式拒绝。真实 PostgreSQL 已验证记账顺序、会话归属和失败清理；两个真实容器 Agent 已完成并行 Shell 与共享文件测试。主入口、其余电脑工具和真实业务 cgroup 联合验收仍待完成。

本轮推进主入口所需的[受保护节点 API](../internal/nodeapi/README.md)，让非 root API/Worker 通过固定本机协议调用 root 节点，无需 Docker 权限。已实现完整生命周期、会话、终端、图形状态及 Agent 接口，固定 UID/GID 校验、有界连接、规范消息和不重发传输；调用及执行租约分别携带绝对单调时钟期限，节点继续沿用原持久命令和版本屏障。真实跨身份进程验证通过；独立节点程序、能力探针与 PostgreSQL 主入口仍待组合，当前 `tomd` 仍为旧入口。

用户已明确要求同一用户名下所有 Bot 共用一台远端电脑，业务文件、安装工具和浏览器账号状态全部共享；不同 Bot 同时执行，各自拥有完整 Linux 图形桌面，截图、鼠标键盘和浏览器窗口不能跨 Bot 操作。用户同时确认登录、退出和账号切换影响所有 Bot。不同用户的容器与数据隔离保持不变。

目标方案已经更新。`0041` 已建立稳定桌面身份与机器人归属，`0042` 已替换电脑级独占索引和用户级控制代次，实现桌面级独占、独立代次及用户并发额度。节点和代理已替换为按桌面独立持久记录、授权和回收；任务引导也已改为稳定 Bot UID、公共 GID 和共享业务文件访问。共享卷与图形基础目录已在代理启动时准备；每次执行的私有桌面目录与受管终端已接通代理和节点，图形启动与状态查询已接通，屏幕网关、独立 Bot Agent 和主服务仍待组合，下面历史测试只证明各模块当时的行为。后续继续接通桌面、窗口归属和用户进程，并把共享浏览器服务与单个运行的回收分离；历史迁移脚本不修改。

共享浏览器方向已完成[真实 Chromium 基础验证](../verification/README.md)：单一持久配置下两个独立窗口通过各自 CDP 会话并发输入，Cookie、登录、退出、账号切换、localStorage 和 IndexedDB 共享，关闭一个窗口不影响另一个，重启后账号状态保留。画面截图已复核。这是浏览器协议验证；完整 Linux 桌面已在专用镜像中另行验收，窗口客户端、主服务图形会话和跨 Bot 画面授权仍需接入及端到端验证。

远程电脑镜像规格进一步确定为 Debian 13（Trixie）、XFCE、xfwm4、X11 和 1280×800。新增[专用镜像目录](../deploy/computer/README.md)，按语言运行时、常用命令行工具、桌面软件预装用户指定的完整清单，使用官方版本与摘要校验；包括独立图形会话启动程序、工具检查及双桌面验收脚本。镜像实际构建和图形验收结果以本文后续验证记录为准；这不表示多桌面控制、代理与主入口已完成联合接入。

本地 `tombot-computer:trixie` 的 linux/arm64 镜像已构建并通过完整镜像验收：两个中文 XFCE 桌面使用独立 UID、X11 认证、D-Bus 和私有 RFB socket，实际远程键盘与鼠标输入互不干扰，文件与自装工具共享，跨桌面访问被拒绝，单桌面异常不影响另一桌面。所有指定工具均检查版本，并实际执行 Python 环境、Go/Rust/C 编译和 LibreOffice 转 PDF；Chrome 通过私有 CDP 管道执行页面并确认命名空间及 Seccomp-BPF 沙箱。新增的 seccomp 配置保留 Docker 默认拒绝策略，仅补充浏览器所需四项系统调用；没有关闭浏览器沙箱或增加 SYS_ADMIN。节点 tmpfs、seccomp 和共享内存配置已对齐，代理启动会准备共享卷与图形基础目录；图形启动和监督已接入，屏幕输入网关及主入口组合仍待实现。amd64 尚未实际构建与验收。

## 当前基线

实施从提交 `1946c40` 开始。该版本在方案初稿之后增加了自定义头像、机器人主动发消息、群聊与私信协作等功能，最新历史迁移为 `0028_custom_cat_profiles.sql`。这些功能需要在新领域模型下接入，不能以旧文档快照覆盖当前实现。

现有 `0001` 至 `0028` 脚本保持不变。新 PostgreSQL 链从 `0029_multitenant_foundation.sql` 开始，不读取 SQLite，也不提供数据转换。

## 已落地的基础能力

- 新增 [PostgreSQL 存储模块](../internal/store/postgres)，连接池按租户、身份、控制和调度用途分离，拒绝超级用户、迁移所有者、危险权限继承及混合权限组作为业务连接。
- 建立用户、会话摘要、电脑与创建操作、全局模型策略、机器人、群组、用户偏好和事件等首批目标实体。租户表强制行级安全，复合外键限制跨用户关联。
- 租户数据库操作使用事务局部用户上下文；用户切换及失败回滚后不会把身份遗留到池中连接。快照接口采用一致性只读事务。
- 用户事件在业务事务中分配序号并写入，通过每用户事件头锁保证提交与回放顺序；不使用全局自增序列充当提交水位。
- 新增独立数据库初始化命令 [tomdb](../cmd/tomdb/main.go)，使用外部配置的迁移连接；空库初始化、重复执行、历史摘要校验及失败回滚由新执行器负责。
- 新增 [账户与会话模块](../internal/identity/README.md)，实现用户激活、管理员动态验证登记、账户登录、访问与续期凭据分离、轮换重放撤销、改密、退出、设备撤销和账户/来源限流。口令计算有并发上限，不持有数据库事务。
- 新增 `0030`、`0031` 身份迁移，约束激活时电脑必须就绪、验证材料所属关系及最后一名有效管理员；既有 `0029` 脚本保持不变。
- 新增 [v2 认证 HTTP 模块](../internal/authapi/README.md)，分别提供桌面凭据通道与管理端安全 Cookie，管理写操作先验证来源及绑定续期会话的 CSRF，再进入身份服务；协议及 PostgreSQL、HTTPS 联合测试覆盖两用户完整认证流程。
- 新增 [同步开户模块](../internal/accounts/README.md) 及不可变 `0032` 迁移。管理员开户和首位管理员引导共用持久操作，预留容量、独立工作者、租约及控制代次、节点对账、失败补偿和隔离恢复均已实现；激活材料只能在电脑就绪后签发。
- 新增 [管理业务 HTTP 模块](../internal/adminapi/README.md)，实现开户、进度查询、失败重试、隔离对账及激活签发；统一复用认证门，只有最终就绪才返回创建成功，请求超时返回 `504` 与操作 ID。
- 新增 [Docker 启动引导模块](../internal/dockerbootstrap/README.md)、`tomdocker-bootstrap` 命令、安装清单 Schema 及 systemd 前置单元。支持 Ubuntu 24.04 的固定版本官方软件源安装、已有版本复验、独占引导锁、受保护文件和失败状态记录；尚未接入节点启动链。
- 新增 [受信 Docker 节点模块](../internal/dockernode/README.md)，实现固定 Docker API、每用户持久锁与控制记录、资源身份及配置核验、创建对账、先停容器再删卷的清理流程，以及新旧代次物理资源分离。已接入独立代理控制目录和节点会话协议，完整电脑能力探针和主入口仍未接入。
- 新增 [固定容量持久磁盘后端](../internal/dockernode/storage.md)，实现预分配 ext4 镜像、设备身份核验、重新挂载、格式化结果未知时只读检查、忙挂载及延迟解除关联时保留资源。完整内核挂载验收等待明确授权，不能宣称生产硬配额已验收。
- 新增 [服务端模型目录与策略模块](../internal/modelregistry/README.md) 及 `0033`、`0034` 前向迁移，实现不可变配置版本、凭据加密轮换与退役、能力验证后的原子发布、用户和 Bot 策略分配，以及固定版本选择和逐次撤销检查；新运行模块已接入持久版本绑定。
- 管理 HTTP 已扩展到模型目录、凭据版本、策略发布与撤销、默认策略及用户和 Bot 分配；统一执行管理权限、CSRF、路径归属、分页和并发修订检查，不提供普通用户模型配置或任意模型代理。
- 新增 [受管模型网关](../internal/modelgateway/README.md)，实现部署限定来源、逐次 DNS 与地址检查、TLS 验证、有界流式调用及文本、工具、视觉能力探测。生成请求不自动重放，实际用量缺失明确保留未知；运行额度与用量已由新执行领域接入，生产主入口组合仍待完成。
- 新增 [用户机器人与协作目录](../internal/bots/README.md) 及 `0035`、`0036` 前向迁移，实现按用户隔离的机器人、群组、唯一会话目录、修订控制、创建幂等、归档恢复、工作空间选择及自定义 PNG 头像。主动消息与私信权限作为机器人配置保留，实际消息投递及运行调度仍待接入。
- 新增[桌面身份登记](../internal/computerdesktop/README.md)及 `0041` 前向迁移，机器人创建同事务分配稳定桌面 ID、独立 Linux UID、显示编号和公共文件组。双向延迟外键、RLS、唯一约束及不可变触发器保护归属；归档恢复、幂等重试和电脑实例重建保留身份。v2 用户 API 返回 `desktop_id` 并拒绝外部指定运行映射，节点和代理尚未使用该身份启动桌面。
- 新增 [v2 用户资源 API](../internal/userapi/README.md)，复用桌面认证入口，不接收用户身份覆盖或客户端模型配置。真实 PostgreSQL 与 HTTPS 已联合验证两账户激活、登录、资源操作、头像、群组、退出和禁用，未接入当前主入口与桌面端。
- 新增 [持久消息与运行模块](../internal/execution/README.md) 及不可变 `0037` 迁移，原子保存单聊消息、回合、运行、队列和事件；实现用户公平领取、Bot 运行互斥、租约与代次校验、固定机器人快照及模型版本、操作去重、取消和有界崩溃恢复。外部结果未知时明确终止，不自动重发。
- 用户 API 已扩展到消息受理与分页、已读、运行查询和取消；群聊、私信、主动消息及例程编排仍待实现。受理返回持久任务 ID，不虚报模型执行成功；生产 Worker、电脑执行和实时事件尚未挂入主入口。
- 新增 [模型额度与真实用量](../internal/modelusage/README.md) 及 `0038`、`0039` 迁移。模型操作与用户并发名额、月度保守预留原子提交，供应商计数、未知状态、缓存语义和可选费用独立保存；迟到证据受限追加并对账，不能恢复旧 Worker 的运行写入权。
- 新增 [执行 Worker](../internal/execution/worker.go)，组合持久领取、固定模型、逐次撤销检查、模型与工具循环、用量结算、续约、取消及后台恢复。已知结果恢复不重复外部调用，工具新动作复验当前权限；电脑与协作工具的生产适配、真实进程清理和主入口组合仍待完成。
- [共享电脑执行权](../internal/computercontrol/README.md) 已通过 `0042` 从 `0040` 的电脑级独占改为桌面级独占，同一用户可在显式配置的桌面并发额度内并行；控制代次按桌面递增，待启动与清理未知仍占额度。会话绑定完整持久桌面身份，Worker 操作记账再次核对；用户运行查询和事件暴露等待原因。原清理保护与有限恢复保留，真实节点、代理和主入口仍须接通。
- 新增[节点会话控制器](../internal/dockernode/README.md)及 [Linux cgroup 后端](../internal/processgroup/README.md)，实现持久开启与关闭屏障、单调时钟续约、有限到期回收、重启前清理、固定实例资源组及整体终止；生命周期删除也受会话清理约束。节点已按桌面分别保存会话与期限，并通过生命周期共享锁和桌面独占锁支持并发调用；代理多授权已接通，真实用户进程和内核回收验收仍待完成。
- 新增[容器内任务引导](../internal/taskprocess/README.md)与 `computer-task`，实现不可改写的启动内容、固定入组入口、关闭控制描述符、全线程身份降权、权限复验和原位执行。协议版本 2 从授权桌面取得稳定 UID 与公共 GID，工作目录在降权后按普通文件权限访问，普通文件及工具通过公共组共享；私有图形目录仍检查身份、权限和链接。终端与 XFCE 使用同一份环境模板。只读撤销管道在执行用户代码前复验，阻止迟到启动；实际受限 Linux 容器已验证多线程降权、描述符关闭、共享读写与工具执行、跨桌面目录拒绝和撤销拒绝，成功入组仍须真实 cgroup 联合验证。每次执行的私有桌面目录及终端运行器已接入代理和节点协议，当前主服务运行入口尚未切换。
- 新增[节点与代理描述符通道](../internal/agentchannel/README.md)，实现顺序包有界消息、内核对端身份、原子执行关闭标记、异常帧描述符回收及同一时间命名空间的绝对单调时钟期限。节点新增限定入口授权、同步现有期限和容器对端复验；传输和重发不延长租约。
- [容器代理会话控制](../internal/computeragent/README.md) 已改为容器级 `Controller` 和每桌面独立守卫，按稳定身份校验 Bot、UID 和显示编号，分别持久化关闭屏障、租约及撤销管道；容器并发上限原子预留，未知关闭继续占用。各桌面看门狗独立运行，单桌面任务或刷盘阻塞不妨碍其他桌面，致命失控升级为整个容器退出。`computer-agent` 入口和控制协议已组合多桌面控制器；图形启动与受管终端已接通，独立 Bot Agent、屏幕网关及完整失联回收仍待联合验收。
- 节点生命周期新增每实例代次的独立 root 控制目录及固定挂载，创建前刷盘配置，对账拒绝配置、目录和挂载漂移；清理保留控制记录。节点会话已等待代理开启及续约确认，失败隔离；关闭未确认仍尝试宿主清理，不能报告交接成功。节点和代理均已替换原用户单会话存储和锁范围，真实 Unix 联合测试验证同容器双桌面授权和定向关闭。
- 新增 [Web 管理平台](../../admin/README.md)，接入管理员认证、身份复验、用户分页、同步开户与进度、恢复和激活，以及供应商、模型、凭据和策略版本操作。主题、按钮、表单及弹窗外观已与客户端共用 [ui](../../ui/README.md)；前端调用真实 v2 协议，测试替身不进入生产构建。
- 管理领域新增用户、节点和镜像分页查询，节点目录不返回连接地址；新增 [Go 静态资源处理器](../web/admin/page.go)，启动时检查有限构建产物并设置同源 CSP。主入口组合、管理端剩余账户变更、分配、用量、审计与电脑运维功能尚未完成。
- 修正 [Makefile](../Makefile) 中引用已删除 Flutter 目录的检查命令，保留 Go 格式、静态检查、构建、竞态测试及 Python 脚本测试。

`tomd` 当前已切换到新组合。下列旧模块仍是重构过程中待替换或删除的内容，不是最终兼容层；主入口验收不能代表节点、客户端及整个产品已完成。

## 验证环境与证据

桌面原始基线已通过 `npm run check`，包含格式、静态、类型检查和 240 项测试。服务端原始基线已通过 `go test ./...`；新增基础与身份模块后，已通过 `make lint check test` 的格式检查、静态分析、构建、Python 脚本测试及完整 Go 竞态测试。

新 PostgreSQL 集成测试使用本机 PostgreSQL 18，在临时私有目录中初始化独立集群，关闭 TCP，仅通过私有 Unix 套接字连接；测试结束停止进程并清理目录，不连接或修改开发者已有数据库。缺少测试二进制时测试直接失败，不跳过验证。通过 `TOM_TEST_POSTGRES_BIN` 指定包含 `initdb`、`pg_ctl` 和 `postgres` 的目录，或将这些程序加入 `PATH`。

已验证的回归场景包括：两用户同名机器人、无上下文拒绝读取、连接复用、跨用户读写和外键、身份及管理角色无内容权限、危险数据库角色拒绝、业务与事件共同回滚、并发事件提交顺序、取消事务、并发初始化、历史摘要篡改、非空未知数据库拒绝及错误脚本完整回滚。该测试组已通过竞态检查。

身份模块额外验证了激活故障回滚、并发激活、设备跨用户访问、会话范围、续期重放和并发轮换、禁用与到期、改密撤销全部会话、管理员动态码重放、设备限额、跨实例限流、审计失败回滚、损坏认证记录及最小权限。验证码测试使用标准公开向量，集成场景分开初始化，避免依赖测试耗时恰好落在同一时间窗口；没有延长生产验证窗口或关闭过期检查。

同步开户模块通过真实 PostgreSQL 竞态测试：并发幂等、容量冲突回滚、等待超时后继续执行、节点回包丢失对账、最终事务失败恢复、清理前不释放容量、隔离恢复、旧租约拒绝、首位管理员失败重试，以及开户、激活轮换、激活和登录的联动。节点端使用故障注入替身，这些测试没有创建真实用户容器。

Docker 引导已通过 Linux amd64、arm64 交叉构建及安装流程、权限、失败状态和进程取消的测试。另通过显式只读检查核实本机 Docker 29.7.2 为 Linux、cgroup v2 且具备基础资源限制能力；未改动现有容器。Ubuntu 全新主机自动安装、发布版安装清单、电脑镜像、真实磁盘配额及网络隔离仍未验收，不能以本机引擎检查替代。

本轮新增开户、管理协议与 Docker 引导后，再次执行 `make lint check test`，格式、静态分析、构建、4 项 Python 脚本测试及完整 Go 竞态测试全部通过。桌面代码本轮未修改，前述桌面检查属于原始基线结果。

新增节点模块后也通过完整服务端检查及 Linux amd64、arm64 构建。节点测试覆盖旧控制者、并发创建、重启恢复、未知结果、清理顺序、资源漂移及不安全状态文件；并发首次创建锁文件的回归连续执行 20 次通过。

实际 Docker 生命周期测试已通过，验证生产容器配置的实际表示、独立非 root 容器、内部网络、占用卷拒删和确认停止后的清理。只使用本地固定摘要 Alpine 镜像及本次新建的空白测试卷；结束后只读检查未发现测试容器、卷或网络遗留。这仍不是完整用户电脑验收，不证明生产硬配额、浏览器、受管进程、文件服务或出网策略已可用。

存储后端新增后再次通过完整服务端检查及竞态测试，脚本测试扩展到 7 项。无额外权限的真实 Linux 容器已完成 512 MiB 镜像预分配、ext4 格式和只读完整性检查，测试资源已回收。高权限 loop 探针被自动审批以权限范围过大且缺少明确授权为由拒绝；未绕过拒绝，已准备隔离内核验收脚本并询问授权，挂载、写满拒绝和设备回收仍待实际证明。

模型目录的真实 PostgreSQL 竞态测试已覆盖：目录分页、跨用户 Bot 拒绝、策略优先级与不可用不回退、并发版本分配、发布竞争、探测失败和超时、探测期间撤销、旧运行版本保持、密文替换拒绝、主密钥轮换、条件退役及审计回滚。新增模块和两项迁移后，完整 `make lint check test` 再次通过，包含 7 项 Python 测试和全部 Go 竞态测试。能力探测使用可控替身验证服务边界，尚未对真实模型供应商执行能力验收。

新增模型管理 API 和受管网关后，完整 `make lint check test` 再次通过。管理协议验证全部目录、发布、退役及分配路由和权限边界；网关用真实本地 TLS 服务验证请求与 SSE、文本随机标识、工具结构和随机图像探测，以及 DNS 重绑定、禁止地址、内网许可、转换地址拒绝、证书、重定向、超限、排队、取消与超时。测试替换 DNS 和拨号目标以限定在自有本地服务，不请求公网供应商。这证明传输与探测实现的协议行为，尚不证明生产模型能力、主服务组合或用量账本可用。

真实用量解析新增缺失、明确零值和非法计数测试；三处历史费用集成测试补全模拟供应商的输出计数，原有调用次数及费用断言保持不变。新网关不采用旧运行链的字符估算用量。文档本地链接与 `git diff --check` 均已通过。

机器人与协作目录新增后，完整 `make lint check test` 再次通过，包含格式检查、静态分析、构建、7 项 Python 测试和全部 Go 竞态测试。数据库测试覆盖并发同键创建、修订竞争、两用户同名与隔离、会话及头像复合外键、RLS、群邀请及成员更新回滚、归档名称恢复冲突、容量、头像规范化与完整性、账户禁用、设备撤销和会话过期。HTTPS 联合测试实际完成两账户激活登录和全部已实现目录路由，验证身份字段及模型字段注入拒绝、跨用户读取与修改拒绝、未知错误脱敏、退出撤销和禁用生效。

新增持久运行与消息 API 后，完整 `make lint check test` 再次通过。真实 PostgreSQL 测试覆盖并发同键入队、原子回滚、公平领取、并发任务互斥、过期和旧代次拒绝、固定配置不可改写、操作结果去重、取消、已知结果恢复、未知结果不重发、恢复次数耗尽，以及上下文、操作数和排队容量限制。HTTPS 测试新增消息入队、重试、分页、已读、运行与取消、跨用户拒绝、非法正文和模型字段注入、会话撤销及未接入群编排明确拒绝。这些测试不执行真实模型调用或电脑命令，不代表完整聊天、工具和群协作已验收。

新增用量与 Worker 后，完整 `make lint check test` 再次通过，包含格式、静态分析、构建、7 项 Python 测试和全部 Go 竞态测试。计量测试覆盖并发额度与名额、固定配置校验、未知预留、真实零值、缓存拆分、费用缺失与补齐、溢出、部分计数下界、迟到证据和原运行终态保护。Worker 测试实际执行两个用户的后台领取、模型工具循环、回复与用量提交，并验证最终提交失败后模型与工具不重放、长调用续约、用户取消、权限撤销和非法输出。测试中的供应商及工具使用可控回调，不调用公网模型或真实电脑；网关自身的真实本地 TLS 测试仍独立执行。主服务、电脑工具和前端尚未通过组合验收。

新增共享电脑执行权后，完整 `make lint check test` 再次通过，包含格式、静态分析、构建、7 项 Python 测试和全部 Go 竞态测试。真实 PostgreSQL 测试覆盖同用户独占及不同用户并行、并发申请去重、队列上限、等待取消及超时、最大持有时间、错误节点回包、清理事件回滚、有限重试、旧清理不影响新会话、数据库禁止提前换代，以及电脑到期与最终回复竞争。Worker 联合测试验证等待时不占模型槽、模型思考期间保留电脑、清理失败不报成功和恢复不重放电脑动作。节点为受控协议替身，没有启动真实用户命令；实际进程回收、隔离后的容器恢复、人工接管、多阶段编排和等待时释放 Worker 名额仍待实现。

初始化命令为 `make database-migrate`，连接通过 `TOM_DATABASE_MIGRATION_URL` 外部注入。该命令只处理新 PostgreSQL 版本，不创建旧数据库映射，也不会清空无法识别的数据库。完整服务端验证入口为 `make lint check test`；专用镜像新增后再次通过该入口，包含 14 项 Python 测试及全部 Go 竞态测试。

新增节点会话与 Linux cgroup 后端后，完整 `make lint check test` 再次通过，包含格式、静态分析、构建、7 项 Python 测试和全部 Go 竞态测试；Linux amd64、arm64 构建通过。新增测试验证重复命令不延长租约、迟到关闭不影响新会话、并发容量上限、过期拒绝入组、有限重试、节点单实例、重启先清理、计时到期，以及损坏记录阻止执行与删除。另修正回收前置失败绕过次数上限的问题，覆盖锁等待超时和损坏记录的退避与耗尽。资源组使用受控替身，未在宿主创建 cgroup 或启动用户命令；真实进程回收、容器代理和看门狗仍须实际 Linux 验收。

新增管理平台后，服务端完整 `make lint check test` 通过；管理端 `npm run check` 通过格式、静态、类型、12 项协议及界面测试和生产构建；客户端共享样式移动后，`npm run check` 的 240 项测试及 `npm run build` 通过。真实 Chromium 在正式 Go 静态处理器和生产 CSP 下完成浅色用户页、深色模型页、窄屏布局、Esc 关闭与焦点恢复、模型草稿及发布流程，未出现脚本异常或 CSP 违规。浏览器回归还核对草稿创建后必须读取新修订再发布；验证器使用不依赖动态编译的完整校验模式，没有放宽 CSP。截图已逐张复核，业务响应由测试拦截；这不替代真实 PostgreSQL、身份 Cookie、模型供应商和容器联合端到端验收。用户及电脑目录的真实 PostgreSQL 测试另已覆盖分页、资源关联、撤权、取消和敏感字段排除。

新增容器内任务引导后，完整 `make lint check test` 再次通过，包含 7 项 Python 测试和全部 Go 竞态测试；Linux 专用静态检查及 amd64、arm64 引导程序构建通过。固定摘要 Alpine 的临时容器使用无网络、只读根文件系统、默认 seccomp、不可提权和六项受限权限，实际验证 memfd 封存、目录链接拒绝、多线程降权、附加组及 capability 清除、描述符关闭、显式环境和程序启动失败。新增跨 UID 测试证明：任务不能修改 root 持有的角色目录权限，即使放宽内部 HOME 权限，另一任务身份仍不能进入。伪造入组描述符在用户代码执行前被拒绝。该验证没有创建真实会话 cgroup，成功入组和整树回收仍待联合内核验收；测试容器已确认删除，没有触碰既有用户卷。

新增描述符通道、代理会话控制和撤销管道后，完整 `make lint check test` 再次通过，包含 7 项 Python 测试和全部 Go 竞态测试；Linux amd64、arm64 全部包构建及 Linux 专用静态检查通过。受限 Linux 临时容器实际验证顺序包通信、描述符传递与执行关闭标记、异常帧无描述符遗留、期限不因传输延长、客户端和服务端协议、取消及连接上限，以及撤销管道阻止迟到执行。故障注入覆盖未知保存结果不能降低版本、任务启动持锁及保存阻塞期间仍可撤销、独立终止失败、有限回收和致命状态拒绝后续操作。资源组使用受控替身，未实际创建或终止宿主 cgroup；生产容器对端鉴别、终端链路及完整失联回收仍待联合验收。测试容器已只读确认自动删除，没有挂载既有用户数据或 Docker socket。

新增代理入口、节点绑定和控制目录后，完整 `make lint check test`、Linux amd64/arm64 构建及 Linux 专用静态检查再次通过。受限容器实际运行代理二进制，验证启动、同一 PID 空间 root 对端拒绝、双实例拒绝、SIGTERM 退出、监听回收和无效配置失败；真实 Unix socket 联合验证节点开启、续约、关闭及对端容器归属拒绝。故障测试覆盖代理未知结果、定向宿主清理、配置漂移及停服阻塞，资源组仍为替身。真实 Docker 生命周期回归也验证了新增控制挂载的实际表示和测试资源清理。上述验证没有启动完整图形桌面或真实用户命令。

新共享浏览器要求通过 `make test-shared-browser` 使用独立 Chromium 158 配置及本地站点验证，生产浏览器配置未被读取。两个不同窗口并发接收鼠标及文本输入，各自内容与截图正确，站点 Cookie、localStorage 和 IndexedDB 共用，账号切换与退出传播，关闭单窗及重启行为符合预期。测试还确认页面视口与窗口渲染尺寸；完整 Linux 桌面、窗口客户端和浏览器网关权限不在此项验证范围。

桌面身份登记接入后，`make lint check test` 全部通过，包含格式检查、静态分析、构建、14 项 Python 测试及全部 Go 竞态测试。新增真实 PostgreSQL 测试覆盖无需目录锁的并发编号分配、创建幂等、跨用户读写、错误归属、归档恢复与容器换代、身份不可回收、容量耗尽、事件失败及锁等待取消。迁移测试确认空目录可升级、已有机器人时拒绝转换并保留原数据；HTTPS 测试验证稳定桌面 ID 和运行身份注入拒绝。本轮不改变镜像运行程序，也未启动生产桌面或接入并发控制。

桌面执行控制接入后，完整 `make lint check test` 再次通过，包括格式、静态分析、构建、14 项 Python 测试及全部 Go 竞态测试；Linux amd64、arm64 全包构建通过。真实 PostgreSQL 新增验证同用户双桌面同时准入、独立控制代次、并发额度竞争、单桌面清理失败时其他桌面继续续约、取消队首不阻塞新准入、等待原因和事件去重、物理身份替换拒绝、数据库同桌面唯一约束及容量兜底。开户验证显式额度与幂等保留，迁移验证旧限额拒绝转换后保留原数据。完整回归发现并修复首次身份登记的复合唯一索引竞态；加强后的 32 路并发回归连续执行十轮通过。上述控制测试使用受控节点，不执行真实用户命令；节点多记录、代理多授权和共享浏览器仍需联合验收。

节点改为桌面独立持久记录和锁后，完整 `make lint check test` 再次通过，包括 14 项 Python 测试及全部 Go 竞态测试；Linux amd64、arm64 全包构建通过。新增测试验证真实重叠的同用户桌面调用、生命周期互斥、同桌面串行、按桌面原子预留容量、独立隔离和重试、重启逐条回收、旧格式及错误归属拒绝，并修复首次关闭失败没有进入后台回收和停服集合的问题。已在 arm64 专用镜像的受限临时容器中运行同组测试、描述符委派及真实 Unix 节点代理绑定测试，全部通过。首次测试直接以 PID 1 运行时被正式对端检查拒绝，改用与正式配置一致的 Docker init 后通过，未放宽检查。资源组仍使用替身，未执行真实用户命令或重建镜像内代理；多桌面代理和完整电脑联合验收仍待完成。

代理多桌面控制器接入后，完整 `make lint check test` 再次通过，包括格式、静态分析、构建、14 项 Python 测试及全部 Go 竞态测试；Linux amd64、arm64 全包构建通过。新增验证独立桌面身份与授权、同容器并发额度竞争、任务和刷盘阻塞不妨碍其他桌面、关闭未知保留占用、独立到期、初始化失败后重新核验、旧记录与冲突身份拒绝、致命故障封闭接单及有界停服。两个受限 arm64 临时容器分别验证全部代理测试和节点联合协议，正式代理入口、真实 Unix 描述符通道及双桌面定向关闭均通过，容器已确认自动删除。测试程序和代理为本次编译，资源组仍使用替身，未挂载宿主 cgroup 或已有用户卷；此前镜像内代理未更新，实际桌面、用户进程和共享浏览器仍须联合接入。

任务引导改为稳定桌面 UID、公共 GID 和共享业务路径后，完整 `make lint check test` 通过，包括格式、静态分析、构建、15 项 Python 测试及全部 Go 竞态测试；Linux amd64、arm64 全包构建和任务引导专用静态检查通过。受限 arm64 容器实际验证两个 Bot 读写同一文件、执行共享工具、共享临时目录、通过普通链接进入业务目录，同时拒绝跨桌面控制目录、伪造入组和已撤销授权。首次验收发现最小权限下测试准备目录丢失 setgid 位，改为以公共组执行授权并复验实际权限后通过，未增加 capability 或降低断言。生产目录准备须遵守相同内核约束。

本地镜像已重建为 `sha256:0ff0007d33b03460a0ea58d89d6b1f1e646cb63567fbb5c5edbea9b3bf2694b8`，包含多桌面代理、版本 2 任务引导及 Go/Python 共用环境模板。完整镜像验收再次通过指定工具、真实编译与 PDF 转换、Chrome 沙箱、双 XFCE 桌面、独立输入、文件与工具共享、跨桌面拒绝和定向关闭；两张 1280×800 中文桌面截图已复核，测试容器已确认删除。产物位于 `/private/tmp/tombot-shared-environment-image-verification-20261111`。实际业务 cgroup 入组回收、生产运行目录和主服务、浏览器窗口及客户端仍未联合接入；amd64 镜像尚未实际构建运行。

节点图形运行配置与代理共享目录准备接入后，完整 `make lint check test` 通过，包括格式、静态分析、构建、20 项 Python 测试及全部 Go 竞态测试；Linux amd64、arm64 全包编译和代理专用静态检查通过。新增测试覆盖空白卷、重复启动保留业务文件、旧数据拒绝、链接与权限漂移、空目录中断恢复、受限初始化输出及业务卷 `noexec` 拒绝。实际 Linux 验收发现并修正新目录继承 setgid 的初始化状态；macOS 测试显式准备目录归属，未放宽生产权限检查。

真实 Docker API 验证了节点新 seccomp、tmpfs 和共享内存配置的保存、对账及生命周期。重建的 arm64 镜像为 `sha256:e7ad59fe94c813f89f73cde412af265a6af517a3e35c7421e82c5ead31adcd07`；在受限容器运行镜像内正式代理，证明共享目录和图形 socket 目录在监听前准备，目录异常带电脑标识报错且不接单，原有双实例拒绝、对端检查和有界退出保持通过。完整镜像工具、Chrome 沙箱、双桌面及共享文件验收再次通过，两张 1280×800 截图已复核，产物为 `/private/tmp/tombot-runtime-layout-image-verification-20261111`。临时容器、卷和网络均已确认清理。仍未接通每次执行的私有桌面目录、终端和图形启动、共享浏览器窗口及主服务，未完成真实 cgroup 或存储内核联合验收。

本轮接通每次执行的私有桌面目录及受管终端。代理统一准备 NSS/XDG 身份状态，XFCE 只复验并读取；回收确认前不清理目录，清理不跟随链接进入共享业务卷。节点和代理新增终端启动、游标读取、带序号输入及尺寸协议，复用完整桌面身份和现有租约。固定引导仍负责入组和降权；启动去重、部分输入回执、尾部输出排空、有限缓冲和终端独立退出已实现，未切换主服务或重放未知操作。

`make lint check test` 通过，包含 21 项 Python 测试和全部 Go 竞态测试；新增节点转发及终端边界测试随后单独复验通过。Linux amd64/arm64 全包构建和相关静态检查通过。受限 arm64 容器实际验证桌面目录生命周期、两个 UID 的图形权限、终端中文与输入去重、尺寸和非零退出、双终端互不终止、阻塞写入超时，以及固定引导拒绝伪造入组入口。真实 Unix 通道覆盖终端协议。成功 PTY 测试使用专用测试进程，真实业务 cgroup 成功入组与整组回收仍待联合验收。

镜像重建为 `sha256:636a36d54cc2e449a80e12503410b35854bb31450f1681fd241e699548bbd7b1`，包含本轮代理和只读身份的 XFCE 启动器；完整工具、Chrome 沙箱、双桌面、共享文件与远程输入验收全部通过，两张 1280×800 中文截图已复核。产物为 `/private/tmp/tombot-managed-terminal-image-verification-20261111`，日志为 `/private/tmp/tombot-managed-terminal-checks.log`、`/private/tmp/tombot-managed-terminal-focused.log` 和 `/private/tmp/tombot-managed-terminal-agent-final.log`。临时容器已确认清理；amd64 镜像、真实存储/cgroup、共享浏览器窗口及主入口联合验收尚未完成。

本轮继续接通图形会话：节点和代理提供异步发起及状态查询，固定引导启动当前 Bot 的 XFCE；就绪文件原子发布，代理随后核验私有 socket、内核对端、执行资源组及真实 RFB 初始化结果。启动总期限、握手取消、核心退出和定向撤销均有明确结果；修复并新增回归覆盖“握手中总期限耗尽却未报告失败”的分支。终端与图形调用共用授权和期限检查，未知启动不会自动重放。

完整 `make lint check test` 通过，包含 22 项 Python 测试和全部 Go 竞态测试；Linux amd64/arm64 构建及相关静态检查通过。受限容器实际运行两套降权 XFCE，验证 Go 监督与私有 RFB 就绪、错误 UID/资源组拒绝、退出报告及另一桌面继续连接；固定引导仍拒绝伪造入组入口。Unix 协议和节点测试覆盖正常状态、错误响应、过期/旧代次及未知启动不重放。真实成功图形进程测试使用临时容器根组，生产业务 cgroup 成功入组和整组回收仍未联合验收。

最终 arm64 镜像为 `sha256:46f9f1cf4abfcad46771b3999222bba8a4338e1b32af9ebdc1040eac93610eea`，本轮镜像验收产物为 `/private/tmp/tombot-graphics-final-image-verification-20261111`；检查及代理测试日志为 `/private/tmp/tombot-graphics-checks.log` 和 `/private/tmp/tombot-graphics-agent-final.log`。独立 Bot Agent、模型与持久操作中继、共享浏览器窗口、屏幕输入网关和主入口仍需继续实现，整体目标保持未完成。

上一阶段新增 [Bot Agent 模块](../internal/botagent/README.md)和 `cmd/bot-agent`，将模型与工具循环从 Worker 提取为唯一实现；原 Worker 的单次授权、用量与持久恢复移入中继适配。独立进程使用有界私有标准流，拒绝错序、错误结果类型、超限与部分写入；工具参数以原始 JSON 跨进程传输，专项回归证明大整数与字段顺序不变。没有新增或修改数据库迁移。

真实容器测试复现并修复标准输入阻塞时取消不能及时退出的问题，入口现将私有管道纳入 Go 轮询器。最终 `make lint check test` 通过，包含 22 项 Python 测试和全部 Go 竞态测试；Linux amd64/arm64 全包构建及新增 Linux 入口静态检查通过。两个非 root Agent 进程完成独立模型—工具—模型循环，一个中继中断不影响另一个；root/参数拒绝、信号取消、协议错误均已验收。测试中继返回固定内容，未连接真实模型供应商，也未证明生产 cgroup 入组及 Worker 分发已经完成。

该阶段 arm64 镜像为 `sha256:0aa5cb8a5f4e1375f2b3b7639f0322834ad2ac6d776b300cd2a4d20bffa18ebd`，完整工具、Chrome 沙箱、双桌面、远程输入、共享文件与定向关闭验收通过，两张 1280×800 中文 XFCE 截图已复核。产物为 `/private/tmp/tombot-bot-agent-final-image-verification-20261111`，检查及进程验证日志为 `/private/tmp/tombot-bot-agent-final-checks.log` 和 `/private/tmp/tombot-bot-agent-final-container.log`。当时的下一步为替换 Worker 同进程调用并接通电脑授权、进程监督与中继；本轮进展见下一段。

上一阶段将 Worker 的同进程循环替换为 `botagent.Dispatch`，电脑控制服务和 Agent 运行器均为必填。纯文本任务先申请桌面执行权，等待期间不调用模型；恢复时取得新代次电脑会话并启动新 Agent，已知模型及工具结果继续复用。所有新操作在登记事务内复验当前 Agent 会话，原数据库结构及历史迁移保持不变。

节点与代理新增固定 Agent 启动及二进制分块输入/输出，每块复验完整会话。代理只启动固定受信引导，保存单次尝试，使用有界管道、连续偏移和末块回执；未知输入不重发。服务端独立审核 Agent 请求轨迹，拒绝上下文、工具定义、参数、步骤和完成内容篡改；完成帧后确认输出排空和进程退出，再执行电脑回收。关闭私有流失败也不会阻止整组终止。

`make lint check test` 和 Linux amd64/arm64 构建通过。真实受限容器验证两个非 root Agent、70 KiB 上下文分块、Unix 控制协议、完整模型—工具循环、参数原文、输入去重、阻塞超时、输出复查、旧代次拒绝和固定引导拒绝伪造入组；既有图形、终端与代理入口测试通过。成功 Agent 进程仍由测试启动器直接降权，模型中继使用固定结果，没有完成真实业务 cgroup 成功入组或公网供应商联合验收。

该阶段 arm64 镜像为 `sha256:5946c0e753e5edbb325bc3def44dba528190f4e59a67c4f84222c53b6d0ac258`，完整工具、Chrome 沙箱及双桌面镜像验收通过。日志为 `/private/tmp/tombot-dispatch-final-checks.log`、`/private/tmp/tombot-dispatch-agent-final.log`，镜像报告为 `/private/tmp/tombot-dispatch-image-verification-20261111`。

上一阶段新增 `computer_shell` 实际工具适配，命令只经所属电脑的终端运行器启动，运行身份由 Worker 及节点验证。沿用当前不等待审批的规则，默认拒绝特权模式，工作目录按用户共享文件访问。测试复现并修复了控制字符 JSON 转义膨胀导致有效尾部全部丢失的问题；协议和工具共用输出校验，补齐截断游标重叠、无进度尾部和游标边界检查。

完整 `make lint check test` 通过，包含 22 项 Python 测试及全部 Go 竞态测试；Linux amd64/arm64 全包构建及相关 Linux 静态检查通过。真实 PostgreSQL 验证启动前持久记账、电脑会话外键、已知非零退出、策略拒绝、启动回包丢失后的未知终态与定向清理。最终镜像的完整 Linux 代理测试通过：两个真实非 root Bot Agent 同时经 Shell 工具写入及读取共享文件，核对独立 UID、DISPLAY 与完整模型—工具—模型循环。模型响应仍为受控测试实现，成功进程由测试启动器直接降权，真实业务 cgroup 入组及整树回收尚未验收。

当前 arm64 镜像为 `sha256:843a53a65547ced347abf1abe8a38bdec79b648920716da514be151525bbfcd9`，完整工具、编译、PDF 转换、Chrome 沙箱、双桌面、真实输入、共享文件、权限拒绝及定向关闭验收通过。两张 1280×800 中文 XFCE 截图已复核，临时容器已确认删除。检查、代理和镜像验收日志分别为 `/private/tmp/tombot-shell-final-checks.log`、`/private/tmp/tombot-shell-agent-final.log`、`/private/tmp/tombot-shell-image-verification.log`，镜像报告为 `/private/tmp/tombot-shell-image-verification-20261111`。主入口、其余电脑工具、屏幕网关、共享浏览器窗口及客户端切换仍未完成，整体目标保持进行中。

新增节点 API 后，完整 `make lint check test` 通过，包含 22 项 Python 测试、全部 Go 竞态检查；Linux amd64/arm64 全包构建及节点相关 Linux 静态检查通过。一次普通沙箱测试因本地端口权限被拒绝，随后在允许临时端口的完整检查中通过，没有修改或关闭检查规则。

临时 Linux 容器实际运行 root 节点与指定非 root 客户端，验证全部 15 个受限动作、最大 Agent 输入块、二进制输出、错误 UID/GID 与 root 调用拒绝、并发容量、取消、稳定锁、活跃及陈旧端点、外来描述符、错误响应和失联不重发。绝对期限验证覆盖传输耗时、已过期租约、不同时间命名空间、缺少及多余授权；节点持久状态测试核对开启、续约及重复版本最终传给代理的期限没有增加。日志为 `/private/tmp/tombot-node-api-final-checks.log` 和 `/private/tmp/tombot-node-api-final-linux.log`，两个临时测试容器均已确认删除。

这些验证使用真实内核通信和身份，业务能力实现仍为测试替身，没有连接 Docker 守护进程或既有用户卷。该阶段没有新增电脑镜像构建，没有证明实际节点开户或生产 cgroup 清理完成，也尚未替换旧主入口。

## 后续实施顺序

| 顺序 | 范围 | 完成条件 |
| --- | --- | --- |
| 1 | 生产部署链路 | Docker 自动引导、独立节点、节点与镜像登记、首位管理员、同步开户、实际能力探针及部署恢复入口接通 |
| 2 | 共享浏览器 | 同用户共享 Chrome 内核与账号状态，各桌面窗口客户端隔离，持久 profile、上传下载及故障恢复接通 |
| 3 | 业务功能补齐 | 群聊、私信、事件订阅、记忆、例程、搜索和业务文件接入 PostgreSQL 与容器 Agent |
| 4 | 管理平台补齐 | 账户状态、角色、限额、模型策略分配、价格、用量、审计、任务与资源运维完整可用，视觉验收通过 |
| 5 | 客户端切换 | 登录、凭据保管、账户缓存隔离、v2 业务协议及实时事件接通，移除模型配置 |
| 6 | 旧实现清理 | 删除 SQLite 运行依赖、旧接口、宿主执行路径和服务端品牌专用服务；历史迁移保持不可变且不进入新链路 |
| 7 | 最终交付验收 | 新安装、多用户、多 Bot、故障恢复、资源限额、联合备份恢复与升级回滚逐项通过 |

远程电脑交互界面按本页顶部范围暂缓，另行设计验收；不取消 Agent 执行和浏览器所需的隔离、授权与回收检查。

## 变更与恢复边界

主入口本轮整体改为 PostgreSQL，配置入口为 `TOM_SERVER_CONFIG`，Linux 非 root、私有凭据文件及 HTTPS 必填。旧单用户命令、宿主执行与配置不保留；没有新增数据库结构或修改历史 SQL。切换及回滚先停单排空，保留用户卷、数据库、密钥版本、节点代次与未知执行证据；旧单用户二进制不能连接新库或复用新卷。新旧客户端不能混接，当前还未具备完整生产切流条件，详见[应用组合说明](../internal/serverapp/README.md)。

节点 API 使用独立受保护 Unix socket 和 `node_control` 帧，固定非 root 控制进程 UID/GID；调用期限上限为 60 秒，账户工作者及节点配置须匹配。执行租约经同内核绝对时钟传递，保持原持久命令与版本不变。没有数据库结构或历史迁移变更；发布和回滚须保留电脑与桌面屏障，先停单排空，不直接删除仍有监听者的端点，不让应用进程接管 Docker 权限。

容器 Shell 适配没有新增数据库迁移或修改历史脚本。工具结果容量须与 Worker 输出上限对齐，节点调用期限和轮询间隔显式配置；组合时从同一注册集合生成工具 Schema 与 Bot 权限名称。未知命令不能通过更换操作 ID、重启 Agent 或切回宿主 Shell 重发。发布及回滚继续停单排空、保留操作记录和桌面关闭屏障；当前未替换生产 `tomd` 或客户端，也未连接已有业务数据库与用户卷。

本轮新增必填 `WorkerOptions.AgentTimeout` 和 `computer.AgentRuntime` 依赖，取消可选电脑分支；Node/Agent 协议新增 `agent_start`、`agent_write`、`agent_read`。只按成套版本停单、排空并替换，旧协议缺少能力时明确失败，不回退到服务端循环。输入回执及 Agent 进程不跨代理重启恢复，继续沿用已有持久关闭屏障、代次和未知操作证据。数据库结构没有变更，迁移 `0001`–`0042` 未修改。

Bot Agent 二进制及私有标准流协议沿用现有持久步骤键、JSON 参数原文和数据库记录格式，没有新增或修改迁移。发布与回滚需成套匹配镜像、节点、Agent 中继与 Worker，停单排空后更换实例并保留代次和未知操作证据。主服务部署尚未切换，不能把模块验收当作整个产品已完成，也不能重新引入服务端循环回退。

图形协议增加 `desktop_start` 与 `desktop_status`，桌面启动器改为原子发布就绪文件。节点、代理和镜像需成套替换；先停止接单并排空，再按新实例代次部署或回滚，保留旧控制屏障及未知启动记录，不原地重启失败桌面。数据库迁移未变。Bot Agent 分发已由新 Worker 接通，现有主入口仍须组合；不能恢复服务端循环作为正式回退。

本轮私有会话目录改由代理统一创建，XFCE 只消费已准备的身份文件；节点新增终端协议，三者须成套发布。数据库迁移 `0001`–`0042` 均未修改。升级或回滚先停止接单并排空，保留控制代次与未知操作；不能复用会覆盖新身份文件的旧桌面启动器。终端输出和输入回执是当前代理进程内的有界状态，代理重启按既有会话屏障隔离，不恢复或重放终端程序。

节点 `RuntimeBytes` 与 `SharedMemoryBytes` 为新增必填部署配置，seccomp 直接嵌入镜像同源 JSON。共享目录初始化只接受空白或符合当前约定的卷，不导入或递归转换旧数据；旧容器运行配置不符时对账失败，不原地改写。节点、镜像和初始化程序成套发布，停单排空并保留控制记录后才能建立新实例；回滚使用配套程序与独立环境。此项无数据库迁移变更。

任务引导协议升级为版本 2，移除顶层重复 UID/GID，使用完整授权桌面身份和规范绝对工作目录，拒绝旧版本和旧字段。终端与 XFCE 的环境模板成套发布；未新增或修改数据库迁移。共享业务卷不再使用私有角色目录约定，升级及回滚必须停单排空并保留控制代次、未知操作和身份记录；旧程序不能直接复用新共享卷，应恢复相互匹配的程序与独立环境后再开放服务。

桌面执行控制新增 `0042`，此前 `0001`–`0041` 保持不变。开户设置新增必填 `DesktopConcurrency`；会话协议增加完整桌面身份，旧节点、代理和镜像内二进制不能视为已兼容。迁移拒绝转换已有中间模型的会话和用户限额，失败保留原数据；回滚不得删除桌面代次、释放证据或降低配额检查。正式运行从全新空 PostgreSQL 库初始化，节点多会话接通后必须重建并联合验收镜像。

当前新增数据库模块尚未接入运行入口，不改变现有业务数据或部署。测试和新库初始化仅针对明确指定的空数据库；不存在向旧库导入、双写或自动删除路径。后续修正已记录的 PostgreSQL 结构时继续新增版本脚本，不修改历史脚本。

本轮 `0032` 追加操作调度字段及索引，没有修改 `0001` 至 `0031`。Docker 引导仅新增受限部署入口；尚未在当前宿主机执行安装或修改系统服务。Linux 安装回滚边界和已安装软件包的恢复策略见对应模块说明。

模型 HTTP 与网关不新增数据库迁移，`0033`、`0034` 保持不变。已有模型传输不再自动重试生成 POST，当前旧主服务也会直接收到首次失败；部署与回滚需记录在途未知结果，不能通过重启或更换二进制自动重放。传输边界及恢复说明见 [modelgateway](../internal/modelgateway/README.md)。

目录领域新增 `0035`、`0036`，此前脚本保持不变。小尺寸自定义头像作为受限用户配置存入 PostgreSQL，普通业务文件仍使用用户卷。目录迁移的结构锁、活动重名失败及应用回滚边界见 [bots](../internal/bots/README.md)。当前主服务和桌面未切换，本轮没有读取或修改现有业务数据。

持久运行新增 `0037`，此前脚本保持不变；新增运行表、队列、操作记录及会话回合序号，不导入旧消息。迁移锁影响与应用回滚见 [execution](../internal/execution/README.md)。回滚不能清空未知操作后重发，不能把数据库任务租约恢复视为真实电脑进程已经清理。当前主入口仍未切换，不改变旧业务库与桌面协议。

用量新增 `0038`、`0039`，此前版本保持不变。预留与计量证据不得通过降级删除，应用不能回退到跳过额度接纳的 Worker；已知计数及费用不能被重写，未知预留继续保留。新增复合约束与配置核对会取得结构锁，发布前需评估运行表规模。详细边界见 [modelusage](../internal/modelusage/README.md)。本轮没有连接或修改已有业务库，也未切换桌面和生产主入口。

共享电脑执行权新增 `0040`，此前脚本保持不变。任务和操作表新增约束与触发器，发布前需评估结构锁和校验时长；回滚必须保留会话及控制代次，不能通过删除占用记录释放电脑。详见 [computercontrol](../internal/computercontrol/README.md)。当前只接入 Worker 控制流程，尚未运行真实电脑命令或切换生产主入口。

桌面身份新增 `0041`，`0001`–`0040` 保持不变。机器人响应增加 `desktop_id`，受信内部创建也须同事务登记桌面。迁移只接受空机器人目录，已有中间模型数据时明确拒绝并回滚；不补造身份、不自动删除数据。显示编号与 UID 不回收，部署 `MaxBots` 上限与镜像显示范围一致为 4095；这不是实际运行并发容量。应用回滚保留新结构及分配记录，仅恢复理解桌面归属的版本，详见[桌面身份模块](../internal/computerdesktop/README.md)。

节点执行会话新增独立持久记录，没有数据库结构变更。回滚必须保留会话关闭屏障和父资源组身份，停止接单后确认清理；不能通过删除状态文件释放电脑。节点控制进程或父 cgroup 更换时采取拒绝恢复旧执行权的策略，自动容器重建仍待实现。本轮未修改 Docker 守护进程、宿主 cgroup 或现有用户容器。

节点桌面记录现使用 `<user_id>.<desktop_id>.session.json` 与版本 2 内容；旧用户级文件、旧版本、身份错配或损坏记录会阻止恢复和该用户的生命周期删除，原文件保持不变，不自动迁移。节点容量按桌面计算，隔离记录仍占用名额；用户生命周期变更与所有在途桌面调用互斥。正式部署从空状态建立配套环境；回滚必须保留全部桌面屏障，不能切回会忽略这些记录的旧节点。数据库迁移 `0001`–`0042` 本轮均未修改。

容器代理的配置和持久记录也采用版本 2，每个桌面保存 `<desktop_id>.session.json`，明确拒绝旧 `session.json`、旧版本及身份冲突，失败保留原记录。节点代理配置新增必填 `MaximumActive`，范围为 1–64；固定实例的配置不会被原地覆盖。节点、代理、任务引导及镜像必须成套重建和联合验收，旧镜像内单会话代理不能与新配置混用。回滚保留全部桌面代次、未知操作和关闭屏障，不引入旧格式转换或兼容层。

管理平台新增目录查询与静态资源处理器，没有数据库结构变更。主题和通用控件样式从客户端移动到顶层 `ui/`，两端从同一权威文件构建；客户端业务协议未切换。发布及回滚须保留新目录布局，并让管理产物与相同 v2 协议的服务端成套部署。现有 `tomd` 尚未接入管理 API 和页面，本轮没有向已有服务发布、创建真实账户或调用公网模型。

容器内任务引导新增独立纯 Go 二进制和封存协议，没有数据库结构变更。完整代理接入后须让镜像、节点与引导成套发布，停单排空后才能回滚，不能切回宿主 Shell。验证只创建临时受限容器、只读挂载本轮测试二进制并使用空白 tmpfs，没有挂载宿主资源组、Docker socket 或既有用户卷。

代理控制通道与看门狗没有新增数据库迁移。任务引导协议新增必需的撤销描述符，节点、代理、引导及镜像必须成套部署；不兼容缺少该描述符的启动方式。代理持久记录保留最高会话代次和关闭屏障，回滚不能删除记录或恢复未知任务。当前未切换生产主入口，没有启动真实用户命令或改动现有容器。

本轮代理入口及节点绑定也没有新增数据库迁移，新增实例控制目录、配置及挂载均不可被客户端覆盖。既有实例配置不会原地重写，变更配置与协议须在停单排空后按新代次成套部署；回滚保留关闭屏障。最新并行桌面要求将通过后续前向迁移和代码替换实施，不修改 `0040` 或把目前单会话测试改写成并行支持的证明。
