# 登录用户资源接口

本模块提供 PostgreSQL 机器人、协作目录、消息与运行的 v2 HTTP 协议，复用 `authapi.Handler.Protect("desktop", …)` 的 HTTPS、来源、访问凭据和请求时限。所属用户只从认证结果取得，领域服务在租户事务内复核身份及所属关系。

管理 Cookie、客户端提交的 `user_id`、模型配置、宿主机路径和旧工作空间整份覆盖都不是这里的授权或输入方式。本模块已挂入新的 `tomd` [应用组合](../serverapp/README.md)，并与执行 Worker 一同启动；实时事件订阅、客户端切换和真实节点联合验收仍待完成。

## 路由

| 方法与路径 | 语义 |
| --- | --- |
| `GET /api/v2/workspace` | 有限目录首页、工作空间选择及同快照事件水位 |
| `PUT /api/v2/workspace` | 按期望修订替换当前和主机器人选择，显式空值清除选择 |
| `GET/POST /api/v2/bots` | 分页目录或原子创建机器人、稳定桌面身份及唯一单聊 |
| `GET/PUT /api/v2/bots/{botID}` | 详情或按期望修订替换配置 |
| `POST /api/v2/bots/{botID}/state` | 归档或恢复 |
| `GET/PUT/DELETE /api/v2/bots/{botID}/avatar` | 读取、替换或删除用户自己的自定义 PNG |
| `GET/POST /api/v2/groups` | 分页目录或幂等创建群组、成员与唯一群聊 |
| `GET/PUT /api/v2/groups/{groupID}` | 详情或原子替换群组配置和成员 |
| `POST /api/v2/groups/{groupID}/state` | 归档或恢复群组 |
| `GET /api/v2/conversations` | 分页读取所属用户会话目录 |
| `GET /api/v2/conversations/{conversationID}` | 会话目录详情，不返回消息正文 |
| `GET/POST /api/v2/conversations/{conversationID}/messages` | 分页消息或幂等受理单聊消息与持久任务 |
| `POST /api/v2/conversations/{conversationID}/read` | 按 `sequence` 推进已读位置，成功返回 `204` |
| `GET /api/v2/runs/{runID}` | 当前用户的运行状态与固定错误码 |
| `POST /api/v2/runs/{runID}/cancel` | 提交空对象请求取消，返回实际运行状态 |

目录列表接受 `before`、`limit`、`archived`，默认 50 条、未归档，最大 100 条；`before` 为上一页末项 ID，`archived` 只接受 `true` 或 `false`。工作空间只接受 `limit`。消息列表只接受 `before` 和 `limit`，按序号倒序，`before` 为排他的正整数序号；省略表示最新页，默认 50 条、最大 100 条。未知、重复或无效查询参数被拒绝，其他路由不接受查询参数。

创建使用 `client_request_id`，第一次创建返回 `201`，相同请求重试返回 `200`；两者都提供资源 `Location` 与 `is_created`。冲突时返回 `409`，客户端不能自动换键重复创建。修改使用 `expected_revision`，发生冲突后重新读取再由用户决定。状态接口还必须显式提交布尔字段 `is_archived`。

机器人请求字段见 [bots.Spec](../bots/types.go)，群组请求字段见同文件的 `GroupSpec`。它们不包含任何客户端模型选择。机器人配置正文最多 64 KiB，群组最多 16 KiB，状态与工作空间修改最多 4 KiB。只接受单个 UTF-8 JSON 对象，拒绝未知字段。

机器人响应中的 `desktop_id` 由服务器登记，同键重试、归档恢复和电脑重建都保持稳定。创建或修改请求不能提供该字段，也不能指定 `linux_uid`、`shared_gid` 或 `display_number`。桌面 ID 仅标识资源，不能替代执行授权。

发送消息使用 `client_request_id` 和 `content`，整个 JSON 上限 64 KiB，文本上限 32 KiB。首次持久入队返回 `202`，原请求重试返回 `200`，响应 `submission` 包含回合、运行、消息 ID 与 `is_created`，`Location` 指向运行。受理不表示模型已经执行。活动会话、队列满和同键不同内容返回明确 `409`；群聊和 Bot 私信需要各自编排入口，当前发送接口明确拒绝。已读必须显式提交 `sequence`，不接受未来序号。

运行响应只包含状态元数据，不包含模型选择、机器人提示词或操作账本。`computer_wait_reason` 为最近一次桌面调度观测：空字符串表示当前没有桌面等待记录，`desktop_busy` 表示所属桌面被占用，`user_capacity` 表示用户并发额度或较早的可执行桌面排队，`starting_desktop` 表示节点启动尚待确认。取消排队任务返回 `canceled`；在途任务返回 `canceling`，之后继续查询实际终态，不能在取消请求返回时假定外部操作已停止。客户端没有领取任务、提交模型结果或自行构造租约的接口，具体约束见 [execution](../execution/README.md)。

上传头像使用 `expected_revision` 和 `content_base64`，只携带 PNG 的标准 Base64 正文，不使用 data URL。JSON 上限 100 KiB，解码后的图像限制由领域服务执行。读取返回 `image/png`、禁止缓存和 MIME 嗅探、同源资源策略及沙箱 CSP；不返回重定向、远程图片或内联 HTML。删除头像提交期望修订，不能跨用户删除。

## 错误与日志

错误格式统一为 `code`、中文 `message`、`request_id`。输入无效为 `400`，会话无效为 `401`，群邀请拒绝为 `403`，不存在或其他用户资源为 `404`，修订、活动重名、归档和容量冲突为 `409`，超时为 `504`，未分类领域或数据库失败为固定 `503`。不能透出 SQL、数据库行内容、提示词或请求正文。

响应全部经过认证门，设置 `no-store`、`nosniff` 与服务端请求标识。错误日志只记录请求和用户标识及固定分类；不序列化输入或下层错误。结果未知的修改应先重新读取状态；创建可使用原幂等键核实，不能盲目更换请求身份。

## 验证与接入边界

联合测试用真实 PostgreSQL、真实身份服务和本地 HTTPS 完成两个账户的激活、登录、创建与修改、分页、头像、群组、会话、工作空间、归档恢复、退出及禁用；并验证消息持久入队、同键重试、运行查询、分页已读、取消及新回合。额外覆盖跨用户消息与运行、正文身份和模型注入、私有调度路由拒绝、未接入会话类型、非法序号与正文、旧修订、管理 Cookie、跨来源、数据库故障脱敏及原有 v1 路径拒绝。

测试中的路由组合用于验证协议，不替代生产启动入口。仍需接通主服务、桌面主进程的账户会话与安全存储、真实事件订阅、Worker 生产配置、群聊及私信编排。归档资源会阻止新运行启动和新操作，Worker 续约时也复验状态；生产部署中的撤销通知仍需完成。
