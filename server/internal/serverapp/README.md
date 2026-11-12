# 多用户应用组合与运行入口

`cmd/tomd` 使用本模块组合 PostgreSQL、身份认证、机器人目录、管理平台、模型网关、开户工作池和执行 Worker。API 使用 Linux 非 root 身份，通过受保护的本机节点通道调度电脑；Bot Agent 的模型与工具循环仍在所属用户容器内执行。本模块没有宿主 Shell 或旧运行时回退。

本阶段完成入口组合，不表示部署引导与全部产品能力已完成。[首位管理员本机部署入口](../../docs/admin-bootstrap.md)与[节点及镜像目录登记](../../docs/node-registration.md)已接通，同一服务配置的控制连接由 API 与命令共用；命令没有独立 Worker。目录登记只创建 `bootstrapping` 节点，不发布就绪。独立节点程序、镜像准备和[控制心跳租约](../../docs/node-health.md)已接通，[独立验收与镜像准入](../../docs/node-verification.md)已接通；共享浏览器、真实生产部署验收、客户端切换及备份恢复仍待完成。远程电脑用户交互按最新范围暂缓。不得手工把未验收的电脑标为就绪来绕过前提。

## 配置与秘密文件

`TOM_SERVER_CONFIG` 必须指向规范绝对路径的 JSON 文件。未知字段、尾随内容及无效参数会阻止启动。省略运行限额时使用 `DefaultConfig`；时间均使用 `5s`、`2m` 等带单位字符串。监听地址、管理来源、文件路径、节点身份、验收镜像 ID 与模型允许来源必须显式提供。

首次部署可使用[配置生成程序](../../deploy/CONFIGURE.md)绑定节点、镜像及实际 API 身份，并生成配套文件。服务配置解析阶段复用模型网关的来源、网络和预算校验，不读取信任文件或连接外部服务；不再等到加载凭据、打开数据库后才发现这些参数无效。TLS、附加信任文件及数据库实际状态仍在启动时核验。

以下是字段示意，示例路径不指向现有数据，节点标识须由部署登记产生，模型来源须替换为实际获准来源。它不是可直接上线的部署文件。

```json
{
  "address": "127.0.0.1:8443",
  "admin_origin": "https://admin.example.test",
  "admin_directory": "/opt/tombot/admin",
  "tls_certificate_file": "/etc/tombot/tls/certificate.pem",
  "tls_private_key_file": "/etc/tombot/private/tls.key",
  "identity_key_file": "/etc/tombot/private/identity.key",
  "csrf_key_file": "/etc/tombot/private/csrf.key",
  "active_model_key": "current",
  "model_key_files": {"current": "/etc/tombot/private/model.key"},
  "database": {
    "dsn_files": {
      "tombot_identity": "/etc/tombot/private/identity.connection",
      "tombot_control": "/etc/tombot/private/control.connection",
      "tombot_tenant": "/etc/tombot/private/tenant.connection",
      "tombot_dispatch": "/etc/tombot/private/dispatch.connection"
    }
  },
  "node": {"id": "部署登记的节点 ULID", "socket": "/run/tombot/control.sock", "heartbeat_interval": "10s", "control_lifetime": "2m", "verification": {"image_id": "部署登记的镜像 ULID"}},
  "models": {"origins": [{"url": "https://model.example.test"}]}
}
```

配置文件和证书必须为 root 或服务身份拥有的普通文件，不允许组或其他用户写入。DSN、TLS 私钥及三种用途的加密文件要求只对所有者开放；通常部署为服务身份拥有的 `0600` 文件。拒绝最终路径符号链接、硬链接、目录、管道、空文件及超限内容。路径和父目录由受信部署维护，API 不接受客户端传入文件路径。机密内容与系统原始文件错误不会写入日志。

身份加密、CSRF 和各模型密钥文件分别存放 32 字节随机原始内容，不使用文本编码或换行。模型密钥轮换保留仍用于解密历史凭据的旧版本文件，活动版本通过 `active_model_key` 选择。文件内容由部署机密渠道产生并保管，不放入仓库或命令参数。TLS 证书必须匹配 `admin_origin` 的主机且在有效期内，最低 TLS 1.3；载入后续期或轮换需要停单排空并重启。

四个数据库文件分别包含同一数据库的独立登录连接串，只继承对应权限组。连接主机、端口和库名必须一致，不接受多地址故障转移或不同别名拼接；独立 TLS 配置及登录凭据可以不同。远程连接遵守存储模块的 TLS 限制。迁移角色、超级用户、混合权限组、缺少迁移、被修改的账本及高于当前程序的版本均阻止启动。应用不创建数据库结构；先通过 `tomdb` 执行新增迁移。

`trusted_proxies` 只用于部署明确的代理网段，不能允许所有来源；代理必须覆盖单一客户端地址与 HTTPS 协议信息。模型 `origins` 是精确 HTTPS 来源白名单，私有模型需要显式 `networks`，额外排除地址使用 `denied_networks`，额外信任根使用 `root_ca_file`。这些网络策略不会由管理端模型表单覆盖，详见[模型网关](../modelgateway/README.md)。

