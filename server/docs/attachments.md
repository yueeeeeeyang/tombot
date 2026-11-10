# 聊天附件与视觉输入

输入框内左侧的加号打开系统文件选择器。选中的文件立即上传，附件标签显示在输入边框内顶部，上传完成后即可发送。单条消息最多附四个文件，可以只发附件。移除草稿中的文件会移除引用，工作区中的原件仍可从文件面板取回。发送失败可直接重试，重新上传会生成新路径。历史消息和浏览器截图的保存按钮会校验原始摘要。

## 文件范围与接口

在 macOS 可将普通文件拖入输入区，或输入 `/attach` 打开系统选择器。拖入目录、超过草稿四项余量的一批文件，界面会给出原因。文件逐项上传，失败前已确认的附件保留在草稿中。macOS 沙箱访问权限随本次读取释放。拖放只需更新客户端，回滚时退出应用后安装旧客户端。

- 私有单聊使用 Bot 的 `inbox/<ULID>/<文件名>`。群组和 Bot 私信使用 `shared/inbox/<ULID>/<文件名>`。共享收件目录对可访问共享工作区的 Bot 可见。协调者和执行成员可以读取本次共享的材料，成员自己的浏览器工具结果仅进入自己的模型请求。
- `POST /api/v1/fs/upload?bot_id=<id>` 与 `POST /api/v1/fs/upload?scope=shared` 都需要设备鉴权，并且只接收一个名为 `file` 的 multipart 文件。客户端使用响应中的 `path` 和 `sha256` 发送消息。
- `POST /api/v1/conversations/<id>/messages` 接受可选的 `attachments` 数组，每个元素由 `path` 与 `sha256` 两个字符串组成。只有附件时可将 `content` 设为空字符串。服务在登记消息前验证文件范围、数量、重复路径、原始摘要与图像边界。传输和发送分别确认，一次写入失败后由用户决定是否重试。
- 工作区可上传最大 32 MiB 的文件。聊天接受的 PNG 和 JPEG 最大 4 MiB，每边最多 4096 像素且总计不超过 400 万像素。服务从内容识别类型，对这两类图片解码并拒绝损坏或越界的图片。长文本和其他二进制文件在请求中以工作区路径呈现。

## 模型会收到什么

调用已配置的供应商时，可内联的材料会发送给该供应商。视觉模型收到 PNG 或 JPEG 的原始字节，纯文本模型收到图片路径及缺少像素的说明。识别为文本、有效 UTF-8、无空字节且不超过 64 KiB 的文件会作为引用材料附在提示词内。文件名和文件内容均按不受信任的输入处理。

浏览器截图保存为 Bot 收件目录下的 JPEG。截图工具成功后，OpenAI 兼容协议先发完该批工具的回复，再发包含图像的用户消息。Anthropic 协议将图像绑定到相应的工具结果。已有文本工具和非视觉模型仍可使用路径。协议字段参见 [OpenAI 图像输入](https://developers.openai.com/api/docs/guides/images-vision) 和 [Anthropic 视觉输入](https://platform.claude.com/docs/en/build-with-claude/vision)。

会话快照、事件和 SQLite 保存路径、摘要、尺寸和类型。模型请求从最新消息向前为最多八项内联材料预留上下文预算，然后从工作区读取并再次验证摘要。历史消息只有在完整回合还位于上下文窗口时才可能携带材料。当前回合的原件更改会使执行失败；保留的历史原件已更改或删除时，模型收到原件不可用的说明。较早被裁剪的回合可进入文字摘要。

文件哈希采用流式读取，同时进行的文件校验和图像解码最多两项。每次请求最多装配八项材料，传输图片时还会产生 Base64 编码的临时内存。上下文按图片尺寸估计占用，计费优先采用供应商报告的用量。下载失败或摘要不匹配会终止保存，桌面端清理未完成文件。

## 升级、回滚与复验

一起部署服务端和客户端。此功能沿用数据库版本 14 的消息块和已有工作区文件，升级无需执行迁移。回滚前等待活动消息、文件传输和模型调用收尾，停机备份数据目录，再切换至此前支持版本 14 的服务与客户端。旧客户端可能无法呈现附件消息块；如需完整历史展示可恢复本版本，或恢复升级前的整实例备份，后者会回到备份时刻。

从仓库根目录可运行以下验收。真实浏览器用例需要当前平台匹配的已校验 Chromium 归档。测试通过环回 HTTP 校验上游实际收到的图像和文字。

```sh
go test -race ./internal/workspace ./internal/agent ./internal/provider ./internal/httpapi
TOM_TEST_BROWSER_ARCHIVE=/绝对路径/chrome-platform.zip go test -race ./internal/httpapi -run '^TestRealChromiumScreenshotToVisionModel$' -count=1 -timeout 4m
(cd client && flutter test test/chat_attachments_test.dart)
```

在 macOS 上使用以下命令复验原生桌面。第一项从磁盘读取测试文件并检查聊天界面回执。第二项打开常规原生窗口，按日志中的临时路径选择文件，看到草稿中的文件名后点击发送。服务在验证上游正文和最终消息后自动退出临时实例。运行完成后可用 `cd client && flutter build macos --debug` 重建常规入口。

```sh
TOM_TEST_NATIVE_ATTACHMENT=widgets go test -race ./internal/httpapi -run '^TestNativeChatAttachment$' -count=1 -v -timeout 6m
TOM_TEST_NATIVE_ATTACHMENT=1 go test -race ./internal/httpapi -run '^TestNativeChatAttachment$' -count=1 -v -timeout 6m
```

连接专用 Android 模拟器，将 `ANDROID_SERIAL` 设为 `adb devices` 显示的设备号，在客户端目录运行下列命令。先在系统文档面板确认保存，再选择刚保存的 `tom-doc-*.txt`，最后在再次打开的选择器按返回。用例核对文件名、长度、全部中文 UTF-8 字节及取消结果。可在模拟器的“下载”目录清理测试文件。

```sh
flutter test -d "$ANDROID_SERIAL" --dart-define=TOM_TEST_NATIVE_DOCUMENTS=true integration_test/android_documents_test.dart
```
