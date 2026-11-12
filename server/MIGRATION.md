# 桌面服务迁移（历史记录）

本文记录单用户阶段的迁入过程。当前 `tomd` 已切换到 PostgreSQL 多用户入口，本文的旧认证、初始化码、模型表单和 `/api/v1` 部署说明不再适用于当前服务；现行配置见[应用组合说明](internal/serverapp/README.md)，未完成范围见[实施进度](docs/multi-user-progress.md)。

本文件记录把 [tom](https://github.com/yueeeeeeyang/tom) 的 Go 服务端迁入 `server/`，并和现有 `desktop/` 界面对齐的范围。界面以 desktop 为准：字段名、侧栏、聊天和猫咪面板都不为了迁就原来的 Bot 协议而改样。八只预设猫咪只留在测试夹具里，服务第一次打开工作空间时不再写入。Flutter 客户端不再保留兼容。

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
| 读写工作空间里的猫咪 | `GET` / `PUT /api/v1/workspace` |
| 模型表单 | `GET` / `PUT /api/v1/model-settings` |
| 内置提供商 | `GET /api/v1/model-providers` |
| 用内置地址拉取模型 | `POST /api/v1/model-providers/{id}/models`，正文只含 `apiKey` |
| 发送并观看 agent | `POST /api/v1/messages/stream` |
| 打开这只猫咪的电脑 | `POST /api/v1/computer`，画面在 `GET /api/v1/computer/view` |

初始化、登录不要求主机引导完成，桌面才能在引导过程中验证身份并订阅进度。引导完成前，桌面停在进度页，不进入工作空间。工作空间和电脑页的接口本身仍不检查引导；其余 `/api/v1` 业务要等引导。默认启动会安装字体，并用仓库 `server/browser/` 里的官方离线 zip 准备托管浏览器，不从网络下载，也不跳过这些步骤。

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

已有 `desktop_state` 会在删表前抄进这些列。和 agent 已经写过的同一条正文不会再插一遍。客户端不再读取或导入本机 v1–v5 旧档。服务第一次打开工作空间时写入空列表，不种样例猫咪。侧栏只显示这份服务端数据。

`0022_drop_untouched_sample_cats.sql` 只删除仍与八只样例逐字段一致、且没有消息、资料、草稿、电脑地址、群组、例程、运行、浏览器或终端痕迹的猫咪。改过的、有对话的、以及用户自己招聘的都保留。当前猫咪如果被删掉，改到仍存在的第一只，没有则留空。

`0023_provider_name.sql` 给 `workspace_meta` 增加 `provider_name`，用来保存自定义提供商的显示名称。内置提供商的接口地址不放在这一列，也不交给客户端填写。

内置提供商目前只有 DeepSeek。官方 OpenAI 兼容根地址是 `https://api.deepseek.com`，模型列表是 `GET /models`。服务端用已保存或本次提交的 API Key 调用原来的 `provider.DiscoverDeepSeek`，密钥加密后只留在服务端，响应里只有 `hasApiKey` 和模型标识。测试可以把 `TOM_DEEPSEEK_BASE_URL` 指到本机模拟接口，产品默认仍是官方地址。自定义提供商的名称、接口地址、API Key 和模型名称都由用户填写。

桌面在引导完成后，没有默认模型时停在模型初始化页；还没有猫咪时停在招聘第一只猫咪页。两页都配好才能进入主界面。招聘页直接铺在画布上，不再套一层卡片。

`0024_drop_model_parameters.sql` 删除 `workspace_meta` 的温度、Top P、输出长度和流式开关。这些不再出现在设置接口里。调用模型时输出上限使用 4096，回复仍按流式增量画到界面上。

`0025_lead_agent.sql` 增加 `lead_agent_id`，表示固定置顶的主猫。空工作空间招来的第一只会写成主猫。已经有猫咪时，若还留着标识为 `assistant` 的默认猫，主猫仍是她；否则用最早创建的那只。点击其他猫只改当前会话。其余临时置顶、未读和隐藏仍在本机。

招聘时选中的模型名称写入这只猫咪的 `catalog_model_id`。发送时在原来的模型目录里找到或创建同名模型，`bots.model_id` 指向它，agent 运行时用这一行。

主题、侧栏宽度、未读和隐藏仍在本机。主猫置顶在服务端。

新增页面有填写服务地址、初始化、验证密码、无法连接，以及引导未完成时的进度页。样式沿用现有表单、主按钮和猫咪头像。进度页读取原来的 `GET /api/v1/bootstrap/status` 和 `GET /api/v1/bootstrap/events`，失败时调用原来的 `POST /api/v1/bootstrap/retry`。电脑页沿用原来的未连接、全屏查看和断开；多了一个「连接电脑」，手动地址仍可作为备选。

## 这一批新增

- 猫咪面板的「记忆」只查看。记下、修改和忘掉由服务端 agent 在对话里调用原来的 `memory_search`、`memory_write`、`memory_delete`。读写的仍是同一批 `.tom/memory` 文件。删除不再等待审批。
- 猫咪面板的「定时」没有新建和编辑表单。每个任务只有启用/暂停开关，右键菜单可以删除。创建、修改和删除由 agent 在对话里调用 `routine_list`、`routine_upsert`、`routine_delete`，这三个工具直接走原来的 `Routines.Commit`、`UpdateNow` 和 `Delete`，不等审批。到点仍由原来的调度器执行。
- 电脑页的「开始准备」仍调用原来的 `POST /api/v1/bootstrap/retry`。服务启动时已经做完整引导，浏览器来自仓库里的官方离线 zip。端到端测试用引导夹具模拟进度，不会安装浏览器，也不会点击「开始准备」。
- 记忆、例程、群组和文件接口不再等主机引导完成。
- 用量和审计的 HTTP 入口已去掉。导出对话框不再承诺云端分享。
- 桌面猫咪的系统提示和工具列表包含记忆、例程和 `message_bot`。工具执行不再等待审批。`0021_drop_approval_usage_audit.sql` 删除 `approvals`、`policy_rules`、`audit_log`、`usage_events`、`monthly_budget_alerts`、`routine_changes`。路径越界和密钥不进入响应这些检查还在。
- 新建按钮先弹出菜单，再选招聘猫咪或创建群聊。群聊走原来的群组和 `POST /conversations/{id}/messages`。猫咪用 `message_bot` 发私信时，双方和用户的单聊里按时间插入居中提示。点开后在模态框里靠左用浅色气泡只读查看这段 `bot_dm`，不能代发。消息可以上传图片和文件，走原来的 `/api/v1/fs/upload` 和 `SendInput.Attachments`。导出草稿入口已去掉。

私信在 tom 里是猫咪和猫咪之间的 `bot_dm` 会话，不是用户和一只猫咪的单聊。用户和一只猫咪的对话仍是 `direct`。

## 还没做

- 端到端用 `TOM_BOOTSTRAP_FIXTURE` 模拟引导进度，不安装字体和浏览器。不设置这个变量时，`tomd` 会安装字体，并用仓库里的官方离线 zip 准备浏览器。
- 语音不动。
- 默认 Bot 仍由原服务在初始化后准备，桌面侧栏不显示它。

## 运行

在仓库根目录：

```sh
cd server
export PATH="/usr/local/go/bin:$PATH"   # 需要 Go 1.26.5 或与 go.mod 一致的工具链
export TOM_ADDR=127.0.0.1:8787
export TOM_DATA_DIR="$PWD/data"
export TOM_WORKSPACE_DIR="$PWD/workspace"
go build -o bin/tomd ./cmd/tomd
./bin/tomd
```

四个平台的官方包已经在 `server/browser/`，由 Git LFS 提供。使用前必须先安装 Git LFS，否则 `git lfs` 不是 git 子命令。macOS 执行 `brew install git-lfs`；Debian/Ubuntu 执行 `sudo apt-get install git-lfs`；Fedora 执行 `sudo dnf install git-lfs`；也可以按 [Git LFS](https://git-lfs.com) 安装。装好后在仓库根目录执行 `git lfs install`，再执行 `git lfs pull`。工作区里如果还是指针，引导会读本地 Git LFS 对象，不会把指针当 zip 校验。本地没有对象或系统没有 `git-lfs` 时，失败提示会先要求安装，再给出这两步。默认会做完整主机引导，包括安装中文字体，并从这里复制、校验、解压浏览器，不会访问浏览器分发地址，也不会跳过安装。换位置时再设置 `TOM_BROWSER_ARCHIVE`。平台文件名见 [server/README.md](README.md)。引导完成前，桌面显示进度；完成后才进入工作空间。不要设置 `TOM_BOOTSTRAP_FIXTURE`，它只让端到端测试模拟进度，不是启动方式。

标准输出里的一次性初始化码只用于第一次初始化，不要转发。然后：

```sh
cd ../desktop
npm ci
npm run setup
TOMBOT_SERVER_URL=http://127.0.0.1:8787 npm run dev
```

`TOMBOT_SERVER_URL` 只预填桌面连接页，确认后才会连接。不设置它时打开空白连接页，不会自己去连本机。确认地址并完成初始化或密码验证后，若主机引导还没结束，桌面停在进度页。

## 客户端服务地址

桌面客户端不再默认连接本机 `8787`。服务通常在另一台机器上，客户端只转发消息。

首次打开没有已保存地址时显示连接页，用户确认后才请求 `GET /api/v1/auth/status`。地址写在固定用户目录 `TomBot/desktop-session.json`（应用名在单实例锁之前就定成 TomBot，避免和包名目录错开），和设备凭据放在一起，文件权限 0600。这不是服务端表，没有新的 SQL 迁移。旧文件只有 `deviceToken` 时视为还没有地址；用户保存地址时会清掉旧凭据，避免把上一台服务的令牌发给新地址。

已经保存的地址优先于 `TOMBOT_SERVER_URL`。连不上时失败页可以改地址再试，不会停在「正在连接服务」。

回滚客户端程序后，旧版本会忽略 `serverUrl` 并恢复默认连本机。若这台客户端曾经连的是远端，回滚前应先退出登录或删掉用户目录里的 `desktop-session.json`，避免旧程序把远端凭据发给本机端口。

## 检查

```sh
cd server && go test ./...
cd desktop && npm run check && npm run test:e2e
```

端到端测试会自己构建并启动服务，每个用例使用独立数据目录，并用引导夹具模拟进度，因此不会安装浏览器。

## 回滚

`0018` 到 `0020` 都是向前迁移，不改历史脚本。`0020` 会删除 `desktop_state`。回滚程序前先停机备份 `TOM_DATA_DIR` 和 `TOM_WORKSPACE_DIR`。旧二进制打不开版本 20 的数据库，也不能把删掉的 JSON 找回来。客户端不再读取本机工作空间旧档。回滚到仍会导入旧档的客户端时，本机 localStorage 里如果还留着旧档，可能再次尝试导入；服务端已经有改动时会拒绝覆盖。
