# 会话执行状态与真实模型验收

进入单聊后，顶部仅悬浮显示 Bot 头像与名称，点击打开详情；执行与停止动作保留在输入区，审批和连接错误显示在其上方。在另一台设备发送的消息也会进入本机的排队或执行状态，可以在本机停止。群组按成员显示排队、执行、等待回答或审批。并行执行时，成员各自显示所处步骤。

## 数据来源与续传

- `GET /api/v1/conversations/{id}` 在一次读事务内取得最近 50 条消息、事件游标、`active_turn_id`、`active_turn_status` 和 `active_runs`。每个活动 Run 返回 `model_name`，有正在进行的工具时附 `current_tool`。即使该调用的原消息已在首屏之外，也能恢复当前步骤。
- 含工具请求或结果的消息带有 `tool_calls`。历史消息分页和快照使用同一投影。客户端按 `tool_call_id` 合并卡片，折叠时显示工具、目标摘要、状态与实际耗时，展开后可读调用参数和结果。若请求位于更早一页，先单独展示已加载的结果。
- `tool.started`、`tool.status`、`tool.output` 使用同一组字段：`tool_call_id`、`run_id`、`name`、`step`、`status`、`input_preview`，结束时可带 `duration_ms` 和 `error_code`。数据库步号从零开始，界面从第一步开始显示。耗时从开始处理工具起算，包含等待审批的时间。
- 准备、等待审批、重新核对、实际执行和结束分别进入持久事件流。审批通过后重新验证目标与策略，再提交执行状态并调用工具。拒绝或取消保留对应结果。状态与事件同事务提交，提交失败会阻止即将开始的外部动作。回合结束后客户端重新读取快照，恢复中断或取消的消息标记。
- 活动摘要只发布工具名、参数字节数和已脱敏的目标或模式摘要。历史记录中其他输入字段不会进入活动事件。展开的聊天参数和结果来自已有的、需要设备鉴权的会话消息。排队事件带回合、Run、Bot 标识，模型与步骤按实际 Run 关联。
- 每页合并一次按调用主键和活动 Run 索引的批量查询。正文和思考各自的首个非空增量立即提交，之后每 500 毫秒或积累 4 KiB 时合并写入。相比整轮批量写入，每个模型步骤至多增加两次首段提交。三次本机环回测量中，从可控上游发出首段到会话 SSE 收到该段约 8.1–8.5 毫秒。

## 本地真实推理复验

三路会话的实时转发可用 `TOM_TEST_SSE_LATENCY=1 go test -count=1 -run '^TestLiveSSEForwarding$' -v ./internal/httpapi` 复验。每个会话发出 32 个持久事件，从开始写 SQLite 计时到环回 HTTP 客户端完整读到本条正文。本机 96 个样本的中位数为 0.26 毫秒，P95 为 0.58 毫秒，最长 0.78 毫秒。此口径单独衡量 SSE，将供应商推理和上文所述的增量合并分别测量。

macOS ARM64 验收使用 Qwen2.5-0.5B-Instruct 的 Q4_K_M 权重和 llama.cpp b12144。GGUF 的 SHA-256 为 `74a4da8c9fdbcd15bd1f6d01d621410d31c6fc00986f5eb687824e7b93d7a9db`。推理服务只监听环回地址，验证过程使用临时业务数据库和工作区。权重和推理服务是可选的验收依赖。[Qwen 固定版本权重](https://huggingface.co/Qwen/Qwen2.5-0.5B-Instruct-GGUF/tree/9217f5db79a29953eb74d5343926648285ec7e67) · [llama.cpp b12144](https://github.com/ggml-org/llama.cpp/releases/tag/b12144)

在一个终端启动已安装的 llama-server，将 `QWEN_GGUF` 指向已校验的权重文件。

```sh
llama-server --model "$QWEN_GGUF" --alias tom-verification-qwen \
  --host 127.0.0.1 --port 18081 --ctx-size 4096 --parallel 1 --threads 4 --offline
```

在项目根目录用另一终端运行。macOS 还可以设置 `TOM_TEST_NATIVE_MODEL=1`，让正式聊天组件在同一会话追问并停止长回复。

在 Android 上设置 `TOM_TEST_NATIVE_MODEL=1` 及 `TOM_TEST_BOT_DEVICE=emulator-<端口>`，并将 Android SDK 的 `platform-tools` 加入 PATH。Go 验收程序仅为本次临时服务转发一个端口，用例结束后撤销。群组分工用例使用同一设备参数与 `TOM_TEST_NATIVE_GROUP=1`。

```sh
TOM_TEST_LOCAL_MODEL_URL=http://127.0.0.1:18081/v1 \
TOM_TEST_LOCAL_MODEL=tom-verification-qwen \
go test -race -count=1 ./internal/httpapi -run '^TestLocalModelChatStreaming$' -v -timeout 6m
```

三次独立会话都收到了“7 加 5”的正确流式答案。请求提交至首段到达分别约为 268、78、78 毫秒，最终文本与增量拼接一致。服务记录的输入分别为 213、214、211 个 token，输出均为 3 个。原生客户端另一次打开已有回复“12”，追问再加 2，收到“14”。停止下一条长回复后，推理服务记录取消并释放推理槽位，客户端恢复空闲并在重新同步后显示部分正文。

同 Bot 在活跃回合中再次发送返回 409。独立的 Shell 验收让模型通过审批发起长命令，确认子进程已运行后调用回合停止接口，核验系统已回收该 PID、工具和回合均已取消，且模型没有进入下一步。文件、Shell、终端、浏览器的批准与拒绝另由可控上游覆盖。托管供应商的网络环境与商业账单需要在相应账号下另行核对。

## 升级与回退

服务和客户端一起发布，继续使用数据库版本 0014。更新前等待活跃回合结束，或主动取消并等待收尾。旧版持久事件缺少完整工具摘要时，客户端重新读取当前会话快照。回退时使用同样方式停止在途任务，再将服务和客户端一并换回上一版支持 0014 的构建，保留数据库、密钥文件和工作区。
