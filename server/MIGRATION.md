# 桌面服务迁移

本文件记录把 [tom](https://github.com/yueeeeeeyang/tom) 的 Go 服务端迁入 `server/`，并和现有 `desktop/` 界面对齐的范围。界面以 desktop 为准：字段名、八只预设猫咪、侧栏、聊天和猫咪面板都不为了迁就原来的 Bot 协议而改样。

## 这一阶段做了什么

`server/` 是 tom 的 Go 服务端（不含 Flutter 客户端）。原有 `/api/v1` 路由、数据库迁移 0001–0017 和业务包都保留。新增迁移 `0018_desktop_workspace.sql`，用单独的 `desktop_state` 保存桌面工作空间和模型设置。

桌面接口在 `/api/desktop/v1`，不要求主机引导完成。原来的 `/api/v1` 仍要等浏览器、字体和运行库检查通过。设置 `TOM_DESKTOP_COMPANION=1` 时跳过这些安装，桌面接口可以马上使用。

| 桌面概念 | 服务端 |
| --- | --- |
| 初始化码 + 管理员密码 | 复用 tom 的 Argon2id 初始化。初始化码仍只打印在服务标准输出 |
| 验证密码 | 复用设备登录，签发 `tomd_` 凭据。凭据只留在 Electron 主进程 |
| 八只预设猫咪、分组、草稿、表情、回复、资料库、电脑地址、目录模型 | `desktop_state.workspace_json`，形状与 `tombot.workspace.v5` 相同 |
| 本机 v1–v5 旧档 | 服务还是预设时导入一次；已有改动则拒绝覆盖 |
| 模型供应商、API 地址、模型名称、温度、Top P、输出长度、流式偏好 | `settings_json`。API 密钥用主密钥加密，接口只返回是否已保存 |
| 发送消息 | 未配置地址或模型名称时保持 `unsent`。已配置则请求 OpenAI 兼容的 `/chat/completions`，成功写入助手回复，失败把该条标为 `failed` |
| 主题、侧栏宽度、置顶、未读、隐藏 | 仍在本机。服务没有对应的界面状态 |

新增页面只有初始化、验证密码和无法连接。样式沿用现有表单、主按钮和猫咪头像。

## 还没做

这些能力在 tom 里已经有实现，但 desktop 没有对应页面，这一阶段没有改界面去承载它们，也没有把桌面聊天接到完整的工具循环：

- 主机引导、浏览器安装和修复
- 持久浏览器、终端、文件工作区。电脑页仍是用户保存的 HTTP(S) 地址，空地址就是未连接
- 审批、例程、记忆、群组协作、Bot 私信、用量和审计
- 流式输出。流式开关会保存，回复仍在完成后一次出现
- 语音、附件和云端分享
- 招聘弹窗里的模型名称只决定能否招聘，不会写进猫咪的 `modelId`。详情页里的目录模型才会保存
- 默认 Bot `tom` 仍由原服务在初始化后准备，桌面侧栏不显示它

下一阶段如果要接电脑、文件或工具，应继续让服务输出桌面已经有的字段（例如把产物写进 `artifacts`，或给出可嵌入的电脑地址），而不是改侧栏和面板的结构。

## 运行

在仓库根目录：

```sh
cd server
export PATH="/usr/local/go/bin:$PATH"   # 需要 Go 1.26.5 或与 go.mod 一致的工具链
export TOM_DESKTOP_COMPANION=1
export TOM_ADDR=127.0.0.1:8787
export TOM_DATA_DIR="$PWD/data"
export TOM_WORKSPACE_DIR="$PWD/workspace"
go build -o bin/tomd ./cmd/tomd
./bin/tomd
```

标准输出里的一次性初始化码只用于第一次初始化，不要转发。然后：

```sh
cd ../desktop
npm ci
npm run setup
TOMBOT_SERVER_URL=http://127.0.0.1:8787 npm run dev
```

不设置 `TOM_DESKTOP_COMPANION` 时，`tomd` 会按原来的方式做主机引导，包括准备浏览器。桌面接口在引导完成前也能用。

## 检查

```sh
cd server && go test ./...
cd desktop && npm run check && npm run test:e2e
```

端到端测试会自己构建并启动配套服务，每个用例使用独立数据目录。

## 回滚

桌面表是新增的 `0018`，不改历史迁移。回滚程序前先停机备份 `TOM_DATA_DIR` 和 `TOM_WORKSPACE_DIR`。旧二进制打不开版本 18 的数据库，不能只换回旧程序。客户端回滚后会重新读取本机旧档；已经导入到服务的内容不会自动写回 localStorage。
