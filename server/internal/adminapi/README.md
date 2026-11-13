# 管理平台业务接口

本模块提供多用户重构的管理业务 HTTP 路由，已实现账户与电脑资源目录、同步开户、恢复、管理员账号、节点目录配置及服务端模型配置接口。`New` 必须注入开户服务、管理员账号服务、节点目录服务、模型目录服务、认证门与日志器。[管理页面](../../../admin/README.md)已接入这些协议；`tomd` 已完成 [入口组合](../serverapp/README.md)，账户变更、电脑运维等接口及完整部署仍需补齐。

全部路径通过 `authapi.Handler.Protect("admin", …)`，统一执行 HTTPS、来源、管理 Cookie 和写请求 CSRF 校验。领域服务再次按数据库当前身份验证管理员权限，写操作需要近期二次验证。客户端无法通过请求体或查询参数覆盖操作者及服务端请求标识。

| 方法与路径                                                   | 成功响应与语义                                                                 |
| ------------------------------------------------------------ | ------------------------------------------------------------------------------ |
| `GET /api/v2/admin/users`                                    | `200`，账户、电脑状态和创建操作标识的分页目录，不返回认证材料或业务正文        |
| `GET /api/v2/admin/computer-nodes`                           | `200`，节点名称、状态、容量与预留数量，不返回节点连接地址                      |
| `GET /api/v2/admin/computer-images`                          | `200`，固定镜像引用及启用状态的分页目录                                        |
| `POST /api/v2/admin/users`                                   | `201`，电脑已经就绪、创建事务已经提交，返回操作及 `Location` 查询地址          |
| `GET /api/v2/admin/user-creation-operations/{id}`            | `200`，同一操作的状态、阶段、错误分类和相关资源 ID                             |
| `POST /api/v2/admin/user-creation-operations/{id}/retry`     | `200`，已清理的失败操作重新创建成功；等待超时仍为 `504`                        |
| `POST /api/v2/admin/user-creation-operations/{id}/reconcile` | `200`，隔离资源已确认清理，操作为 `failed`，用户仍不可登录，之后可另行重试开户 |
| `POST /api/v2/admin/users/{id}/activation`                   | `201`，一次性返回激活材料；再次签发会废止原值                                  |
| `GET /api/v2/admin/administrators`                           | `200`，管理员账号元数据的 ID 倒序分页目录，不返回凭据或动态验证材料            |
| `POST /api/v2/admin/administrators`                          | `201`，创建管理员并一次性返回动态验证器密钥及 `Location`                       |
| `POST /api/v2/admin/computer-nodes`                          | `201`，登记或复验本机节点与镜像目录；相同字段幂等，冲突返回 `409`              |
| `GET /api/v2/admin/computer-nodes/{imageID}`                 | `200`，固定节点与指定镜像的目录快照，含验收状态，不含连接地址                  |

创建用户请求包含 `username`、`display_name`、`node_id`、`image_id`、`resources` 和 `idempotency_key`。角色不再是开户字段：本接口只创建普通用户，管理员由独立接口管理。资源字段为 `cpu_millis`、`memory_bytes`、`disk_bytes`、`process_limit`，全部由服务端限制范围并执行节点容量准入。重试、对账和激活签发提交空 JSON 对象，目标 ID 只从路径读取；查询操作使用 GET，无请求体。管理员创建接受 `username`、`display_name` 与初始 `password`，账号创建后即为有效，动态验证器密钥只返回这一次；节点登记只接受 `name`、`capacity`、`image_id`、`image_reference`，节点身份与连接入口固定取自服务部署配置，页面不能指定地址。这里不提供首位管理员引导的远程入口。

JSON 请求最大 16 KiB，必须为单个 UTF-8 对象，不允许未知字段。账户、管理员、节点、镜像及下述模型列表接口允许 `before`、`limit` 查询参数，其他请求不接受查询参数。全部响应设置 `no-store` 和请求标识，有正文的响应设置 JSON 内容类型。管理会话使用安全 Cookie，不接受桌面 Bearer 通道。

账户与电脑目录使用 ID 倒序游标，默认每页 50 条、最大 100 条；目录读取复验当前管理员身份，不要求写操作的近期二次验证。节点容量是读取时快照，选择可用节点不能替代开户事务中的最终容量准入。目录协议和真实 PostgreSQL 测试覆盖空页、连续分页、资源状态、非法参数、撤权与取消。

## 模型管理

