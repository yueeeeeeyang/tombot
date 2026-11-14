# 管理平台业务接口

本模块提供多用户重构的管理业务 HTTP 路由，已实现账户与电脑资源目录、同步开户、恢复、管理员账号、节点接入凭据及服务端模型配置接口。`New` 必须注入开户服务、管理员账号服务、节点接入服务、模型目录服务、认证门与日志器。[管理页面](../../../admin/README.md)已接入这些协议；`tomd` 已完成 [入口组合](../serverapp/README.md)，账户变更、电脑运维等接口及完整部署仍需补齐。

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
| `POST /api/v2/admin/computer-nodes/enrollment-tokens` | `201`，返回一次性接入凭据的 ID、值和有效期，需近期验证 |

创建用户请求包含 `username`、`display_name`、`node_id`、`image_id`、`resources` 和 `idempotency_key`。角色不再是开户字段：本接口只创建普通用户，管理员由独立接口管理。资源字段为 `cpu_millis`、`memory_bytes`、`disk_bytes`、`process_limit`，全部由服务端限制范围并执行节点容量准入。重试、对账和激活签发提交空 JSON 对象，目标 ID 只从路径读取；查询操作使用 GET，无请求体。管理员创建接受 `username`、`display_name` 与初始 `password`，账号创建后即为有效，动态验证器密钥只返回这一次；接入凭据签发接受空对象，节点随后通过独立签名接入接口自动登记；管理平台仅在当前弹窗展示并复制凭据码，安装时通过隐藏输入粘贴，由安装器保存到 root 0600 文件；凭据不进入安装参数、命令行或浏览器存储。这里不提供首位管理员引导的远程入口。

JSON 请求最大 16 KiB，必须为单个 UTF-8 对象，不允许未知字段。账户、管理员、节点、镜像及下述模型列表接口允许 `before`、`limit` 查询参数，其他请求不接受查询参数。全部响应设置 `no-store` 和请求标识，有正文的响应设置 JSON 内容类型。管理会话使用安全 Cookie，不接受桌面 Bearer 通道。

账户与电脑目录使用 ID 倒序游标，默认每页 50 条、最大 100 条；目录读取复验当前管理员身份，不要求写操作的近期二次验证。节点容量是读取时快照，选择可用节点不能替代开户事务中的最终容量准入。目录协议和真实 PostgreSQL 测试覆盖空页、连续分页、资源状态、非法参数、撤权与取消。

## 模型管理

以下路径均以 `/api/v2/admin` 为前缀，读操作复验当前管理员身份，写操作还要求近期二次验证。

| 方法与路径                                                                                                                    | 行为                             |
| ----------------------------------------------------------------------------------------------------------------------------- | -------------------------------- |
| `GET/POST /model-providers`                                                                                                   | 分页查看或登记供应商             |
| `GET /model-providers/{providerID}` | 按标识读取供应商详情，不返回凭据 |
| `GET/POST /model-providers/{providerID}/models`                                                                               | 查看或登记该供应商的模型         |
| `GET/POST /model-providers/{providerID}/credentials`                                                                          | 查看凭据元信息或新增加密凭据版本 |
| `POST /model-credentials/{credentialID}/revoke`、`/destroy`                                                                   | 撤销或按退役条件销毁凭据         |
| `GET/POST /model-policies`                                                                                                    | 查看或创建策略                   |
| `GET /model-policies/{policyID}` | 按标识读取策略状态及修订号 |
| `GET/POST /model-policies/{policyID}/versions`                                                                                | 查看版本或创建停用版本         |
| `GET /model-policies/{policyID}/versions/{version}`                                                                           | 读取指定版本                     |
| `POST /model-policies/{policyID}/versions/{version}/enable`、`/disable`                                                       | 启用或停用版本             |
| `POST /model-policies/{policyID}/default`                                                                                     | 设置平台默认策略                 |
| `POST /model-providers/{configurationID}/state`、`/models/{configurationID}/state`、`/model-policies/{configurationID}/state` | 按期望修订启用或停用             |
| `PUT /users/{userID}/model-policy`                                                                                            | 按期望原策略设置用户策略         |
| `PUT /users/{userID}/bots/{botID}/model-policy`                                                                               | 按期望原策略设置机器人特例       |

`POST /model-providers/{providerID}/edit` 接收 `expected_revision`、`name`、`protocol`、`base_url` 和可选 `credential`，仅编辑停用供应商，返回 `200 {provider}`。旧 `/replace` 入口已移除。启用中编辑返回 `409 MODEL_PROVIDER_ENABLED`，跨主机或端口未提供新凭据返回 `400 MODEL_NEW_CREDENTIAL_REQUIRED`。

`GET /model-providers/{providerID}/remote-models` 返回 `200 {models}`，每项包含标识、名称及 `metadata`，可选 `context_limit`、`output_limit`、`supports_tools`、`supports_vision`；字段缺失表示供应商未提供，明确 `false` 才表示不支持。`metadata` 包含可选 `input_modalities`、`output_modalities` 和 `effort: {supported_levels, default_level?}`，不透传未知上游字段。

`POST /model-providers/{providerID}/models` 在现有规格字段之外接收可选 `metadata` 和 `reasoning_effort`，返回的模型始终包含 `metadata`；所选思考级别必须属于元数据候选。导入页按模型预填并允许调低上限；未知必填项由管理员确认，不能把缺失当作零或不支持。多模型登记逐个提交，部分失败保留已完成结果，关闭后重新读取目录，不自动重放。