## 启动及进程生命周期

1. 用受信部署准备空 PostgreSQL、四种登录角色、凭据文件和管理前端构建产物；API 不执行迁移或创建演示账号。
2. Docker 安装与节点运行分别由受限部署组件负责。节点 socket 的 root 所有者、控制组、固定调用 UID/GID 和保护目录遵守[节点 API](../nodeapi/README.md)。API 不能加入 Docker 组。
3. `tomd` 校验 Linux 非 root 身份、配置、TLS、私有文件、各用途连接及完整迁移账本，再构造全部服务和前端资源。任何失败释放已经打开的数据库连接且不监听业务端口。
4. HTTPS、节点控制观察、独立电脑验收、开户工作池和执行 Worker 使用同一进程监督。任一组件提前退出即停止接单并取消其他组件；正常退出信号不作为故障。Worker 在自己的有界收尾上下文中保存恢复证据和清理所属会话。
5. 所有后台退出后才允许关闭连接池。退出超过 `http.shutdown_timeout` 时明确返回错误，保留“仍在停止”的状态并结束进程，不能假装资源已经释放。进程重启仍须按持久租约和关闭屏障恢复，未知外部动作不重发。

默认 HTTP 请求时限为 60 秒，开户请求等待 30 秒、模型验证 45 秒，写入时限不低于请求时限。部署节点调用总时限必须覆盖开户、桌面及 Agent 的单次调用时限；收尾时间必须落在退出总时限内。各领域构造函数继续核对自己的上限与并发约束。工具注册集合同时生成 Bot 可用工具名称，当前为 `computer_shell`、`computer_screenshot` 和 `computer_input`。

桌面启动等待由 `computer.desktop_start_timeout` 控制，默认 `45s`。临时截图配置位于 `runs.images`，默认 `max_user_bytes=268435456`、`max_user_images=256`、`context_images=2`、`retention="168h"`；范围和清理语义见[执行模块](../execution/README.md#截图持久化与模型视觉)。模型请求默认上限由 2 MiB 调整为 16 MiB，启动检查其足以承载配置张数的最大 PNG base64 加文本预留空间；配置更多图片时必须相应提高 `models.max_request_bytes`。模型自身也必须声明并验证视觉能力，客户端无须配置模型。

## 路由和健康语义

| 路径 | 行为 |
| --- | --- |
| `/admin/` | 已构建的管理平台，沿用同源 CSP；缺少产物时拒绝启动 |
| `/api/v2/auth/` | 桌面激活、登录、续期、会话与退出 |
| `/api/v2/admin/auth/` | 管理平台 Cookie 认证 |
| `/api/v2/admin/` | 管理操作，受管理身份、来源及 CSRF 保护 |
| `/api/v2/` | 当前用户资源及持久运行接口，受桌面身份保护 |
| `/health/live` | API 进程可以响应 |
| `/health/ready` | 应用正在运行且四个数据库连接可用，返回 `scope: api` |

API 就绪探针不连接 Docker 或模型供应商，也不声明用户电脑已就绪。电脑故障通过其所属开户或运行流程报告；新 Worker 仍要求每次运行先取得电脑授权，纯文本任务同样遵循。旧 `/api/v1` 和旧命令不再注册。

## 变更、恢复与验证边界

当前需执行至节点能力验收迁移 `0046`，增加独立临时电脑账本、镜像绑定及控制/能力双期限门禁；节点目录身份继续受 `0044` 保护，历史迁移不变。`tomd` 从单用户入口整体切换，旧环境变量、初始化码、宿主浏览器安装、SQLite 归档及恢复命令已移除。相应旧命令测试随功能退役，由新入口的配置、TLS、数据库权限与版本、生命周期及组合路由测试替代；旧领域测试继续保留，未关闭检查规则。

正式部署从新库和新卷开始，不把旧单用户数据转换为新数据。切换前停止接单并排空，保存 PostgreSQL、加密文件、用户卷、节点代次与关闭屏障的一致恢复点。回滚使用配套的新架构版本与资源；不能让旧 `tomd` 读取新库、复用新卷或忽略未知执行证据。需要保留旧产品时只能在独立环境恢复旧版本及其原数据，不与新服务混接。新的全链路备份恢复尚未验收，当前不能按完整生产交付切流。

测试使用真实临时 PostgreSQL、独立 TLS 证书和正式服务组合，覆盖两用户激活登录、独立机器人桌面登记、跨用户拒绝、管理通道拒绝、退出撤销、数据库失联、启动失败连接释放、工作者异常、取消竞争及退出超时。测试中的电脑目录为明确的数据库夹具，不启动真实 Docker，不调用公网模型；该范围不能替代节点或电脑端到端验收。
