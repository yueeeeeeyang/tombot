# 桌面服务迁移

本文件记录把 [tom](https://github.com/yueeeeeeyang/tom) 的 Go 服务端迁入 `server/`，并和现有 `desktop/` 界面对齐的范围。界面以 desktop 为准：字段名、八只预设猫咪、侧栏、聊天和猫咪面板都不为了迁就原来的 Bot 协议而改样。Flutter 客户端不再保留兼容。

## 复用了什么

这些都是 tom 原来的模块，没有另写一套：

- 初始化、Argon2id 密码和 `tomd_` 设备凭据（`internal/auth`）。初始化码仍只打印在标准输出。凭据只留在 Electron 主进程。
- 模型目录、密钥和供应商（`internal/service` 的 Catalog / Secret）。桌面设置里的地址、模型名称和密钥写入原来的 provider、model、secret。
- Bot、工作区目录和单聊（`Bots.Create` / `Update`）。桌面猫咪就是这张表里的一行，`desktop_id` 用客户端原来的助手标识。
- agent 循环、工具、审批策略和模型流式调用（`agent.Runtime.Submit`、`provider.HTTP.Stream`）。发送消息只把正文交给运行时，不再自带 chat/completions 客户端。
- 运行过程事件（`text.delta`、`thinking.delta`、`tool.started`、`turn.finished`）。`/api/v1/messages/stream` 把这些事件转成桌面气泡能画的增量，客户端断开不会取消已经开始的运行。
- 电脑页读取原来的引导阶段、文件树和持久浏览器状态（`Provision.Snapshot`、`Files.Tree`、`Browser.Status`）。终端仍由服务端 agent 持有。

`/api/desktop/v1` 和第一阶段那个独立的补全客户端已经删除。桌面走原来的 `/api/v1`：

| 桌面操作 | 接口 |
| --- | --- |
| 是否已初始化 | `GET /api/v1/auth/status` |
| 初始化 | `POST /api/v1/auth/setup`，正文 `{password, setupCode}` |
| 验证密码 | `POST /api/v1/auth/login`，正文 `{password, deviceName, platform}` |
| 读写八只猫咪的界面文档 | `GET` / `PUT /api/v1/workspace`，`POST /api/v1/workspace/import` |
| 模型表单 | `GET` / `PUT /api/v1/model-settings` |
| 发送并观看 agent | `POST /api/v1/messages/stream` |
| 打开这只猫咪的电脑 | `POST /api/v1/computer`，画面在 `GET /api/v1/computer/view` |

初始化、登录、工作空间和电脑页不要求主机引导完成。其余 `/api/v1` 业务仍要等引导。`TOM_DESKTOP_COMPANION=1` 跳过字体和浏览器安装，方便桌面在引导完成前工作。

流式开关在客户端：打开时按 `delta` / `process` 逐段画进原来的气泡；关闭时忽略增量，等 `done` 后一次显示。失败时保留已经生成的正文，并把用户消息标为 `failed`，界面上的重试按钮继续可用。

## 表结构

迁移 `0020_desktop_columns.sql` 把 `desktop_state` 拆进原来的表，然后删掉 `desktop_state` 和 `desktop_agent_link`。`0018`、`0019` 没有改。

| 桌面字段 | 落点 |
| --- | --- |
| 名称、介绍、分组、头像预设、草稿、电脑地址、`modelId` | `bots` 的原有列和新列 `description`、`group_name`、`avatar_preset`、`draft`、`computer_url`、`catalog_model_id` |
| 当前猫咪、修订号、是否还是预设 | `workspace_meta` 的标量列 |
| 模型表单和加密后的 API 密钥 | `workspace_meta` 的标量列。真正调用仍写入原来的 provider、model、secret |
| 消息正文 | `messages.blocks_json` 里的 text block。agent 的工具块和桌面气泡是同一行 |
| 状态、请求标识、回复 | `messages.status`、`messages.request_id`、`messages.reply_to_id` |
| 表情 | `message_reactions` |
| 资料库 | `bot_artifacts` |

已有 `desktop_state` 会在删表前抄进这些列。和 agent 已经写过的同一条正文不会再插一遍。本机 v1–v5 旧档导入也走这套列，不再写 JSON。

招聘时选中的模型名称写入这只猫咪的 `catalog_model_id`。发送时在原来的模型目录里找到或创建同名模型，`bots.model_id` 指向它，agent 运行时用这一行。

主题、侧栏宽度、置顶、未读和隐藏仍在本机。服务没有对应的界面状态。

新增页面只有初始化、验证密码和无法连接。样式沿用现有表单、主按钮和猫咪头像。电脑页沿用原来的未连接、全屏查看和断开；多了一个「连接电脑」，手动地址仍可作为备选。

## 还没做

- 审批、例程、记忆、群组协作、Bot 私信、用量和审计还没有桌面页面。
- 配套模式不下载 Chrome，所以电脑页里的持久浏览器在引导完成前是未就绪。引导由原来的 provision 负责，这一阶段没有在端到端里自动安装浏览器。
- 语音、附件和云端分享。
- 招聘弹窗里的模型名称只决定能否招聘，不会写进猫咪的 `modelId`。详情页里的目录模型才会保存。
- 默认 Bot 仍由原服务在初始化后准备，桌面侧栏不显示它。

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

不设置 `TOM_DESKTOP_COMPANION` 时，`tomd` 会按原来的方式做主机引导，包括准备浏览器。

## 检查

```sh
cd server && go test ./...
cd desktop && npm run check && npm run test:e2e
```

端到端测试会自己构建并启动配套服务，每个用例使用独立数据目录。

## 回滚

`0018` 到 `0020` 都是向前迁移，不改历史脚本。`0020` 会删除 `desktop_state`。回滚程序前先停机备份 `TOM_DATA_DIR` 和 `TOM_WORKSPACE_DIR`。旧二进制打不开版本 20 的数据库，也不能把删掉的 JSON 找回来。客户端回滚后会重新读取本机旧档；已经导入到服务的内容不会自动写回 localStorage。