以下路径均以 `/api/v2/admin` 为前缀，读操作复验当前管理员身份，写操作还要求近期二次验证。

| 方法与路径                                                                                                                    | 行为                             |
| ----------------------------------------------------------------------------------------------------------------------------- | -------------------------------- |
| `GET/POST /model-providers`                                                                                                   | 分页查看或登记供应商             |
| `GET/POST /model-providers/{providerID}/models`                                                                               | 查看或登记该供应商的模型         |
| `GET/POST /model-providers/{providerID}/credentials`                                                                          | 查看凭据元信息或新增加密凭据版本 |
| `POST /model-credentials/{credentialID}/revoke`、`/destroy`                                                                   | 撤销或按退役条件销毁凭据         |
| `GET/POST /model-policies`                                                                                                    | 查看或创建策略                   |
| `GET/POST /model-policies/{policyID}/versions`                                                                                | 查看版本或创建不可变草稿         |
| `GET /model-policies/{policyID}/versions/{version}`                                                                           | 读取指定版本                     |
| `POST /model-policies/{policyID}/versions/{version}/publish`、`/revoke`                                                       | 探测并发布或撤销版本             |
| `POST /model-policies/{policyID}/default`                                                                                     | 设置平台默认策略                 |
| `POST /model-providers/{configurationID}/state`、`/models/{configurationID}/state`、`/model-policies/{configurationID}/state` | 按期望修订启用或停用             |
| `PUT /users/{userID}/model-policy`                                                                                            | 按期望原策略设置用户策略         |
| `PUT /users/{userID}/bots/{botID}/model-policy`                                                                               | 按期望原策略设置机器人特例       |

列表默认每页 50 条，最大 100 条。`before` 使用上一页末项的 ID，凭据与策略版本使用版本号；拒绝未知或重复参数。修改目录状态必须显式提供 `is_enabled` 和 `expected_revision`；发布及默认策略也校验期望修订。分配必须显式提供 `expected_policy_id`，空字符串表示原来继承上级；新 `policy_id` 为空时恢复继承。并发冲突应重新读取后交由管理员决定，不自动覆盖。

路径确定供应商、策略、用户与 Bot，正文不能覆写路径归属。新增凭据只接受专用字段，响应不含凭据值、密文或摘要；明文字节在服务调用结束后清零，但 Go 字符串及 HTTP 缓冲不承诺立即物理擦除。禁止记录请求正文。撤销与销毁独立执行，销毁条件由领域服务和数据库共同验证。

这里不暴露 `Resolve`、`Use`、任意模型代理或用户可选模型入口。策略选择与凭据解密仅供受信运行调度器调用。供应商能力探测由 [modelgateway](../modelgateway/README.md) 执行，版本、加密及审计规则见 [modelregistry](../modelregistry/README.md)。

## 错误与前端处理

错误格式为 `code`、中文 `message`、`request_id`，已有操作时额外包含 `operation`。`504 CREATE_PENDING` 必须保留操作 ID 并继续查询，不能展示创建成功，也不能自动更换幂等键重新开户。`502 PROVISION_FAILED` 表示开户失败；`409 COMPUTER_QUARANTINED` 表示资源结果尚不确定且容量仍被预留。

幂等键或用户名冲突为 `409`，管理员重名或节点目录冲突为 `409`，容量不可用为 `503`，权限拒绝为 `403`，未找到操作、管理员或镜像为 `404`。未分类数据库或节点错误仅返回固定中文提示，日志保留请求、操作者与操作标识，不透传底层错误、正文或激活值。

协议测试覆盖成功时点、未完成操作不能返回 `201`、等待超时携带操作、恢复与激活路由、管理员创建、节点登记与目录读取、供应商模型拉取、跨来源及 CSRF 拒绝、桌面凭据拒绝、身份字段注入、查询参数和超长 JSON 拒绝、错误脱敏及禁止远程引导。账户状态和真实 PostgreSQL 验证见 [accounts](../accounts/README.md)，真实用户容器验收仍待节点运行器接入。

模型协议测试覆盖全部管理路由、分页边界、路径归属、并发修订参数、缺少布尔字段、错误脱敏、凭据不回显和明文字节清零。非法配置为 `400`，修订冲突为 `409`，能力验证失败为 `422`，验证超时为 `504`；未分类错误返回固定 `503`，不透传供应商正文或底层错误。这些协议测试与领域、传输测试分别验证边界，尚不等同于主服务端到端验收。