`POST /models/{modelID}/test` 接收 `expected_provider_revision` 与 `expected_model_revision`，返回 `200 {test}`，包含两项独立的 `connectivity`、`tools` 结果（`status`、`message`、`duration_ms`）、测试时间和目录修订。`200` 表示测试过程完成，能力是否通过必须读取各项状态；连通性失败时工具状态为 `skipped`。配置竞争返回 `409`，缺失凭据、撤权或审计错误均拒绝返回有效测试结果。服务端限制测试总时长，客户端不自动重试或恢复关闭前的请求。

列表默认每页 50 条，最大 100 条。`before` 使用上一页末项的 ID，凭据与策略版本使用版本号；拒绝未知或重复参数。修改目录状态必须显式提供 `is_enabled` 和 `expected_revision`；版本启停及默认策略也校验期望修订。分配必须显式提供 `expected_policy_id`，空字符串表示原来继承上级；新 `policy_id` 为空时恢复继承。并发冲突应重新读取后交由管理员决定，不自动覆盖。

路径确定供应商、策略、用户与 Bot，正文不能覆写路径归属。新增凭据只接受专用字段，响应不含凭据值、密文或摘要；明文字节在服务调用结束后清零，但 Go 字符串及 HTTP 缓冲不承诺立即物理擦除。禁止记录请求正文。撤销与销毁独立执行，销毁条件由领域服务和数据库共同验证。

这里不暴露 `Resolve`、`Use`、任意模型代理或用户可选模型入口。策略选择与凭据解密仅供受信运行调度器调用。供应商能力探测由 [modelgateway](../modelgateway/README.md) 执行，版本、加密及审计规则见 [modelregistry](../modelregistry/README.md)。

## 错误与前端处理

错误格式为 `code`、中文 `message`、`request_id`，已有操作时额外包含 `operation`。`504 CREATE_PENDING` 必须保留操作 ID 并继续查询，不能展示创建成功，也不能自动更换幂等键重新开户。`502 PROVISION_FAILED` 表示开户失败；`409 COMPUTER_QUARANTINED` 表示资源结果尚不确定且容量仍被预留。

幂等键或用户名冲突为 `409`，管理员重名或节点目录冲突为 `409`，容量不可用为 `503`，权限拒绝为 `403`，未找到操作、管理员或镜像为 `404`。未分类数据库或节点错误仅返回固定中文提示，日志保留请求、操作者与操作标识，不透传底层错误、正文或激活值。

协议测试覆盖成功时点、未完成操作不能返回 `201`、等待超时携带操作、恢复与激活路由、管理员创建、接入凭据签发、近期验证与旧登记接口移除、供应商模型拉取、跨来源及 CSRF 拒绝、桌面凭据拒绝、身份字段注入、查询参数和超长 JSON 拒绝、错误脱敏及禁止远程引导。账户状态和真实 PostgreSQL 验证见 [accounts](../accounts/README.md)，真实用户容器验收仍待节点运行器接入。

模型协议测试覆盖全部管理路由、分页边界、路径归属、并发修订参数、缺少布尔字段、错误脱敏、凭据不回显和明文字节清零。非法配置为 `400`，修订冲突为 `409`，能力验证失败为 `422`，验证超时为 `504`；未分类错误返回固定 `503`，不透传供应商正文或底层错误。这些协议测试与领域、传输测试分别验证边界，尚不等同于主服务端到端验收。

## 停用模型维护接口

`POST /api/v2/admin/models/{modelID}/edit` 接收 `expected_revision` 及与模型登记相同的参数，不接收 `provider_id`、`is_enabled` 或操作者。成功返回 `200 {model: ...}`；`POST /api/v2/admin/models/{modelID}/delete` 仅接收 `expected_revision`，成功返回 `204`。启用中返回 `409 MODEL_ENABLED`，仍有策略版本引用返回 `409 MODEL_REFERENCED`，修订冲突沿用 `409 MODEL_CONFIGURATION_CONFLICT`。均复用来源、CSRF、当前身份和近期验证边界，删除不级联策略。

## 多模型策略协议

创建版本的请求为 `{"routing_mode":"priority|weighted|round_robin","candidates":[...]}`。候选字段包括 `model_id`、`priority`、`weight`，以及可选 `credential_id`、`output_limit`、`reasoning_effort`、`prices`。省略凭据/输出/思考字段时继承当前目录值；空思考字符串表示不发送该参数。策略归属始终从路径绑定；旧顶层单模型字段和候选中的任意地址、协议、操作者字段均拒绝。版本响应返回 `routing_mode` 与 `candidates`，每项包含固定的 `configuration` 和安全凭据引用，不返回明文。

`POST /model-policies/{policyID}/versions/{version}/enable` 和 `/disable` 接收 `expected_revision`，需要近期管理员验证。启用成功返回 `200` 及 `version`，停用成功返回 `204`。版本响应包含 `is_enabled` 和 `verified_at`，不再包含 `status`、`published_at`。新版本默认停用；首次启用必须测试全部候选，失败保持停用。已验证版本可重新启用，启用时原子替换当前选择；停用清空当前选择，已有运行保持原版本。停用非当前版本及重复操作不会改变其他版本，过期修订返回 `409`。管理页面直接使用版本响应的 `is_enabled`，不拼接不同请求中的状态。

旧 `/publish`、`/activate`、`/revoke` 版本入口已移除；凭据撤销接口保持独立。新协议、迁移 0058 和管理静态资源必须同步部署，不提供旧请求兼容层。迁移和回滚要求见 [模型策略说明](../modelregistry/README.md#多模型路由与版本启停)。

节点接入码封装服务地址、HTTPS 证书指纹和一次性授权（最大 2048 字符）；首次安装无需手填服务参数，API 客户端信任由已鉴权引导接口提供。离线电脑镜像由配套节点安装包交付，详见节点安装文档。
