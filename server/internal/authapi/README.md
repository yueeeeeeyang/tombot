# 新认证 HTTP 接口

本模块提供多用户认证传输协议，使用 `identity.Service` 完成身份规则。已通过真实 PostgreSQL 与 HTTPS 联合测试，并挂接到新的 `tomd` [应用组合](../serverapp/README.md)；完整节点与客户端部署仍待验收。

## 入口与响应

桌面认证前缀为 `/api/v2/auth`，管理平台认证前缀为 `/api/v2/admin/auth`。

| 方法与后缀                   | 用途及输入                                                    |
| ---------------------------- | ------------------------------------------------------------- |
| `POST /login`                | 用户名、口令、动态验证码、设备名；桌面端还提供平台            |
| `POST /refresh`              | 桌面提交续期值；管理端提交空 JSON 对象，续期值来自安全 Cookie |
| `GET /session`               | 当前 Principal；管理端同时返回当前 CSRF 校验值                |
| `POST /logout`               | 提交空 JSON 对象，撤销当前会话                                |
| `POST /password`             | 当前口令、新口令及管理员动态验证码；成功后全部会话失效        |
| `POST /verify`               | 口令和动态验证码，用于刷新敏感管理操作的验证窗口              |
| `GET /devices`               | 当前用户有效设备；`before` 游标及 `limit` 有界分页            |
| `DELETE /devices/{deviceID}` | 撤销当前用户指定设备；不能通过用户标识切换操作对象            |

激活入口仅为 `POST /api/v2/auth/activate`，输入 `activation_token`、`password`、`code`。管理员验证器登记入口为 `POST /api/v2/auth/admin-enrollment`，输入未消费的激活值，返回需要短暂展示的登记材料。两者均不接受 URL 中的凭据。

客户端提交 JSON 对象，最大 16 KiB；未知字段、多个对象、无效 UTF-8 及其他内容类型会被拒绝。会话范围由路由决定，不能提交 `scope`、`role` 或 `user_id` 覆盖身份。身份由访问凭据解析，其他业务路由也必须沿用相同 Principal 契约。

业务 HTTP 模块通过 `Protect` 复用相同安全边界，回调接收已验证的 Principal 与服务端生成的请求标识。范围由服务入口固定，管理处理器只能挂接 `/api/v2/admin/` 命名空间，不能借路由别名跳过来源或 CSRF。领域层仍须执行当前管理员权限、二次验证及资源所属校验。

所有认证响应设置 `Cache-Control: no-store`，包含服务端生成的请求标识。错误响应固定为 `code`、`message` 和 `request_id`，不传递数据库错误内容、原始请求或凭据。参数错误、认证失效、权限拒绝、限流和服务不可用分别使用明确 HTTP 状态；审计或数据库错误不转换为登录成功。

## 桌面与管理平台

桌面登录及续期返回访问值、续期值、过期时间和 Principal，不设置管理 Cookie。后续请求使用 `Authorization: Bearer …`，不从查询参数或 Cookie 读取身份。桌面主进程保存凭据、串行续期及向渲染层提供受限 IPC 的接入仍待客户端阶段完成。

管理平台登录及续期仅把两份会话值写入 `__Host-` Cookie，设置 `Secure`、`HttpOnly`、`SameSite=Strict`、根路径及绝对过期时间，不设置 `Domain`。JSON 仅返回 Principal、过期时间和 CSRF 校验值，不把会话值交给页面脚本。退出、改密及撤销当前设备后清除两份 Cookie。

管理写请求必须同时满足：

1. `Origin` 与部署配置的管理站点完全一致；拒绝跨站和同站其他来源的 Fetch Metadata。
2. 已登录操作的 `X-CSRF-Token` 与服务端计算结果一致。计算采用独立密钥的 HMAC-SHA-256，并绑定当前 HttpOnly 续期 Cookie。旧会话或其他会话的校验值不能用于当前请求；重复头和重复 Cookie 被拒绝。
3. 校验完成后才调用身份服务。管理续期也遵守此顺序，不能在 CSRF 验证之前轮换或撤销会话。

CSRF 值在登录、续期或查询当前会话后从 JSON 读取，页面只需保存在内存中。刷新页面后可查询当前会话重新取得；续期成功后使用新值，不自动重发已可能提交的敏感请求。实现采用绑定会话的签名校验及来源核验，依据 [OWASP CSRF 防护说明](https://cheatsheetseries.owasp.org/cheatsheets/Cross-Site_Request_Forgery_Prevention_Cheat_Sheet.html)。

## 部署约束

所有认证接口要求 HTTPS。直连连接使用实际 TLS 状态和对端地址，不信任任意转发头；只有外部配置明确列出的代理网段可以覆盖来源。受信代理必须覆盖而不是追加 `X-Forwarded-For`，只转发一个合法客户端 IP，并设置 `X-Forwarded-Proto: https`；不明确的代理链直接拒绝。

`Options.AdminOrigin`、`TrustedProxies`、`CSRFKey` 和 `RequestTimeout` 由部署配置提供。CSRF 密钥与模型、MFA 加密材料分开管理，不向浏览器、用户容器或日志暴露。此模块的请求上下文超时不能代替 HTTP Server 的读取、写入与空闲超时，运行入口须同时配置传输层时限。

## 验证范围

协议测试覆盖安全 Cookie、CSRF 在业务操作之前执行、续期后校验值变化、跨会话和重复值拒绝、来源及代理伪造、JSON 限制、身份参数注入、凭据通道隔离、错误脱敏和真实 HTTPS Cookie 往返。独立联合测试使用真实临时 PostgreSQL 与 HTTPS 服务执行两用户激活、登录、设备隔离、续期及重放撤销。

联合测试中的电脑记录仅作为身份前置条件夹具，不创建真实 Docker 电脑，也不代替同步开户与容器验收。完整运行入口、管理页面、桌面凭据保管以及长连接撤销尚在后续实施范围。
