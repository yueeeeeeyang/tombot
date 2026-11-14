# 受管模型调用与能力验证

本模块执行服务端固定模型版本的流式调用，并实现 `modelregistry.Verifier`。消息与工具由受信执行器传入；模型名称、协议、地址、输出上限和声明能力来自不可变配置，供应商凭据仅在本次调用期间使用。新持久 Worker 与 `tomd` [应用组合](../serverapp/README.md) 已接入本模块。

## 部署边界

`New` 要求配置连接及请求时限、请求与响应字节上限和并发槽位。供应商 HTTPS 地址无需域名或公网 IP 白名单，支持内网及代理 Fake-IP；不接受 URL 中的身份、查询串或片段。管理接口不能修改部署拒绝网段。

`DeniedNetworks` 用于额外排除控制平面、数据库、节点管理和本地网络转换目标。取消公网 IP 限制后，内网模型地址由管理员登记；部署须按实际网络配置拒绝范围，避免访问其他基础设施。

每次调用重新解析 DNS，整组地址中出现禁止地址就拒绝调用；通过校验后连接数字地址，连接器不会再次解析域名。禁止环回、链路本地、已知云节点特殊地址，以及标准 NAT64、6to4、Teredo 转换范围，这些禁止地址不能通过供应商配置放行。IPv4 映射地址先还原再检查。网络自定义转换前缀无法从地址本身识别，必须通过部署拒绝范围和网络出口规则封闭。标准转换前缀依据 [RFC 6052](https://www.rfc-editor.org/rfc/rfc6052.html)、[RFC 8215](https://www.rfc-editor.org/rfc/rfc8215.html)、[RFC 3056](https://www.rfc-editor.org/rfc/rfc3056.html) 和 [RFC 4380](https://www.rfc-editor.org/rfc/rfc4380.html)。

TLS 验证证书和原始主机名，最低 TLS 1.2，可由部署注入受信 CA；禁止关闭验证、读取环境代理或跟随重定向。每次调用使用独立 HTTP/1.1 连接，避免复用过时的 DNS 决策及让连接级时限影响其他调用。这会增加握手开销，并发总量由网关槽位约束；未来复用连接需要重新设计策略有效期，不能直接打开连接池。

整体时限覆盖排队、DNS、连接、TLS、请求和响应；连接、TLS、响应头、读空闲、写操作分别受限。响应头限制为 64 KiB，流式调用只接受 SSE，模型发现只接受 JSON，均拒绝压缩正文；单帧及总响应均有限额。超限、流中断、未知协议或回调失败明确返回错误。连接和传输时限分别遵循 [Go Conn](https://pkg.go.dev/net#Conn) 与 [Go Transport](https://pkg.go.dev/net/http#Transport) 的语义。

## 调用与真实用量

生成 POST 只发送一次，包括 `429`、`5xx`、超时和回包丢失。只在 HTTP 请求尚未开始时尝试同一已验证 DNS 结果集中的其他地址；不将有副作用的请求自动重放。传输关闭请求体重建能力，失败保留“结果可能未知”的含义。

`Result.IsComplete` 仅表示收到完整终止协议；用量使用 `provider.ReportedUsage`，没有供应商用量时为 `nil`，单项缺失也为 `nil`，明确报告零才记录零。兼容协议的输入总数包含缓存命中，Anthropic 的普通输入、缓存读取及缓存创建分别报告；`InputIncludesCache` 保留这一区别，避免结算时重复计数。失败时返回已经收到的真实计数，不用字符估算填补未知值。

网关不保存运行、额度或账单。新 [execution Worker](../execution/README.md) 固定策略版本、逐次检查撤销、原子预留额度，并通过 [modelusage](../modelusage/README.md) 保存实际用量或未知状态。主入口已组合，能力探测用量仍需接入同一账本。历史运行链的估算字段仍由待替换的旧代码使用，新调用结果不使用它。

## 发布前探测

`Verify` 使用没有用户内容的临时请求，各项只调用一次，共享整体时限：

1. 文本要求回传本次随机标识，确认实际请求和响应链路。
2. 声明工具能力时要求返回一次指定工具及严格参数，校验名称、数量和随机标识；不执行工具。
3. 声明视觉能力时生成随机顺序的六色色带 PNG，要求按顺序读出颜色；答案不出现在文件名或文本中。

`Probe` 复用同一文本和工具校验逻辑，供模型目录分别显示连通性与工具调用结果；不重复文本探测，也不在目录测试中调用视觉探测。

任一失败或结果未知均阻止发布。探测只是发布时的代表性能力检查，不能保证后续所有任务的模型质量或可用性。它会产生实际供应商调用和可能的费用，界面应说明后再执行管理员已提交的发布操作。

## 验证与变更影响

测试使用真实本地 TLS 服务验证证书、请求编码、SSE、文本及工具和图像探测；DNS 与数字地址连接映射到测试自有服务，不连接公网供应商。测试覆盖混合 DNS 地址、重新解析、无需白名单的公网、内网与代理地址、显式拒绝优先、IPv4 映射和转换地址、域名大小写、代理与重定向拒绝、状态失败不重放、超限、取消、排队和空闲超时。真实供应商能力仍需部署验收。

```sh
go test -race ./internal/modelgateway ./internal/provider ./internal/adminapi
```

`provider.HTTP` 已取消状态码自动重试，当前旧入口也会直接收到首次生成失败，避免不确定请求重复计费。已知费用测试显式提供完整供应商计数，保留原有费用断言。取消重试本身不需要数据库迁移；Responses 的迁移与回滚要求见下文。回滚前应停止接收新任务、排空或记录在途未知请求，不自动补发失败请求，也不得重新开启盲目重试。

## Responses 与模型发现

`responses` 使用 `/responses` 端点，发送完整历史、`store: false`、`max_output_tokens` 和扁平函数工具定义；工具调用与结果通过 `call_id` 配对。流必须以 `response.completed` 完整结束；`incomplete`、`failed`、乱序事件和参数不一致明确失败，保留上游真实用量但不执行失败流中的工具。缓存 token 已包含在输入总量中，新增迁移 `0053_responses_protocol.sql` 同步扩展供应商约束和计量约束。工具能力探测使用自动选择，仍要求一次正确的工具调用和随机参数，适用于不允许强制工具选择的思考模型。协议依据 [Responses 迁移说明](https://developers.openai.com/api/docs/guides/migrate-to-responses) 与 [DeepSeek Responses 说明](https://api-docs.deepseek.com/guides/responses_api/)。

Responses 与 Chat Completions 均从 API 基础地址下的 `/models` 读取目录。读取认证、限流、不支持接口、格式、地址限制与超时分别返回中文提示及请求编号，日志仅记录固定分类与供应商标识。读取不会登记模型；登记采用单模型契约，部分失败后重新读取状态，禁止自动重放。

发现接口按模型读取 `context_window`、`max_output_tokens`、`input_modalities`、`output_modalities`、`effort.supported_levels/default_level` 和明确提供的能力布尔值；Anthropic 同时解析 `max_input_tokens`、`max_tokens`、`capabilities.image_input/effort`。缺失字段保持未知，不按名称推测，显式非法或矛盾规格拒绝导入。字段依据 [DeepSeek 模型列表](https://api-docs.deepseek.com/api/list-models/) 和 [Anthropic 模型列表](https://platform.claude.com/docs/en/api/models/list)。

配置中的非空思考级别分别编码为 Responses 的 `reasoning.effort`、Chat Completions 的 `reasoning_effort`、Anthropic 的 `output_config.effort`；空值不发送，保留上游默认行为。配置已选择思考级别时，工具探测使用自动选择，并严格检查返回的调用及参数，避免强制工具选择与上游思考模式冲突。规格持久化及 `0055` 的升级、回滚见[模型目录](../modelregistry/README.md#供应商规格导入)。

升级时备份数据库及服务配置，删除 `models.origins`（包括其中的 `networks`），新版本会拒绝过时字段。保留 `models.denied_networks`、证书与预算配置，重启服务自动执行新增迁移。约束更新短暂锁定并扫描供应商目录；迁移失败整体回滚并阻止启动。供应商可先停用再通过“编辑”修改协议、地址及凭据，保留模型目录和历史策略快照；此能力由后续迁移 `0054_disabled_provider_editing.sql` 提供，详情见[模型目录](../modelregistry/README.md#目录测试)。协议或地址改变后须创建并发布新策略版本。回滚应排空运行并恢复升级前数据库、配置与二进制，不能让旧版本读取 Responses 记录。
