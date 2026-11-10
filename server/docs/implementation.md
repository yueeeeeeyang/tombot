# Tom 实现文档

> 本文是可执行的实现规格：引导自举、目录结构、数据模型、接口契约、事件协议、各模块实现要点、客户端设计系统、测试策略与交付里程碑。设计动机见 [design.md](./design.md)。
>
> 遵守仓库根目录 `CLAUDE.md`：中文注释与日志、最小改动、迁移脚本只增不改、测试覆盖正常/边界/失败路径。

### 命名与标识（全项目唯一来源）

| 位置 | 取值 |
| --- | --- |
| 产品名 | `tom`（标识一律小写，界面与文档显示 `Tom`） |
| Go module | `io.yueyang/tom`（导入如 `io.yueyang/tom/internal/agent`） |
| Flutter 包名 | `tom`（`import 'package:tom/...'`） |
| 应用包标识 | `io.yueyang.tom`（Android applicationId、iOS/macOS bundle id、Linux app id） |
| 服务端二进制 | `tomd` |
| 深链协议 | `tom://` |
| 环境变量 | 统一 `TOM_` 前缀 |
| 数据库 | `data/tom.db` |

实现中新出现的标识必须从这张表来；不允许另起代号、缩写或临时名。

---

## 1. 技术选型与依赖

### 1.1 后端（Go 1.23+，无 CGO）

| 用途 | 选型 | 说明 |
| --- | --- | --- |
| HTTP | `chi/v5` | 轻量，中间件清晰 |
| 数据库 | `modernc.org/sqlite` | 纯 Go，无 CGO，单二进制 |
| 迁移 | 自研 `store/migrate`（`embed`） | 只增不改，启动串行执行 |
| 浏览器 | `chromedp` + 托管的 Chrome for Testing | 由引导层下载并按 sha256 校验 |
| 终端 | `creack/pty` | 进程内 PTY，不依赖 tmux |
| 检索 | SQLite FTS5（trigram + porter 双索引） | 中英文分流 |
| 文本搜索 | 自研 Go 实现（`filepath.WalkDir` + `regexp`） | **不依赖 `rg`**，保证单二进制可用 |
| 调度 | `robfig/cron/v3` | 时区与下次触发时间 |
| 日志 | `log/slog`（JSON） | 脱敏 Handler |
| 加密 | `crypto/aes` GCM + `argon2id` | `golang.org/x/crypto` |
| 测试 | 标准 `testing` + `testify` | 集成测试用临时目录与真实 SQLite |

**硬性约束**：二进制不得依赖 `rg`、`fd`、`tmux`、系统 Chromium、系统 CJK 字体。前四项改为内置实现或自管资产；字体由引导层安装并在自检中验证。

### 1.2 客户端（Flutter / Dart，一套代码全平台）

| 用途 | 选型 | 说明 |
| --- | --- | --- |
| 框架 | Flutter 3.x（Dart 3） | 一套代码编译 macOS / Windows / Linux / iOS / Android |
| 状态 | `riverpod` | 编译期安全、易测试 |
| 路由 | `go_router` | 深链、自适应导航、便于将来补只读页 |
| HTTP | `http` + 自写 SSE 客户端 | SSE 用流式读取 + 游标续传 |
| WebSocket | `web_socket_channel` | 终端与浏览器画面 |
| 安全存储 | `flutter_secure_storage` | 设备令牌、证书指纹 |
| 本地偏好 | `shared_preferences` | 主题、断点偏好、草稿 |
| Markdown | 自建块渲染器 + `flutter_highlight` | `flutter_markdown` 不足以承载工具卡片与产物块，见 13.5 |
| 终端 | `xterm.dart` | 桌面完整交互；移动端只读 |
| 通知 | `flutter_local_notifications`；移动端系统推送（`firebase_messaging` 或自建网关）不在首个版本内，见 M8 后续里程碑 | 没有推送时退化为"打开应用即拉取待审批"，不阻塞交付 |
| 动效 | 内建 `AnimationController` + `flutter_animate`（仅列表进出） | 统一时长曲线令牌 |
| 图标 | 自绘 SVG 图标集（`flutter_svg`） | 统一 1.5px 描边 |
| 测试 | `flutter_test` + `golden_toolkit` | 组件、断点与视觉基线 |

> 不引入第二套 UI 技术：不内嵌 WebView 承载会话界面，不出现 React/TS。局部用 WebView（如某个富文本预览）需在设计评审中单独说明理由。

---

## 2. 目录结构

```
.
├── go.mod                        # module io.yueyang/tom
├── CLAUDE.md / AGENTS.md
├── README.md                     # 部署三步 + 常见问题
├── Makefile                      # fmt lint test build release doctor
├── deploy/
│   ├── tomd.service              # systemd 单元（含非 root 用户示例）
│   └── Caddyfile
├── docs/design.md / implementation.md
├── cmd/tomd/main.go              # 入口：引导 → 就绪门禁 → 起服务
├── internal/
│   ├── provision/                # ★ 引导与自举
│   │   ├── provisioner.go        # 步骤编排、幂等、进度事件
│   │   ├── detect.go             # 发行版/架构/libc/包管理器/权限探测
│   │   ├── packages.go           # apt/dnf/yum/apk/zypper 适配
│   │   ├── fonts.go              # CJK 字体安装与校验
│   │   ├── browser.go            # Chrome for Testing 下载/校验/解包
│   │   ├── selfcheck.go          # 功能自检（中文渲染截图、FTS5、PTY、磁盘）
│   │   └── capabilities.go       # 能力表读写
│   ├── config/
│   ├── infra/{log,ids,clock,bus,crypto}
│   ├── store/{db.go,migrate.go,migrations/*.sql,*_repo.go}
│   ├── domain/
│   ├── agent/{runtime.go,loop.go,context.go,summarizer.go,delegation.go}
│   ├── orchestrator/{planner.go,router.go,guard.go}
│   ├── provider/{provider.go,anthropic.go,openai.go,registry.go}
│   ├── tool/{registry.go,fs.go,shell.go,terminal.go,browser.go,
│   │        memory.go,search.go,collab.go,ask.go,truncate.go}
│   ├── policy/{engine.go,rules.go,reviewer.go}
│   ├── memory/{store.go,search.go}
│   ├── browser/{manager.go,tabs.go,snapshot.go,screencast.go}
│   ├── term/{pty.go}
│   ├── scheduler/cron.go
│   ├── billing/usage.go
│   ├── secret/manager.go
│   ├── service/{bot.go,conversation.go,group.go,run.go,approval.go,
│   │           routine.go,memory.go,fs.go,system.go}
│   └── httpapi/{router.go,middleware.go,sse.go,bootstrap.go,...}
├── client/                       # Flutter 客户端（一套代码全平台）
│   ├── pubspec.yaml
│   ├── lib/
│   │   ├── main.dart
│   │   ├── app.dart              # 主题、路由、能力注入
│   │   ├── design/{tokens.dart,typography.dart,theme.dart}
│   │   ├── api/{client.dart,sse.dart,ws.dart,models.dart,error_messages.dart}
│   │   ├── state/{bots.dart,conversations.dart,messages.dart,approvals.dart,
│   │   │          groups.dart,routines.dart,usage.dart}
│   │   ├── platform/{capabilities.dart,storage.dart,notify.dart,links.dart}
│   │   └── features/{bootstrap,auth,workbench,chat,group,dm,memory,
│   │                 settings,approvals,files,terminal,browser,routines}/
│   ├── test/                     # 单元与 Widget 测试
│   ├── integration_test/
│   └── {macos,windows,linux,ios,android}/   # 各平台壳工程（模板生成，少改）
├── web/health/                   # 唯一服务端页面（构建后 go:embed 进二进制）
└── testdata/
```

---

## 3. 引导与依赖自举（`internal/provision`）

这是"单二进制、启动即全功能"的实现核心，必须先于业务模块完成。

### 3.1 状态机

```
pending → detecting → preparing → selfcheck → ready
                              └──► failed（记录步骤与原因，可 retry）
```

- `system_state` 持久化当前阶段、每步结果、耗时；重启从上次阶段续跑（每步幂等）。
- 就绪门禁：`ready` 之前 `httpapi` 只挂载 `/api/v1/bootstrap/*` 与引导页面；其余路由返回 `503 BOOTSTRAP_NOT_READY` 并带 `retry_after`。**不出现"能用一半"的形态**。

### 3.2 步骤定义

```go
type Step struct {
    ID       string   // detect_os, install_fonts, ensure_browser, selfcheck_browser ...
    TitleZH  string   // 界面文案，例如 "安装中文字体"
    Blocking bool     // 是否阻断就绪
    Run      func(ctx context.Context, st *State) error
    Verify   func(ctx context.Context, st *State) error  // 幂等校验：已满足则跳过 Run
}
```

顺序（全部 Blocking，除非注明）：

1. `detect_os` — 读 `/etc/os-release`、`runtime.GOARCH`、`ldd --version`、`command -v` 探测包管理器；记录 root/免密 sudo。
2. `prepare_dirs` — `data/`、`workspace/`、`data/runtime/`；校验可写、可用空间 ≥ 2GB。
3. `install_fonts` — 检测 `fc-list` 中是否含 CJK 字体；缺失则安装 `fonts-noto-cjk`（Debian/Ubuntu）、`google-noto-sans-cjk-fonts`（Fedora/RHEL）、`font-noto-cjk`（Alpine）、`noto-sans-cjk-fonts`（openSUSE）。Alpine 需同时装 `fontconfig`。
4. `install_runtime_libs` — Chromium 运行库（`libnss3 libatk1.0-0 libatk-bridge2.0-0 libcups2 libdrm2 libxkbcommon0 libxcomposite1 libxdamage1 libxfixes3 libxrandr2 libgbm1 libasound2 libpango-1.0-0 libcairo2`；RPM 名映射在 `packages.go` 的表里维护）。失败先尝试 `--no-install-recommends` 重跑一次，仍失败则给出完整命令。
5. `ensure_browser` — 展开结果和清单已经校验通过则跳过。否则安装官方离线 zip：默认是数据目录同级的 `browser/chrome-<平台>.zip`，`TOM_BROWSER_ARCHIVE` 可改路径。复制到 `runtime/` 后核对 `browser.go` 里的固定版本与 SHA-256，再解压。不从网络下载。备份还原后的运行时归档若摘要匹配，直接复用，不必再提供外部文件。
6. `selfcheck` — 见 3.3。

非 Blocking 步骤（失败只降级能力，界面隐藏入口）：

- `optional_tools`：`git`、`curl`、`unzip`、`python3`（存在则记录，缺失不装）。

### 3.3 自检项

| 检查 | 方法 | 失败含义 |
| --- | --- | --- |
| 中文字体 | 无头浏览器渲染固定中文测试页 → 截图 → 断言非空且与基准图 pHash 距离 < 阈值 | 界面与截图会出方块，必须阻断 |
| 浏览器可用 | 启动 → `navigate data:text/html` → `Page.captureScreenshot` | 浏览器子系统不可用 |
| 根证书 | 访问 `https://` 测试端点（可在配置里换成自建健康地址），或用 `crypto/x509` 加载系统根证书并断言非空 | TLS 失败 |
| SQLite FTS5 | 建临时表写读一次 | 记忆检索不可用 |
| PTY | 起一个 `sh -c 'echo ok'` 的 PTY 并读到 `ok` | 终端不可用 |
| 磁盘 | `data/` 与 `workspace/` 剩余空间 ≥ 1GB | 很快会写满 |
| 时钟 | 系统时间与 HTTP Date 头差值 < 10 分钟 | 影响 token 与 cron，仅告警 |

结果写 `capabilities`（`key, status, version, detail, checked_at`）。

### 3.4 幂等与修复

- 每步 `Verify` 先行；例如字体安装前先 `fc-list :lang=zh`，命中即跳过。
- `POST /api/v1/bootstrap/repair` 对失败步骤重跑；`POST /api/v1/bootstrap/reset` 清空引导状态（保留数据），用于换环境。
- `tomd doctor`（子命令）：只诊断、输出 JSON 与中文报告，不做修改。
- 包安装命令统一：`sh -c "<pm> install -y <pkgs>"`，非 root 时前置 `sudo -n`；`sudo -n` 失败则返回 `BOOTSTRAP_NEED_ROOT` 并附完整命令，绝不静默降级为"跳过"。

### 3.5 引导期间的 HTTP

```
GET  /api/v1/bootstrap/status   → {phase, steps:[{id,title,status,detail,duration_ms}], error}
GET  /api/v1/bootstrap/events   → SSE，每步状态变化推送
POST /api/v1/bootstrap/retry    → 重跑失败步骤
```

客户端引导页直接消费这些事件，不做轮询。

---

## 4. 数据模型

### 4.1 约定

- 主键 `TEXT` + ULID；时间 `INTEGER` 毫秒 UTC；
- 结构变更只新增 `migrations/NNNN_*.sql`，历史文件不可改；
- PRAGMA：`journal_mode=WAL`、`synchronous=NORMAL`、`foreign_keys=ON`、`busy_timeout=5000`。
- 外键删除语义：`bots.model_id` 为 `ON DELETE RESTRICT`（有 Bot 在用则禁止删模型，服务层给出中文提示并引导先换模型）；`providers` 有模型引用时同样 RESTRICT；`groups.coordinator_bot_id` 为 `ON DELETE SET NULL`，服务层在删除 Bot 时同步清理 `group_members` 并提示群组失去协调者。

### 4.2 DDL（`0001_init.sql`）

```sql
CREATE TABLE system_state (
  key        TEXT PRIMARY KEY,
  value_json TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE capabilities (
  key        TEXT PRIMARY KEY,
  status     TEXT NOT NULL,          -- ok | degraded | failed | skipped
  version    TEXT,
  detail     TEXT,
  checked_at INTEGER NOT NULL
);

CREATE TABLE providers (
  id           TEXT PRIMARY KEY,
  kind         TEXT NOT NULL,        -- anthropic | openai_compat
  display_name TEXT NOT NULL,
  base_url     TEXT,
  secret_id    TEXT,                 -- 可空（本地无鉴权端点）
  is_enabled   INTEGER NOT NULL DEFAULT 1,
  extra_json   TEXT NOT NULL DEFAULT '{}',
  created_at   INTEGER NOT NULL
);

CREATE TABLE models (
  id                    TEXT PRIMARY KEY,
  provider_id           TEXT NOT NULL REFERENCES providers(id),
  model_key             TEXT NOT NULL,
  display_name          TEXT NOT NULL,
  context_window        INTEGER NOT NULL,
  max_output            INTEGER NOT NULL,
  input_price_micro     INTEGER NOT NULL,     -- 微美元 / 百万 token（micro-USD per 1M tokens）
  output_price_micro    INTEGER NOT NULL,
  cache_read_price_micro INTEGER,
  supports_tools        INTEGER NOT NULL DEFAULT 1,
  supports_vision       INTEGER NOT NULL DEFAULT 1,
  is_default            INTEGER NOT NULL DEFAULT 0,
  created_at            INTEGER NOT NULL
);
CREATE UNIQUE INDEX idx_models_provider_key ON models(provider_id, model_key);

CREATE TABLE bots (
  id                TEXT PRIMARY KEY,
  name              TEXT NOT NULL,
  avatar_path       TEXT,
  role_title        TEXT NOT NULL,          -- 一句话职责，界面展示
  soul              TEXT NOT NULL,          -- 角色提示词（英文）
  model_id          TEXT REFERENCES models(id),
  tool_policy_json  TEXT NOT NULL DEFAULT '{}',
  messaging_policy_json TEXT NOT NULL DEFAULT '{}',  -- {accept_dm, can_initiate, accept_group_invite}
  workspace_dir     TEXT NOT NULL,
  accent_hue        INTEGER NOT NULL,       -- 0..359，界面身份色
  is_archived       INTEGER NOT NULL DEFAULT 0,
  created_at        INTEGER NOT NULL,
  updated_at        INTEGER NOT NULL
);
CREATE UNIQUE INDEX idx_bots_name ON bots(name) WHERE is_archived = 0;

CREATE TABLE groups (
  id            TEXT PRIMARY KEY,
  name          TEXT NOT NULL,
  purpose       TEXT NOT NULL DEFAULT '',   -- 群组用途说明，注入每个成员的群上下文
  coordinator_bot_id TEXT REFERENCES bots(id),
  routing       TEXT NOT NULL DEFAULT 'mention',  -- mention | coordinator | all
  max_speakers  INTEGER NOT NULL DEFAULT 3,
  max_cost_micro INTEGER,                   -- 单回合成本上限
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);

CREATE TABLE group_members (
  group_id  TEXT NOT NULL REFERENCES groups(id),
  bot_id    TEXT NOT NULL REFERENCES bots(id),
  role_note TEXT NOT NULL DEFAULT '',       -- 在本群的分工备注
  joined_at INTEGER NOT NULL,
  PRIMARY KEY (group_id, bot_id)
);

CREATE TABLE conversations (
  id           TEXT PRIMARY KEY,
  kind         TEXT NOT NULL,        -- direct | group | bot_dm
  source       TEXT NOT NULL DEFAULT 'user',   -- user | bot | routine | delegation（区分委派与闲聊产生的 bot_dm）
  bot_id       TEXT REFERENCES bots(id),       -- direct / bot_dm 的对方（bot_dm 存“另一方”）
  group_id     TEXT REFERENCES groups(id),     -- group 会话
  initiator_bot_id TEXT,                       -- bot_dm 的发起方
  title        TEXT NOT NULL,
  summary      TEXT NOT NULL DEFAULT '',
  summary_upto_seq INTEGER NOT NULL DEFAULT 0,
  last_read_seq INTEGER NOT NULL DEFAULT 0,    -- 左栏未读计算的游标
  is_archived  INTEGER NOT NULL DEFAULT 0,
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL
);
CREATE INDEX idx_conversations_updated ON conversations(updated_at DESC);
CREATE INDEX idx_conversations_bot ON conversations(bot_id, updated_at DESC);
CREATE UNIQUE INDEX idx_conversations_group ON conversations(group_id) WHERE kind = 'group';
-- direct 会话与 Bot 一一对应；闲聊型 bot_dm 按有序对唯一（两端必须非空，服务层校验）
CREATE UNIQUE INDEX idx_conversations_direct ON conversations(bot_id) WHERE kind = 'direct';
CREATE UNIQUE INDEX idx_conversations_dm ON conversations(
  MIN(initiator_bot_id, bot_id), MAX(initiator_bot_id, bot_id)
) WHERE kind = 'bot_dm' AND source != 'delegation';
-- 委派会话（source='delegation'）不参与上面的唯一约束：每次委派独立成会话，可多次并存

CREATE TABLE turns (
  id              TEXT PRIMARY KEY,
  conversation_id TEXT NOT NULL REFERENCES conversations(id),
  seq             INTEGER NOT NULL,
  source          TEXT NOT NULL,      -- user | routine | bot_message
  status          TEXT NOT NULL,      -- queued|running|waiting_approval|succeeded|failed|canceled|interrupted
  plan_json       TEXT,               -- 群组分工计划
  error_code      TEXT,
  cost_micro      INTEGER NOT NULL DEFAULT 0,
  created_at      INTEGER NOT NULL,
  finished_at     INTEGER
);
CREATE UNIQUE INDEX idx_turns_seq ON turns(conversation_id, seq);
CREATE INDEX idx_turns_status ON turns(status) WHERE status IN ('queued','running','waiting_approval');

CREATE TABLE messages (
  id              TEXT PRIMARY KEY,
  conversation_id TEXT NOT NULL REFERENCES conversations(id),
  turn_id         TEXT,
  run_id          TEXT,
  seq             INTEGER NOT NULL,
  sender_kind     TEXT NOT NULL,      -- user | bot | system
  sender_bot_id   TEXT,
  blocks_json     TEXT NOT NULL,
  status          TEXT NOT NULL DEFAULT 'complete',  -- streaming | complete | interrupted | canceled
  token_in        INTEGER NOT NULL DEFAULT 0,
  token_out       INTEGER NOT NULL DEFAULT 0,
  created_at      INTEGER NOT NULL
);
CREATE UNIQUE INDEX idx_messages_seq ON messages(conversation_id, seq);

CREATE TABLE runs (
  id              TEXT PRIMARY KEY,
  turn_id         TEXT NOT NULL REFERENCES turns(id),
  conversation_id TEXT NOT NULL REFERENCES conversations(id),
  bot_id          TEXT NOT NULL REFERENCES bots(id),
  parent_run_id   TEXT,               -- 委派/私信时指向调用方 Run
  source          TEXT NOT NULL,      -- chat | group | routine | delegation | bot_message
  status          TEXT NOT NULL,      -- queued|running|waiting_approval|succeeded|failed|canceled|interrupted
  model_id        TEXT NOT NULL,
  prompt          TEXT NOT NULL,
  plan_role       TEXT,               -- coordinator | member | solo
  error_code      TEXT,
  error_message   TEXT,
  cost_micro      INTEGER NOT NULL DEFAULT 0,
  budget_micro    INTEGER,
  hop_depth       INTEGER NOT NULL DEFAULT 0,
  started_at      INTEGER,
  finished_at     INTEGER,
  created_at      INTEGER NOT NULL
);
CREATE INDEX idx_runs_turn ON runs(turn_id, created_at);
CREATE INDEX idx_runs_bot ON runs(bot_id, created_at DESC);
CREATE INDEX idx_runs_status ON runs(status) WHERE status IN ('queued','running','waiting_approval');

CREATE TABLE tool_calls (
  id            TEXT PRIMARY KEY,
  run_id        TEXT NOT NULL REFERENCES runs(id),
  step          INTEGER NOT NULL,
  tool_name     TEXT NOT NULL,
  input_json    TEXT NOT NULL,       -- 已脱敏
  output_json   TEXT,
  status        TEXT NOT NULL,       -- pending|awaiting_approval|running|succeeded|failed|denied|canceled
  error_code    TEXT,
  approval_id   TEXT,
  duration_ms   INTEGER,
  created_at    INTEGER NOT NULL,
  finished_at   INTEGER
);
CREATE INDEX idx_tool_calls_run ON tool_calls(run_id, step);

CREATE TABLE approvals (
  id           TEXT PRIMARY KEY,
  run_id       TEXT NOT NULL REFERENCES runs(id),
  tool_call_id TEXT NOT NULL,
  bot_id       TEXT NOT NULL,
  action_class TEXT NOT NULL,
  title        TEXT NOT NULL,        -- 中文
  detail_json  TEXT NOT NULL,
  risk_level   TEXT NOT NULL,
  reviewer_note TEXT,
  status       TEXT NOT NULL,        -- pending|approved|denied|expired
  decided_at   INTEGER,
  expires_at   INTEGER NOT NULL,
  created_at   INTEGER NOT NULL
);
CREATE INDEX idx_approvals_pending ON approvals(status) WHERE status = 'pending';

CREATE TABLE policy_rules (
  id           TEXT PRIMARY KEY,
  bot_id       TEXT NOT NULL,        -- '*' 表示所有 Bot
  action_class TEXT NOT NULL,
  pattern      TEXT,
  decision     TEXT NOT NULL,        -- allow | deny
  note         TEXT,
  created_at   INTEGER NOT NULL
);
CREATE UNIQUE INDEX idx_policy_unique ON policy_rules(bot_id, action_class, COALESCE(pattern,''));

CREATE TABLE routines (
  id               TEXT PRIMARY KEY,
  name             TEXT NOT NULL,
  bot_id           TEXT REFERENCES bots(id),        -- direct 目标
  group_id         TEXT REFERENCES groups(id),      -- 群组目标
  cron             TEXT NOT NULL,
  timezone         TEXT NOT NULL DEFAULT 'Asia/Shanghai',
  prompt           TEXT NOT NULL,
  overlap_policy   TEXT NOT NULL DEFAULT 'skip',
  catch_up         TEXT NOT NULL DEFAULT 'once',
  target_mode      TEXT NOT NULL DEFAULT 'new',     -- new | pinned
  pinned_conversation_id TEXT,
  is_enabled       INTEGER NOT NULL DEFAULT 1,
  next_run_at      INTEGER,
  last_run_at      INTEGER,
  created_at       INTEGER NOT NULL
);
-- 约束：bot_id 与 group_id 恰有一个非空（应用层校验 + CHECK）
CREATE TRIGGER trg_routines_target_insert BEFORE INSERT ON routines
WHEN (NEW.bot_id IS NULL) = (NEW.group_id IS NULL)
BEGIN SELECT RAISE(ABORT, 'routine_target_invalid'); END;
CREATE TRIGGER trg_routines_target_update BEFORE UPDATE ON routines
WHEN (NEW.bot_id IS NULL) = (NEW.group_id IS NULL)
BEGIN SELECT RAISE(ABORT, 'routine_target_invalid'); END;

CREATE TABLE routine_runs (
  id           TEXT PRIMARY KEY,
  routine_id   TEXT NOT NULL REFERENCES routines(id),
  turn_id      TEXT,
  trigger_kind TEXT NOT NULL,        -- schedule | manual | catch_up
  status       TEXT NOT NULL,        -- triggered | skipped_overlap | failed
  scheduled_at INTEGER NOT NULL,
  created_at   INTEGER NOT NULL
);
CREATE INDEX idx_routine_runs ON routine_runs(routine_id, scheduled_at DESC);

CREATE TABLE secrets (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL,
  ciphertext BLOB NOT NULL,
  nonce      BLOB NOT NULL,
  hint       TEXT,
  created_at INTEGER NOT NULL,
  rotated_at INTEGER
);
CREATE UNIQUE INDEX idx_secrets_name ON secrets(name);

-- 已授权客户端：只存令牌哈希，明文仅登录时返回一次
CREATE TABLE devices (
  id             TEXT PRIMARY KEY,
  name           TEXT NOT NULL,        -- 客户端上报的显示名，如 "MacBook Pro"
  platform       TEXT NOT NULL,        -- macos | windows | linux | ios | android
  token_hash     TEXT NOT NULL,        -- sha256(device_token)
  token_hint     TEXT NOT NULL,        -- 前 4 + 后 4，用于列表展示
  push_token     TEXT,                 -- 移动推送令牌，可空
  last_seen_at   INTEGER,
  created_at     INTEGER NOT NULL,
  revoked_at     INTEGER
);
CREATE UNIQUE INDEX idx_devices_token ON devices(token_hash);
CREATE INDEX idx_devices_active ON devices(created_at DESC) WHERE revoked_at IS NULL;

CREATE TABLE usage_events (
  id                TEXT PRIMARY KEY,
  run_id            TEXT,
  turn_id           TEXT,
  bot_id            TEXT,
  model_id          TEXT NOT NULL,
  input_tokens      INTEGER NOT NULL DEFAULT 0,
  output_tokens     INTEGER NOT NULL DEFAULT 0,
  cache_read_tokens INTEGER NOT NULL DEFAULT 0,
  cost_micro        INTEGER NOT NULL DEFAULT 0,
  is_estimated      INTEGER NOT NULL DEFAULT 0,
  created_at        INTEGER NOT NULL
);
CREATE INDEX idx_usage_time ON usage_events(created_at);
CREATE INDEX idx_usage_bot ON usage_events(bot_id, created_at);

CREATE TABLE memory_index (
  bot_id       TEXT NOT NULL,
  path         TEXT NOT NULL,
  title        TEXT NOT NULL,
  summary      TEXT NOT NULL,
  tags         TEXT NOT NULL DEFAULT '',
  importance   INTEGER NOT NULL DEFAULT 3,
  size_bytes   INTEGER NOT NULL,
  content_hash TEXT NOT NULL,
  updated_at   INTEGER NOT NULL,
  last_used_at INTEGER,
  PRIMARY KEY (bot_id, path)
);

-- 普通 FTS 表（非 external content），由索引维护器在同一事务内同步写入
CREATE VIRTUAL TABLE memory_fts_ascii USING fts5(
  bot_id UNINDEXED, path UNINDEXED, title, summary, tags, content,
  tokenize='porter unicode61'
);
CREATE VIRTUAL TABLE memory_fts_cjk USING fts5(
  bot_id UNINDEXED, path UNINDEXED, title, summary, tags, content,
  tokenize='trigram'
);

CREATE TABLE audit_log (
  id          TEXT PRIMARY KEY,
  actor       TEXT NOT NULL,         -- user | bot:<id> | system | routine:<id>
  action      TEXT NOT NULL,
  target      TEXT,
  detail_json TEXT NOT NULL DEFAULT '{}',
  ip          TEXT,
  created_at  INTEGER NOT NULL
);
CREATE INDEX idx_audit_time ON audit_log(created_at DESC);

CREATE TABLE settings (
  key        TEXT PRIMARY KEY,
  value_json TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE schema_migrations (
  version    INTEGER PRIMARY KEY,
  name       TEXT NOT NULL,
  applied_at INTEGER NOT NULL
);
```

> 应用层需在 `service` 层再校验一次 `routines` 的目标二者取一（触发器防漏，服务层给出可读错误）。

### 4.3 查询约定

- 列表一律游标分页（`WHERE updated_at < ? ORDER BY updated_at DESC LIMIT ?`），不用 `OFFSET`。
- 写消息在事务内 `SELECT COALESCE(MAX(seq),0)+1` 后插入，配合会话级锁。
- 用量按时间范围聚合走 `idx_usage_time`。

---

## 5. 领域类型

```go
type TurnStatus string  // queued running waiting_approval succeeded failed canceled interrupted
type ConversationKind string // direct | group | bot_dm

type MessageBlock struct {
    Kind       string          `json:"kind"` // text | thinking | tool_use | tool_result | note | artifact | handoff
    Text       string          `json:"text,omitempty"`
    ToolUse    *ToolUseBlock   `json:"tool_use,omitempty"`
    ToolResult *ToolResultBlock `json:"tool_result,omitempty"`
    Artifact   *ArtifactRef    `json:"artifact,omitempty"`
}

// handoff 块用于群组/私信：表示"这条消息是某个 Bot 转交给另一个 Bot 的"
type HandoffRef struct {
    FromBotID string `json:"from_bot_id"`
    ToBotID   string `json:"to_bot_id"`
    Reason    string `json:"reason"`
    HopDepth  int    `json:"hop_depth"`
}

type AppError struct {
    Code    string // PATH_OUT_OF_WORKSPACE / BOOTSTRAP_NOT_READY / ...
    Kind    string // user_error | tool_error | system_error
    Message string // 英文
    Detail  string // 脱敏补充
}
```

Flutter 客户端 `client/lib/api/error_messages.dart` 做 `Code → 中文文案` 映射，未收录回退到 `Kind` 文案。

---

## 6. Provider 适配层

### 6.1 内部契约

```go
type Provider interface {
    Stream(ctx context.Context, req Request, onEvent func(Event) error) (Usage, error)
}

type Request struct {
    Model      ModelSpec   // model_key, max_output, temperature, thinking budget
    System     string
    Messages   []Message
    Tools      []ToolSchema
    ToolChoice string      // auto | required | none
}

type Event struct {
    Kind       string // text_delta | thinking_delta | tool_use_start | tool_use_delta | tool_use_end | stop
    Text       string
    ToolCallID string
    ToolName   string
    InputDelta string
    StopReason string
}

type Usage struct { InputTokens, OutputTokens, CacheReadTokens int; Estimated bool }
```

### 6.2 Anthropic

- Messages API 流式；`system` 独立字段（便于缓存）；工具结果作为 `user` 下的 `tool_result` 块。
- 提示缓存：系统提示与稳定前缀打 `cache_control: ephemeral`；命中量写 `usage.cache_read_tokens`。
- 错误映射：`429/529 → PROVIDER_RATE_LIMITED`（读 `Retry-After`）、`400 → PROVIDER_BAD_REQUEST`（不重试）、网络 → `PROVIDER_NETWORK`。
- 重试：仅对 `429/5xx/网络`，指数退避 1/2/4s，上限 3 次。

### 6.3 OpenAI 兼容

- `/v1/chat/completions` 流式，`stream_options.include_usage=true`；缺失 usage 时按字符估算并标记 `Estimated`。
- `tool_calls` 按 `index` 累积参数分片；`reasoning_content` 存在则转 `thinking_delta`。

### 6.4 模型配置（全部由界面完成）

- 初始化**不要求**任何模型；未配置模型时，Bot 可创建但发送消息接口返回 `MODEL_NOT_CONFIGURED`（`user_error`，界面引导去添加供应商）。
- 供应商：`kind`（anthropic / openai_compat）+ `base_url` + 可选密钥；密钥以 `secret_id` 引用，不落明文。
- 模型：填 `model_key` 与价格（可留空则成本按 0 计并标注 estimated）；`GET /models/discover` 尝试拉取供应商模型列表（实现为可选增强，失败不影响手动添加）。
- 默认模型：`models.is_default`，新 Bot 创建时预填。
- 默认助手：迁移 `0017_default_tom.sql` 仅写入 `system_state` 的一次性 `default_bot.pending` JSON 种子；认证提交后，服务在同一事务内先准备 `bots/tom` 工作目录，再创建 Bot `tom`、会话 `tom-direct` 并删除种子。失败保留种子，后续启动或登录重试，不覆盖已有文件；不改变丢失认证文件时拒绝已有业务数据的保护。创建时绑定已有默认模型，保留同名用户 Bot，必要时采用带序号名称；后续设置默认模型的同一事务仅绑定尚未配置模型的 Tom。Tom 保留普通 Bot 的编辑、归档、删除语义，种子消费后不再创建。升级前停机备份，回退须恢复迁移前数据库、认证文件、工作区及匹配版本。完整事务与文件恢复约定见[默认助手 Tom](default-assistant.md)。

---

## 7. Agent 运行时

### 7.1 调度（`runtime.go`）

```go
type Runtime struct {
    globalSem chan struct{}          // TOM_MAX_CONCURRENT_RUNS
    botLocks  sync.Map               // bot_id -> *sync.Mutex
    queues    map[string][]*QueueItem
    bus       *bus.Bus
}
```

- 入队：写 `runs(status=queued)` → 取 Bot 锁 + 全局信号量 → 拿不到则排队并推 `run.queued`（含队列位置）。
- 取消：`POST /turns/{id}/cancel` 取消该 Turn 下所有 Run；循环在 step 边界与工具执行中检查 `ctx.Done()`。
- 启动恢复：`running/queued/waiting_approval` → `interrupted`；`tool_calls.running` → `canceled`；`approvals.pending` → `expired`。

### 7.2 循环（`loop.go`）

```go
for step := 0; step < cfg.MaxSteps; step++ {   // 默认 60
    if err := checkLimits(ctx, run); err != nil { return gracefulStop(err) }
    var toolUses []ToolUseBlock
    usage, err := provider.Stream(ctx, req, func(ev Event) error { ... })
    recordUsage(usage)
    if len(toolUses) == 0 { return nil }
    results := executeTools(ctx, run, toolUses)   // 有序；只读类可并发
    history = append(history, assistantBlocks(toolUses), toolResultBlocks(results))
}
```

- 流式增量每 500ms 或 4KB 合并落库，同时推事件。
- 每个 `tool_use` 必有且仅有一个 `tool_result`；被拒绝的以 `is_error=true` + `TOOL_DENIED` 回灌，让模型能换路。
- 达上限时写 `note` 块说明原因，状态 `failed` + `RUN_BUDGET_EXCEEDED` / `RUN_MAX_STEPS` / `RUN_TIMEOUT`，界面显示"已停止：达到上限"。

### 7.3 上下文装配（`context.go`）

系统提示（全部英文，因面向模型）：

```
[1] Identity & role        Bot 的 name / role_title / soul
[2] The computer           workspace 布局、共享区约定、可用命令、当前时间与时区
[3] Tools guidance         何时用 shell / browser / terminal；输出精简要求
[4] Memory index           ≤2KB：标题 + 一行摘要 + 路径 + “use memory_search to expand”
[5] Colleagues             仅在协作工具启用时注入：其他 Bot 的名字 + 一句话职责
[6] Group context          群组会话：群用途、成员与本群分工备注、本回合你的角色（coordinator/member）
[7] Turn instruction       本回合要做什么（user 消息 / 协调者分配的 brief / 其他 Bot 的消息）
```

历史窗口按预算从最新往前填；开始丢弃时异步更新 `conversations.summary` 并记 `summary_upto_seq`。

群组会话的历史包含所有成员发言（带发送者标注），这是群组协作的前提；`bot_dm` 会话只含双方往来。

### 7.4 单次执行的上限

| 限制 | 默认 | 触发后 |
| --- | --- | --- |
| Provider 单 step 超时 | 180s | 重试 ≤3，仍失败则 Run 失败 |
| Run 墙钟 | 30min | 取消当前工具，优雅收尾 |
| 工具超时 | shell 120s / browser 30s / 其余 30s | 杀进程组，回灌 `TOOL_TIMEOUT` |
| Run 成本 | $2 | 优雅收尾 |
| 步数 | 60 | 优雅收尾 |
| 群组回合成本 | 配置项，默认 $6 | 停止后续成员发言，协调者做收口 |

---

## 8. 群组编排与 Bot 间消息

### 8.1 数据与入口

- 群组 = `groups` + `group_members`；群组绑定唯一 `conversations(kind='group')`。
- 创建 Bot 时在同一事务内创建其 `direct` 会话（标题为 Bot 名），删除 Bot 时级联删除；这样左栏只依赖 `conversations` 一张表，不会出现"新建 Bot 不在列表里"。
- 用户往群会话发消息 → 创建 `turn` → `orchestrator.Handle(turn)`。

### 8.2 路由（`router.go`）

```go
func (o *Orchestrator) Handle(ctx context.Context, t *domain.Turn) error {
    switch g.Routing {
    case "mention":    plan = planFromMentions(msg, members)      // 有新 @ 则只派给被 @ 者；无 @ 则仅派给协调者；无协调者则不派 Run，回一条系统消息请用户 @
    case "coordinator": plan = o.planWithCoordinator(ctx, t)       // 模型产出结构化分工
    case "all":        plan = planAll(members, g.MaxSpeakers)
    }
    plan = guard.Clamp(plan, g.MaxSpeakers, budget)                // 数量与预算护栏
    runs := o.executePlan(ctx, t, plan)                            // 并行，各自 Bot 锁
    if g.HasCoordinator && shouldSummarize(plan) { o.summarize(ctx, t, runs) }
    return nil
}
```

### 8.3 规划调用（协调者）

- 独立系统提示（精简、结构化输出），不与协调者人格混用：

```
You are the router for a group chat. Given the group purpose, member roles and the user's
latest message, decide who should act. Return JSON only:
{"steps":[{"bot_id":"...","brief":"..."}],"reason":"..."}
Rules: at most N steps; do not assign the same sub-task twice; prefer members whose role
matches; if no member is needed, return an empty steps array.
```

- 上下文只带：群用途、成员名字与职责、最近 20 条消息摘要、@提及。
- 解析失败重试一次；仍失败 → 退化为 `plan = [{coordinator, brief=用户消息}]`，并把 `PLAN_FALLBACK` 写进 `turns.plan_json` 与界面提示。
- 规划调用本身记 usage，归属该 Turn、`plan_role='coordinator'`。

### 8.4 成员执行

- 每个成员一个 Run，`source='group'`、`plan_role='member'`；上下文含群转录（截至本回合开始）+ 自己的 brief。
- 成员之间**并行**执行；写同一文件由文件锁串行；界面提示并发写冲突。
- 成员发言按完成顺序追加到会话；每条消息带 `sender_bot_id` 与其 `accent_hue`，界面据此着色。
- 成员可在发言中调用 `post_to_group` 追加消息（不产生新 Turn），用于补充产物路径等。

### 8.5 汇总

- 协调者在所有成员完成后做一次汇总（可配置关闭）：输入为各成员回复原文，输出一段面向用户的结论，标记为 `plan_role='coordinator'` 的 Run。
- 汇总失败不影响成员结果，界面显示"未能汇总，以下为各方原话"。

### 8.6 护栏（`guard.go`）

| 护栏 | 规则 | 结果 |
| --- | --- | --- |
| 发言数 | ≤ `groups.max_speakers`（默认 3） | 超出截断，plan 记录 `clamped` |
| 重复分工 | 同一 `(bot, brief)` 相似度 > 0.9 视为重复 | 丢弃后者 |
| 空转 | 连续两条成员回复均无工具调用且 < 40 字 | 提前收口 |
| 成本 | 回合累计 > `groups.max_cost_micro` | 停止新增发言，协调者收口 |
| 环路 | 群内 Bot 通过 `message_bot` 又回到同群 | 拒绝并提示 |

### 8.7 `message_bot` 工具

```json
{ "bot_id": "b_xxx", "message": "string", "reply_required": true, "context_paths": ["shared/brief.md"] }
```

- 校验：目标存在且未归档、`messaging_policy.accept_dm` 允许、`hop_depth + 1 ≤ 4`、非环路。
- 创建或复用 `bot_dm` 会话（同一对 Bot 复用一个会话，按最近活跃排序），创建 `turn(source='bot_message')` 与目标 Bot 的 Run。
- `reply_required=true` 时等待目标 Run 结束（受调用方剩余墙钟与预算约束），把回复文本 + 产物路径作为工具结果返回；`false` 时只投递并返回 `delivered`。
- 无回复（目标 Run 失败/超时）返回 `is_error=true` + `BOT_NO_REPLY`，由调用方决定换路。
- 乒乓检测：同一对 Bot 在 5 分钟窗口内互发 > 6 条 → 拒绝并创建一条系统消息告知用户。

### 8.8 `delegate_task` 与 `message_bot` 的分工

- `delegate_task`：为"交付一件事"设计，结果被结构化（summary + artifacts），有独立的子会话（`conversations.kind='bot_dm'` 且 `source='delegation'`），适合任务型交接。
- `message_bot`：为"问一句话/通个气"设计，轻量，适合协作中的即时澄清。
- 两者共用 `runs.hop_depth` 与预算继承逻辑。

### 8.9 私信与群组在界面上的呈现

- `bot_dm` 会话在左栏的"私信"折叠区分组列出，带双方色块；可只读旁观，也可介入（发出的消息视为用户对该会话的发言）。有未读时该分组自动展开并显示计数。
- 群组会话在左侧列表中与 direct 会话同级，图标为成员色带。

---

## 9. 工具实现要点

### 9.1 注册表

```go
type Tool interface {
    Name() string
    Description() string              // 英文
    Schema() json.RawMessage
    Meta() ToolMeta                   // ReadOnly, ActionClass, MaxOutputBytes
    Execute(ctx context.Context, env *Env, input json.RawMessage) (Result, error)
}
```

`Env` 提供 `Bot`、`Run`、`Turn`、`Workspace`、`Secrets`（名字与注入）、`Browser`、`Term`、`Memory`、`Collab`、`Bus`、`Approvals`、`Clock`。

### 9.2 文件工具

| 工具 | 关键约束 |
| --- | --- |
| `file_read` | 文本、带行号、默认 2000 行；二进制返回 `NOT_TEXT_FILE` |
| `file_write` | 全量覆盖；`create_dirs` 不存在时 `PARENT_MISSING`；写前记录旧 hash 到审计 |
| `file_edit` | 精确串替换；非唯一返回 `EDIT_NOT_UNIQUE`，未找到返回 `EDIT_NOT_FOUND` |
| `file_list` | 忽略 `.git`、`node_modules`、`__pycache__`；默认深度 2 |
| `file_glob` | `filepath.Match` 递归实现，按 mtime 排序 |
| `file_search` | 纯 Go `regexp` + `WalkDir`，跳过二进制与大目录；默认 100 条；**不依赖 rg** |

路径校验：

```go
abs := filepath.Join(env.BotWorkspace, p)
abs, _ = filepath.Abs(abs)
abs, _ = filepath.EvalSymlinks(abs)               // 存在时；不存在则校验父目录
rel, _ := filepath.Rel(env.WorkspaceRoot, abs)
if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
    return ErrPathOutOfWorkspace
}
```

### 9.3 shell 与终端

`computer_shell`：`{command, cwd?, timeout_seconds, env_secrets[]}`

- `sh -lc`（有 bash 用 bash），`Setpgid`；超时 `SIGTERM` 进程组 → 5s → `SIGKILL`。
- 环境变量白名单 + 注入 `env_secrets`（值不进日志与结果，结果中以 `***` 呈现）。
- stdout/stderr 合并，32KB 截断，超出部分写 `.tom/tmp/out-<run>-<step>.log` 并返回路径。
- 返回 `{exit_code, stdout_tail, truncated, output_path?}`。
- 危险模式命中（`rm -rf /`、`mkfs`、`dd of=/dev/`、`sudo`、`shutdown`、写系统目录）提升为 `shell.privileged`，默认 deny。

`terminal_session`：`open/write/read/resize/close`

- `term/pty.go` 管理会话，命名 `tom-bot-<botid8>-<seq>`；`read` 返回自上次读取的新增输出（游标在内存 + 元数据）。
- Run 结束保留 30 分钟，定时清理回收。
- 服务重启不恢复（界面明示），避免误导。

### 9.4 浏览器

- `browser{action:"snapshot"}` 输出格式：

```
URL: https://example.com/orders
Title: Orders
---
[ref=e3] link "Orders"
[ref=e9] button "导出"
[ref=e12] textbox "搜索订单"
[ref=e15] text "共 128 条"
```

  构造：`Accessibility.getFullAXTree` → 丢弃 `ignored` 与无语义容器 → 保留交互角色（button/link/textbox/checkbox/radio/combobox/menuitem/tab）与可见文本 → 合并相邻文本 → 上限 300 行。
- `ref → backendNodeId` 存于 `browser.Tabs`（随快照覆盖）；`click` 走 `scrollIntoView` + 真实鼠标事件，失败回退 focus + Enter。
- `type` 先清空（`clear` 默认 true）再 `Input.insertText`；`key` 支持 Enter/Tab/Escape/Arrow*。
- 新标签自动纳入当前 Bot 组（监听 `Target.targetCreated`）。
- 元素不存在 → 提示模型重新 `snapshot`；导航超时 30s → `BROWSER_NAV_TIMEOUT`。

### 9.5 记忆工具

- `memory_write{title, summary, content, tags[], importance}` → 写 `bots/<id>/.tom/memory/<slug>.md`，同步 `memory_index` 与两张 FTS（同一事务）。
- `memory_search{query, limit=5}`：含 CJK 走 trigram，否则 porter；排序 `bm25 × (1 + 0.2·importance) × 30 天半衰`；返回 `{path,title,summary,snippet}`。
- 用户直接编辑文件时，启动扫描与写入钩子重算 `content_hash`；每 10 分钟一致性校验。
- 群组转录不写记忆（由 Bot 主动 `memory_write` 决定）。

### 9.6 检索与提问

- `conversation_search{query, bot_id?, kind?, limit=10}`：对 `messages` 建 FTS（`0002_message_fts.sql`，`messages_fts` + 触发器同步），返回会话标题、命中片段、时间与消息 id；只检索该 Bot 参与过的会话（`direct` 里自己的、`group` 里在群的、`bot_dm` 里自己参与的）。
- `conversation_read{conversation_id, around_message_id?, limit=30}`：读上下文；越权（不参与的会话）返回 `CONVERSATION_FORBIDDEN`。
- `ask_user{question, options[]}`：创建交互（`approvals.action_class='user.question'`，问题与选项存 `detail_json`），Turn 进入 `waiting_approval`；用户的选择或自由文本通过 `POST /approvals/{id}/decision` 的 `note` 回传，作为工具结果（`{choice, text}`）返回；超时（默认 24h）返回 `USER_NO_RESPONSE`。群组中被多个成员同时提问时排队展示，一次回答可作用于同 Turn 内所有等待项。

### 9.7 截断

`truncate.go`：超限时完整内容落临时文件，返回 `{summary, truncated:true, output_path}`；摘要由确定性规则生成（首尾各 N 行 + 命中行），不调模型。

---

## 10. 审批与策略

### 10.1 动作类

```
fs.delete | fs.write_outside_out | shell.exec | shell.privileged
net.new_domain | browser.login | secret.use
delegate.spawn | message.bot | group.invite | routine.create | user.question
```

### 10.2 决策流程

```
工具在白名单?  否 → TOOL_NOT_ALLOWED
→ 推导 actionClass（含命令危险模式、域名是否首次、路径是否在 out/）
→ 匹配 policy_rules（bot 精确 > '*'，pattern 精确 > 无）
→ allow → 执行｜deny → TOOL_DENIED（附原因）
→ 否则按 bot.tool_policy.approval：
     loose    仅 high 询问
     standard medium/high 询问
     strict   全部询问
→ 可选独立审查：提升风险等级并附 note（不改变方向）
→ 创建 approval、推事件、Run → waiting_approval
→ 批准（可带"以后此类允许"→ 写 policy_rules）或拒绝
```

审批超时 30 分钟 → `expired`，工具结果 `APPROVAL_TIMEOUT`，Run 继续并在上下文中告知未获批。

### 10.3 审查

`reviewer.go` 输入用户意图、工具名、参数、目标；输出 `{consistent, risk, reason}`；审查模型单独配置（默认与执行模型不同）；只配了一个模型时，审查退回使用该模型并在 `approvals.reviewer_note` 中标注"由执行模型自查"，界面显示为弱化提示而非独立审查。审查失败按保守策略提升一档风险，不自动放行。

---

## 11. 记忆、调度、密钥、用量

### 11.1 记忆文件

```markdown
---
title: 信源偏好
tags: [research, sources]
importance: 4
updated_at: 2026-11-05T10:00:00Z
---

优先官方文档与一手数据；社交媒体来源需交叉验证。
```

解析器只识别 `title/tags/importance`，其余原样保留；缺失 front matter 用文件名与首行兜底；单文件 > 200 行或 > 64KB 时检索结果标注 `too_long`。

### 11.2 调度

- `robfig/cron/v3` 解析，统一换算到目标时区的 `next_run_at`。
- 每分钟 tick；用 `UPDATE ... WHERE id=? AND next_run_at=?` 条件更新抢占，避免重复触发。
- 触发：`bot_id` → 直接会话；`group_id` → 群组路由；`target_mode=pinned` 写入指定会话，`new` 则每触发新建。
- `skip` 策略下该目标仍有活跃 Turn 则记 `skipped_overlap`。
- 启动时若 `catch_up=once` 且错过 ≥1 次，补跑一次。
- 创建/修改例程需审批（`routine.create`）。

### 11.3 密钥

- AES-256-GCM；主密钥来自 `TOM_MASTER_KEY`（base64）或派生自 `data/master.key`（0600，首次生成）。
- 备份口径：`tomd backup` 归档 `data/` 与 `workspace/`。主密钥来自文件时**随备份一起打包**，归档说明中必须写明"此归档含解密密钥，需自行加密保存"；主密钥来自环境变量时不打包，还原时需提供同一个 `TOM_MASTER_KEY`，否则密钥无法解密（还原命令显式校验并报错）。
- 解密只在执行期内存发生，用完清零；接口只返回 `name` 与 `hint`（末 4 位）。
- 注入审计记录 `secret.use`（name + run_id），不记录值。

### 11.4 用量

- 价格单位固定为「微美元 / 百万 token」：`cost_micro = in×price_in/1e6 + out×price_out/1e6 + cache×price_cache/1e6`（整数运算，四舍五入到微美元）。
- 每次调用写 `usage_events`，累加同上。
- 累加到 `runs.cost_micro` 与 `turns.cost_micro`（事务内累加）。
- 每 step 前检查 Run 预算；每 Turn 前检查 Turn 预算（群组）；每月检查月度预算（`stop` 时拒绝新 Turn）。

---

## 12. HTTP 接口契约

### 12.1 通用

- 前缀 `/api/v1`；JSON；RFC3339 时间。
- **认证按客户端类型分流**：
  - 桌面/移动客户端：`Authorization: Bearer <device_token>`（长期令牌，存系统安全存储）。
  - 浏览器访问 `/health`：无需认证（只读运行状态）；若设置 `TOM_HEALTH_TOKEN`，则需 `?t=` 或 Basic。
  - 不提供浏览器 cookie 会话——服务端没有业务页面，无需 CSRF 机制。
- 设备令牌：`tomd_` + 32 字节随机（base64url），服务端只存 `sha256(token)` 与前后缀提示；可随时撤销。
- 错误：

```json
{ "error": { "code": "PATH_OUT_OF_WORKSPACE", "kind": "user_error", "message": "...", "request_id": "..." } }
```

### 12.2 引导与认证

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/health` | **唯一 HTML 页面**：服务状态、能力清单、CPU/内存/磁盘、版本、运行时长 |
| `GET` | `/api/v1/health` | 同上，JSON，供监控 |
| `GET` | `/bootstrap/status` | 阶段、步骤、错误 |
| `GET` | `/bootstrap/events` | SSE 进度 |
| `POST` | `/bootstrap/retry` | 重跑失败步骤 |
| `GET` | `/auth/status` | `{initialized, authenticated, ready}` |
| `POST` | `/auth/setup` | **仅 `{password}`**；`initialized=false` 时可用一次 |
| `POST` | `/auth/login` | `{password, device:{name, platform, push_token?}}` → `{device_token, device_id}`；5 次失败后指数退避 |
| `GET` | `/devices` | 已授权设备列表（名称、平台、最后活跃、当前设备标记） |
| `DELETE` | `/devices/{id}` | 撤销（当前设备撤销即登出） |
| `POST` | `/devices/push-token` | 更新推送令牌 |

口令 `argon2id`（m=64MB,t=3,p=2）存 `data/auth.json`。**初始化不接受任何模型/供应商/密钥参数**；额外键一律忽略并记 WARN 日志（便于旧客户端迁移时不误导）。

### 12.3 事件流

`GET /events?conversation_id=&after=<cursor>`

- 全局事件（无 `conversation_id`）只推摘要类：审批、Bot/群组状态、产物、`conversation.updated`。左栏只靠这一条流维持新鲜度。
- `id` 为全局单调游标，服务端保留最近 2000 条；`after` 超界返回 `event: resync`，客户端重拉消息。
- 心跳 15s（`: ping`）。

| event | data |
| --- | --- |
| `bootstrap.step` | `{id, title, status, detail}` |
| `turn.started` | `{turn_id, conversation_id, plan?}` |
| `turn.plan` | `{turn_id, steps:[{bot_id, brief}], reason}` |
| `run.started` | `{run_id, turn_id, bot_id, plan_role}` |
| `run.status` | `{run_id, status, queue_position?}` |
| `message.started` | `{run_id, message_id, sender_bot_id}` |
| `text.delta` / `thinking.delta` | `{run_id, message_id, index, text}` |
| `message.completed` | `{run_id, message_id, usage}` |
| `tool.started` / `tool.delta` / `tool.output` | `{run_id, tool_call_id, name, input_preview?, chunk?, status?, output_preview?}` |
| `approval.requested` / `approval.resolved` | `{approval}` / `{approval_id, status}` |
| `handoff.created` | `{turn_id, from_bot_id, to_bot_id, kind}` |
| `artifact.created` | `{run_id, path, kind}` |
| `turn.finished` | `{turn_id, status, error_code?, cost_micro}` |
| `conversation.updated` | `{conversation_id, last_message_preview, last_message_at, unread_count, bot_status}` —— 左栏据此就地更新，不重拉列表 |
| `system.notice` | `{level, text}` |

### 12.4 Bot / 群组 / 会话

```
GET    /bots                      → Bot[]（含 status: idle|running|waiting_approval）
POST   /bots                      {name, role_title, soul, model_id?, avatar?, template?}
GET    /bots/{id}
PATCH  /bots/{id}
DELETE /bots/{id}?purge_workspace=&confirm_name=
POST   /bots/{id}/avatar          multipart

GET    /groups
POST   /groups                    {name, purpose, routing, coordinator_bot_id?, members:[botId], max_speakers?}
GET    /groups/{id}               → 群组 + 成员 + 关联会话 id
PATCH  /groups/{id}
DELETE /groups/{id}
POST   /groups/{id}/members       {bot_ids:[...]}
DELETE /groups/{id}/members/{botId}

GET    /conversations?kind=&cursor=&limit=20
       → 左栏一次性取全量分组数据，含 {kind, bot_id, group_id, title,
         last_message_preview, last_message_at, unread_count, bot_status, accent_hue}
       # bot_status 由运行时注册表（内存）与 conversations 合并得出，不单独查 bots 表；
       # 群组行取成员状态的最高优先级：waiting_approval > running > idle
POST   /conversations             {kind:"direct", bot_id}      # 已存在则直接返回同一个
GET    /conversations/{id}        → 会话 + 最近 50 条消息
PATCH  /conversations/{id}        {title?, is_archived?}
POST   /conversations/{id}/read   {seq}                        # 更新 last_read_seq
GET    /conversations/{id}/messages?before_seq=&limit=50

POST   /conversations/{id}/messages
       {content, attachments?[], mentions?[botId], reply_mode?:"single"|"group"}
       → {turn_id, message_id}
POST   /turns/{id}/cancel         → 202
GET    /conversations/{id}/turns?cursor=
GET    /runs/{id}                 → Run + tool_calls
```

约束：会话已有活跃 Turn 时返回 `409 TURN_IN_PROGRESS`；`mentions` 仅在 `kind=group` 时有效。

### 12.5 审批 / 例程 / 记忆

```
GET    /approvals?status=pending
POST   /approvals/{id}/decision   {decision:"approve"|"deny", remember?:{scope:"bot"|"global"}, note?}
GET/POST/DELETE /policy-rules

GET    /routines?bot_id=&group_id=
POST   /routines                  {name, cron, timezone, prompt, target:{bot_id|group_id}, overlap_policy, catch_up, target_mode}
PATCH/DELETE /routines/{id}
POST   /routines/{id}/run-now
GET    /routines/{id}/runs

GET    /bots/{id}/memory          → [{path,title,summary,importance,updated_at}]
POST   /bots/{id}/memory          {title, summary, content, tags, importance}
GET/PATCH/DELETE /memory/{botId}?path=
```

### 12.6 文件 / 终端 / 浏览器 / 配置 / 系统

```
GET    /fs/tree?path=&depth=2&bot_id=&scope=bot|shared   # bot_id 缺省时以 shared 为根
GET    /fs/file?path=            (≤1MB，超出 413)
PUT    /fs/file                  {path, content}
GET    /fs/download?path=
POST   /fs/upload                multipart → 落到当前 Bot 的 inbox/

GET    /terminals                → 含归属（user | bot:<id>）
POST   /terminals                {cwd?, title?}
DELETE /terminals/{id}

GET    /browser/tabs
DELETE /browser/tabs/{id}
GET    /browser/screenshot?bot_id=

GET/POST /providers  PATCH/DELETE /providers/{id}   POST /providers/{id}/test
GET/POST /models     PATCH/DELETE /models/{id}      GET  /models/discover?provider_id=
GET/POST /secrets    POST /secrets/{id}/rotate      DELETE /secrets/{id}
GET/PATCH /settings
GET    /system/status            {version, phase, capabilities, runs, queue, browser, disk, db_size}
GET    /system/metrics           {cpu_pct, mem_used, mem_total, disk_used, disk_total,
                                  load1, uptime_s, db_size, runs_running, queue_depth}
GET    /audit?cursor=&limit=
GET    /usage?from=&to=&group_by=bot|model|day
POST   /system/rebuild-memory-index
```

`POST /providers/{id}/test` 用该供应商发起一次最小请求（`max_tokens=1`）验证连通与鉴权，返回耗时与错误码——这是"配置后立刻知道自己配对了"的关键交互。

### 12.7 WebSocket

终端：

```json
{"type":"input","data":"ls\r"}
{"type":"resize","cols":120,"rows":32}
{"type":"output","data":"..."}      // server → client
{"type":"exit","code":0}
```

浏览器画面：

```json
{"type":"start","quality":60,"max_fps":5,"tab_id":"..."}
{"type":"click","x":120,"y":340}
{"type":"input","text":"..."}
{"type":"frame","data":"<base64 jpeg>","meta":{"w":1280,"h":720}}
{"type":"tab_changed","tab_id":"..."}
```

---

## 13. 客户端实现（Flutter，一套代码全平台）

### 13.1 设计令牌（`client/lib/design/tokens.dart`）

Flutter 用 `ThemeExtension` 承载令牌，深浅两套主题各一份，禁止在 Widget 中写死颜色与尺寸。

```dart
@immutable
class TomTokens extends ThemeExtension<TomTokens> {
  // 尺度（逻辑像素，4px 基准 / 8px 网格）
  static const s1 = 4.0, s2 = 8.0, s3 = 12.0, s4 = 16.0, s5 = 24.0, s6 = 32.0, s7 = 48.0;
  static const radiusSm = 10.0, radiusMd = 14.0, radiusLg = 20.0;
  static const measure = 680.0;        // 会话正文最大宽度；会话列自适应，正文封顶
  static const sidebarMinWidth = 240.0; // 左栏展开下限
  static const sidebarWidth = 300.0; // 左栏默认展开
  static const sidebarMaxWidth = 400.0; // 左栏拖拽上限
  static const collapsedSidebarWidth = 100.0; // 收缩后仅显示头像与操作图标
  static const panelWidth = 400.0;     // 右侧详情栏
  static const panelCollapsed = 48.0;  // 收起后的图标竖条

  // 排版
  final TextStyle body;                // 主文字 14，正文行高 1.65
  final TextStyle bodySm;              // 辅助文字 13
  final TextStyle caption;             // 说明文字 13
  final TextStyle titleLg;             // 22
  final TextStyle title;               // 18
  final TextStyle display;             // 28
  final TextStyle mono;                // 13，tabular figures

  // 颜色
  final Color bg, surface, surface2, line, lineStrong;
  final Color sidebarBackground, sidebarActiveBackground, messageBubbleBackground;
  final Color text, text2, text3;
  final Color brand, brandWeak, ok, warn, danger, codeBg;

  // 动效
  static const durFast = Duration(milliseconds: 160);
  static const durBase = Duration(milliseconds: 220);
  static const ease = Cubic(0.2, 0.0, 0.0, 1.0);
}

// 墨（默认深色）
const ink = TomTokens(
  bg: Color(0xFF121212), surface: Color(0xFF131518), surface2: Color(0xFF1A1D21),
  sidebarBackground: Color(0xFF0E0E0E), sidebarActiveBackground: Color(0xFF202020),
  messageBubbleBackground: Color(0xFF1E1E1E),
  line: Color(0x14FFFFFF), lineStrong: Color(0x24FFFFFF),
  text: Color(0xFFE8E6E1), text2: Color(0xFFA8A69F), text3: Color(0xFF6E6C67),
  brand: Color(0xFFE0705B), brandWeak: Color(0x24E0705B),
  ok: Color(0xFF6FB98A), warn: Color(0xFFD8A24C), danger: Color(0xFFDD6A5F),
  codeBg: Color(0xFF101215),
);

// 纸（浅色）
const paper = TomTokens(
  bg: Color(0xFFFCFCFC), surface: Color(0xFFFFFFFF), surface2: Color(0xFFF4F2ED),
  sidebarBackground: Color(0xFFF5F5F5), sidebarActiveBackground: Color(0xFFE4E4E4),
  messageBubbleBackground: Color(0xFFEFEFEF),
  line: Color(0x14121417), lineStrong: Color(0x28121417),
  text: Color(0xFF17181A), text2: Color(0xFF5B5D62), text3: Color(0xFF8E9095),
  brand: Color(0xFFC4553B), brandWeak: Color(0x1AC4553B),
  ok: Color(0xFF3E8E5C), warn: Color(0xFFA8761F), danger: Color(0xFFB5483C),
  codeBg: Color(0xFFF6F4EF),
);
```

Bot 身份色：由 `bots.accent_hue` 生成 `HSLColor.fromAHSL(1, hue, 0.55, 0.62)`（深色）/ `(hue, 0.48, 0.40)`（浅色），用于头像、消息发送者名称、群组成员色带，不做大面积填充。

字体：内置一份 UI 字体（Inter 变体 + 中文回退依赖系统 PingFang / Noto Sans CJK）；等宽用 `JetBrains Mono` 子集随包内置，保证数字与代码在两端一致。字体文件随客户端分发，不依赖服务器安装。

### 13.2 排版与布局规则

- 主文字 14 逻辑像素、辅助文字 13，标题保留字号与字重层级；正文行高 1.65，正文 `measure=680`，会话列随窗口自适应但正文封顶；助手消息使用圆角气泡占满正文宽；用户气泡右对齐且 ≤82% 宽，双方使用同一气泡背景色。
- 布局骨架与 Grok 一致：**左栏列表 + 中间会话 + 右侧可收起详情栏**。
- Bot 会话顶部只悬浮头像和名称，点击打开详情。输入框覆盖在消息流底部，无全宽背景面板，外框与 Bot 气泡左沿、用户气泡右沿对齐。布局同帧测量上下浮层，为消息首尾增加相应可滚动留白；可滚动视口延伸到浮层后方，最后一条消息可完整滚出输入框遮挡。
- 桌面首次启动且没有窗口记录时默认 1280×800 逻辑像素；已有记录正常恢复保存的尺寸与位置，升级不覆盖。默认与恢复窗口均按当前显示器可用区域收敛；窗口记录仍按设备保存，侧栏及会话布局仍按服务器隔离。
- 断点三档：`<720`（单列 + 底部导航，详情栏为全屏页 `/bots/:id/detail`）、`720–1199`（240–400 可调侧栏 + 会话列 + 覆盖式详情抽屉 360）、`≥1200`（240–400 可调侧栏 + 会话列 + 400 详情栏，收起为 48 竖条）。左侧栏默认宽度 300、展开最小宽度 240，最大 400，可收缩到 100（红绿灯左右各留 20 像素），仅通过拖拽调整；计算宽度低于 170 时吸附到 100，否则限制在 240–400，拖动结束后才保存，取消不保存。
- 侧栏按服务器保存到独立的 `workbench.sidebar.v3.*`，schema 版本 3，只接受 100 或 240–400；不读取、不覆盖旧 v1/v2 记录，不保留旧宽度解析或渲染路径。本次宽度规则升级将侧栏偏好一次性重置为默认 300（包括旧收缩和展开宽度），之后恢复合法 v3 记录。会话与详情面板沿用 `workbench.v1.*`。写入串行执行，失败显示重试；无需数据库或服务接口迁移，回滚旧客户端仍读取其原版本侧栏记录。
- 标题只用字号与字重分层，不加色块；分隔一律 1px `line`。侧栏背景与右侧内容区区分，边界分割线贯穿窗口顶部；非全屏 macOS 工作台仅覆盖左侧局部工具条，右侧内容不增加标题条空白。
- 代码块用 `codeBg`，行内代码不加背景，仅等宽 + 略浅色。
- 成本、token、耗时一律 `mono` + `FontFeature.tabularFigures()`。
- 触控：所有可点区域 ≥44×44；移动端次级操作收进长按菜单或滑出。

### 13.3 动效规则

| 场景 | 行为 |
| --- | --- |
| 消息进入 | `Offset(0, 8) → 0` + 透明度 0→1，`durBase`，仅新增项动画 |
| 流式光标 | 1s 呼吸的 2×18 竖条，颜色 `text2` |
| 工具卡片展开 | `AnimatedSize` + 箭头旋转 90°，`durBase` |
| 审批抽屉 / 移动 sheet | 桌面右侧滑入；移动自底上滑（`showModalBottomSheet` + 自定义曲线） |
| 面板折叠 | 宽度过渡 `durBase`，内容淡出 |
| 左侧栏跨折叠阈值 | 220ms 平滑宽度动画；取消拖拽或外部恢复同样过渡，展开区连续拖拽直接跟手 |
| 状态点 | 运行中同色缓慢脉冲（1.6s）；等待审批 `warn` 常亮 |

禁止装饰性心跳动画；系统开启"减少动态效果"（`MediaQuery.disableAnimations`）时左侧栏宽度立即切换，其余动效退化为透明度渐显。

### 13.4 页面、路由与组件

路由（`go_router`）：

```
/bootstrap            引导进度（步骤清单 + 失败修复建议）
/login                登录 / 首次设置密码（单字段）
/                     会话首页；首次无记录优先进入默认 Tom
/bots/:id             与 Bot 的会话（direct）；桌面端查询参数 ?panel=detail|computer 控制右栏
/bots/:id/detail      移动端详情全屏页（详情 / 电脑两个分区）
/groups/:id           群组会话（右栏"详情"显示群设置）
/dm/:id               Bot 间会话（旁观/介入）
/bots/:id/memory      记忆管理
/bots/:id/settings    身份、模型、工具与消息策略、审批规则
/bots/new             新增 Bot；由统一新增选择框打开
/groups/new           拉群（选成员、指定协调者、写群用途）
/routines             例程管理；Bot 详情中的“管理例程”附带 bot_id
/approvals            待办；聊天 /approvals、例程页或手机底部入口
/settings             分类设置模态框；可用 ?section=models|usage|secrets|devices|archived 定位分类
```

桌面端右栏不是路由，是 `/bots/:id` 的内部状态（`panel` 查询参数 + 本地记忆），因此开合不触发页面重建、不打断流式渲染。移动端才用独立页 `/bots/:id/detail`。

搜索与新增按钮并排显示，二者和底部设置按钮均使用圆形背景。`MacWindowLayout` 在非全屏 macOS 工作台让内容从窗口顶部开始，仅覆盖左侧高度 61、宽度 `max(侧栏实际宽度, 212)` 的局部工具条；普通页面保留原 48 像素标题条。搜索与新增按钮为 40×40，上边距为 13，与红绿灯顶部齐平；按钮组左边界取 `max(侧栏实际宽度 − 96, 116)`，随拖拽和宽度动画移动。右侧会话标题与操作顶部同为 13，收缩时为局部工具条保留横向安全区域，不与左侧按钮或拖拽区域重叠。其他平台或全屏时保留侧栏内按钮和原有标题布局，连接页返回与主题按钮位置不变。`showCreateConversationDialog` 只选择新增 Bot 或群组，不发送写请求，关闭模态框后 `push` 已有编辑页。关闭按钮、Escape、返回及遮罩均可取消，连续触发入口或选项不重复导航，背景聊天草稿保留。底部同一行显示主机与端口、设置按钮：地址不含协议，保留域名，IPv6 主机用方括号包围；收缩态用带完整主机与端口提示的图标代替文本。侧栏不再放置收起／展开按钮、例程、待办或资源快捷入口，文件、终端与浏览器统一从 Bot 的“电脑”进入。

移动端底部导航四项：会话、Bot、待办（审批与运行中）、设置；设置与桌面共用分类模态框，详情页只保留文件与浏览器截图，终端仅只读。

客户端使用 `Bot.defaultAssistantId` 与 `isDefaultAssistant` getter 识别默认助手，不新增 JSON 字段；`Conversation.isDefaultAssistant` 只识别其 `direct` 且非 `routine` 会话。侧栏和搜索共用 `findConversations`，先过滤标题与预览，再将默认助手置顶，其余分组保持服务端最近活动顺序；实时摘要更新保留身份字段。首次选择默认助手不能覆盖已验证的外部链接、已保存会话或用户返回的会话列表。无模型时提供配置入口，发送仍受原有模型校验约束。

关键 Widget：

| Widget | 要点 |
| --- | --- |
| `BotRail` | 左栏根组件：圆形背景的搜索、新增、设置按钮，Bot 行、群组行、私信折叠区，底部无协议的主机与端口；独立背景与贯穿标题栏的分割线区分内容区；数据来自 `GET /conversations`，实时事件刷新摘要，分组排序（Tom 常规直接会话 > 进行中 > 待审批 > 按最近活跃） |
| `BotRailRow` | 圆头像（上传图片优先；Tom 使用内置图片，其余使用 hue 底 + 首字）、名字、最新消息预览、状态点/未读点；收缩态保留头像与提示，选中态用 `surface2` 底色而非描边 |
| `ResizableSidebar` | 默认 300、展开 240–400、收缩 100，170 阈值吸附；跨阈值使用 220ms 动画，展开区跟手；仅拖拽调整，预览与动画不写入，结束时提交持久化，取消不写入 |
| `CreateConversationDialog` | 新增 Bot／群组的选择模态框，右上关闭，先关闭再导航，防止连点重复打开或导航 |
| `DetailPanel` | 可收起的右栏容器（400 / 48），详情与电脑两分区切换保持各自滚动位置与连接状态 |
| `BotDetailPane` | 身份、模型、工具与消息策略、审批严格度、记忆索引摘要、例程入口 |
| `FilesPane` | 位于电脑分区，支持 Bot 与共享文件范围；文件树按 mtime 倒序，点开转预览（Markdown / 代码高亮 / 图片），可在同一电脑内打开目录终端 |
| `ComputerPane` | 浏览器画面（`RawImage` 渲染 WS 帧，当前域名常显）、终端（`xterm.dart`）与文件页签，切换保留各页状态 |
| `MessageList` | `ListView.builder` + 自定义虚拟化；>200 条启用回收；粘底滚动策略（用户上滑后暂停自动滚动） |
| `MessageItem` | 依 `sender_kind`/`sender_bot_id` 取左侧标线色与头像；助手无气泡 |
| `MarkdownBlocks` | 见 13.5，自建块渲染 |
| `ToolCard` | 一行摘要（图标 + 工具名 + 参数摘要 + 状态 + 耗时），展开显示输入输出；`computer_shell` 展开为终端样式 |
| `ApprovalSheet` | 桌面右侧抽屉、移动 bottom sheet；风险徽章、动作描述、目标、审查意见；三键（仅此次 / 本 Bot 以后允许 / 拒绝） |
| `GroupRoster` | 成员色带；发言者高亮脉冲；协调者分工为缩进条目 |
| `PlanCard` | `turn.plan` 渲染为折叠卡片，列出"谁做什么"，可展开看 brief |
| `ComputerPanel` | 三页签；文件按 mtime 排序，浏览器为 `RawImage` 渲染 WS 帧，终端 `xterm.dart` |
| `Composer` | 自适应高度 `TextField`；`@` 唤起成员选择（仅群组）；附件选择；桌面 `⌘↵` 发送、`Esc` 中断；移动端发送键 + 长按录音位预留 |
| `EmptyState` | 一句说明 + 一个主行动 + 三个模板卡片 |

### 13.5 Markdown 与富内容渲染

`flutter_markdown` 无法满足工具卡片与产物块的混排，因此自建两级渲染：

1. **解析**：`markdown` 包解析为 AST，按块类型映射为 `Block` 模型（段落、标题、列表、引用、代码、表格、分隔线、内联 HTML 忽略）。
2. **渲染**：`BlockList` 用 `Column` 顺序渲染各 `BlockWidget`；代码块走 `flutter_highlight`（内置若干常用语言的 TextMate 语法），超出 40 行折叠；表格横向滚动；链接用 `url_launcher`。
3. **与消息块合并**：一条消息的 `blocks_json` 中 `text` 块交给 `BlockList`，`tool_use`/`tool_result`/`artifact`/`handoff` 交给对应卡片 Widget，按顺序交错插入——**不把工具卡片塞进 Markdown 里**。

流式更新策略：`text.delta` 累积到 `StreamingBuffer`，每 50ms 触发一次重建；`message.completed` 以服务端最终内容替换本地缓冲。为避免长文本反复布局，增量渲染只重建最后一个未闭合块。

### 13.6 状态与实时通道

- 状态用 `riverpod`：`botsProvider`、`conversationsProvider(filter)`、`conversationProvider(id)`、`messagesProvider(id)`（`AsyncNotifier`，支持增量 `upsert`）、`approvalsProvider`、`routinesProvider`、`groupsProvider`。
- SSE 客户端：`http.Client().send()` 流式读取，按 `event:`/`data:` 行解析；维护 `after` 游标；断线指数退避（1/2/4/8s，上限 30s），超 3 次失败切 15s 轮询并在界面标注"实时连接已降级"。
- 事件落到对应 Notifier：`text.delta` 追加缓冲；`tool.output` 更新工具卡片；`conversation.updated` 更新左栏行（不改排序顺序以外的任何状态）；`turn.finished` 触发拉取会话与用量。
- 未读由服务端计算（`seq > last_read_seq` 且 `sender_kind != 'user'` 的消息数），客户端不自行累加；进入会话即 `POST /conversations/{id}/read`。单用户部署下读写游标不按设备拆分——你在手机上读完，桌面端的未读同步归零。
- 中断：`Esc` 或按钮 → `POST /turns/{id}/cancel`，按钮立即进入"正在停止"，待 `turn.finished` 归来复位。

### 13.7 原生能力与平台差异

| 能力 | 桌面 | 移动 | 实现 |
| --- | --- | --- | --- |
| 安全存储 | Keychain / DPAPI / libsecret | Keystore / Keychain | `flutter_secure_storage` |
| 通知 | 本地通知 | 本地通知（系统推送为后续目标） | `flutter_local_notifications`；`push_token` 字段与 `POST /devices/push-token` 保留，接入推送后启用 |
| 深链 | `tom://bot/:id`、`tom://group/:id`、`tom://dm/:conversationId`、`tom://server?addr=` | Universal Link / App Link | `app_links` |
| 窗口 | 最小尺寸、记忆、分栏 | — | `window_manager`（桌面专用依赖，用条件导入隔离） |
| 文件选择 | 系统对话框 | 系统选择器 | `file_picker` |
| 截图保存 | 下载目录 | 相册 | `path_provider` + `gal` |
| 终端输入 | 完整 | 只读 | 能力探测后限制 |

平台差异只在 `PlatformCapabilities` 一个对象里判断，Widget 不直接读 `Platform.isXxx`。

### 13.8 与 Grok Bot 的差异化检查清单

三张 macOS 桌面基线与手机、平板两张布局存档均已逐项核对。[截图及复验方法](client-visual.md)

- [x] 底色不是纯黑，品牌色不是黑白灰；有明确的暖色主色与纸色浅色主题。
- [x] 助手消息不以气泡呈现，排版是主角；每屏信息密度高于卡片堆。
- [x] Bot 身份由色相 + 排版承担，不依赖大圆头像与渐变徽章。
- [x] 桌面为左栏列表 + 会话 + 可收起详情栏，而非顶部大卡片矩阵；右栏三分区共用一个面板；移动为单列 + 底部导航。
- [x] 群组视图有自己的信息表达（色带 + 分工条目），不是把多个单聊拼在一页。
- [x] 图标统一 1.5px 描边，圆角与间距基于 4/8 网格。
- [x] 动效只服务状态变化，无装饰性弹跳；系统开启减少动态时正确退化。
- [x] 同一功能在桌面与移动的视觉语言一致，仅信息密度不同。

---

## 14. 测试策略

| 层 | 范围 | 方式 |
| --- | --- | --- |
| provision | 步骤幂等、Verify 短路、包管理器命令映射、离线归档、失败即不就绪 | 假 `exec` 注入 + 临时目录；命令构造用表驱动 |
| 自检 | 中文渲染断言、FTS5、PTY | 真实运行（缺浏览器环境时 `t.Skip` 并可见） |
| policy | 决策矩阵（白名单、规则、严格度、危险模式、审批超时） | 表驱动 |
| orchestrator | mention/coordinator/all 三种路由、规划解析失败回退、发言上限、重复分工、成本截断、环路 | 假 Provider + 内存 store |
| 协作 | `message_bot` 跳数、环路、乒乓、无回复；`delegate_task` 深度与预算继承 | 假 Provider 脚本化 |
| agent 循环 | 无工具/单工具/多工具/拒绝回灌/取消/预算/步数 | 假 Provider 事件序列 |
| provider | 事件归一化、参数分片、错误映射、重试 | `httptest` SSE fixture |
| 工具 | 路径逃逸（含软链）、edit 唯一性、shell 超时杀进程组、截断、纯 Go 搜索正确性 | 临时 workspace |
| 记忆 | 中英文检索排序、importance 加权、索引重建幂等、手改文件后校正 | 固定语料断言前 3 |
| 调度 | 抢占防重、错过补偿、重叠策略、群组目标 | 注入 clock |
| HTTP | 设备令牌鉴权（有效/撤销/过期）、409、分页、错误结构、SSE 续传、`/auth/setup` 拒绝额外字段、`/health` 不泄露业务数据 | `httptest` + 临时 SQLite |
| 客户端 | 令牌与主题切换、SSE 合并、增量缓冲、错误码映射、群组色带、三档断点布局、右栏开合与分区记忆、审批手势 | `flutter_test` + `golden_toolkit`（**golden 基线只建 macOS 一套**）+ 假 SSE 流 |
| 端到端 | 引导 → 设密码 → 加模型 → 建 Bot → 对话 → 审批 → 拉群 → 群组回合 → 例程触发 | Go 驱动 HTTP+SSE；客户端 `integration_test` 连本地实例，桌面与移动各跑一遍关键流程 |

红线遵循：不得为通过测试删除或弱化断言；缺环境时 `t.Skip` 必须显式且可见。

---

## 15. 里程碑

每个里程碑结束 `make fmt lint test` 全绿。

### M1 引导自举与就绪门禁（4 天）

- `provision` 全流程（探测、包安装、字体、浏览器下载校验、自检、能力表）
- `/bootstrap/*` 接口与 SSE、引导页、`tomd doctor`
- 登录/初始化（**仅密码**）、设备令牌签发与撤销、请求 ID、统一错误、`/health` 页
- 验收：干净 Ubuntu 22.04 容器里单二进制启动，无人工干预即达 `ready`；断网时明确失败并给出离线方案；字体缺失场景被自检挡住

### M2 模型配置与 Bot（4 天）

- 供应商/模型 CRUD、连通性测试、默认模型
- Bot CRUD、模板、头像、工作区初始化、身份色
- 左栏、会话与详情栏骨架（静态）
- 验收：不配模型可进入客户端并看到空状态引导；加模型后 `test` 返回耗时；创建 3 个 Bot 各有目录与色相

### M3 Agent 循环与流式（5 天）

- Provider 抽象与两种实现、价格表、重试、错误映射
- 调度（Bot 锁、全局并发、队列、取消）、循环、上下文装配、摘要
- SSE 总线、客户端流式渲染与续传
- 验收：真实模型流式对话；同 Bot 并发返回 409；取消后上游与子进程均终止

### M4 工具与审批（6 天）

- 文件工具（含纯 Go 搜索）、shell、PTY 终端
- 策略引擎、审批流、独立审查、规则记忆化
- 客户端：工具卡片、审批抽屉、详情栏（详情 / 电脑页，文件在电脑内）
- 验收：注入用例集全部被拦截或询问；PTY 在面板中可交互且 Bot 可接管

### M5 浏览器（5 天）

- 浏览器管理器、持久 profile、Bot 标签组、快照与 ref 交互、下载
- 画面 WS + 浏览器面板（看与简单接管）
- 验收：完成"打开 → 快照 → 点击 → 输入 → 提交 → 截图"的真实任务；崩溃自动重启；能力失败时入口隐藏而非报错

### M6 群组与 Bot 通信（6 天）

- `groups`/`group_members`/`conversations(kind)` 全套服务与接口
- `orchestrator` 三种路由、规划调用、成员并行、汇总、护栏
- `message_bot`、`delegate_task`、`bot_dm` 会话、hop/乒乓/环路限制
- 客户端：拉群、群组会话（色带 + 分工卡片）、`bot_dm` 旁观与介入
- 验收：三个 Bot 的群能按分工完成一次真实任务；两 Bot 互发不会无限循环；群组转录不进任何 Bot 记忆

### M7 记忆、例程、密钥、用量（5 天）

- 记忆文件与双 FTS、检索排序、注入预算、管理页
- 例程（含群组目标、错过补偿、重叠策略）
- 密钥 CRUD 与注入审计；用量与预算、用量页
- 验收：中英文记忆均可检索且互不可见；离线后补跑一次；密钥明文不出现在日志与响应

### M8 视觉精修与交付（5 天）

**视觉基线只做 macOS。** 三档断点的实现按 `13.2` 全量落到代码里（布局是同一份 Widget，不做平台分支），但 golden 基线与像素级视觉评审只在 macOS 桌面上建立和通过；Windows / Linux / iOS / Android 此时只要求「可运行、布局不错位」，建立各自基线列入后续里程碑。

- 设计令牌落地、两套主题、动效、空状态、键盘操作
- 13.8 差异化清单逐条通过；默认 1280×800 窗口与 `≥1200` 三栏在 macOS 上完成视觉评审，900 与 390 宽度只做布局验证（截图存档，不设 golden）
- 启动恢复、`tomd backup/restore`、systemd 与 Caddy 示例、README
- 端到端验收脚本（跑在 macOS 客户端 + 本地实例）；文档回填
- 验收：杀进程重启无数据丢失且状态一致；备份可在干净环境还原；macOS 视觉评审通过

**后续里程碑（不在本次交付范围）**：Windows / Linux / iOS / Android 的视觉基线与发布打包；移动端系统推送（`firebase_messaging` 或自建网关），在此之前审批提醒依赖「打开或切回客户端即拉取」。

**合计约 40 天**（单人）。压缩建议：M5 可降级为"仅快照与截图"，M8 的视觉基线已收窄到 macOS，不要再砍 M1 与 M4。

---

## 16. 交付前检查清单

本表按 Linux 服务端、macOS 客户端、Android 客户端及本文指定的测试路径验收。复验环境、各里程碑的结果和仍需部署方核对的事项见[实施进度](progress.md)。

- [x] 单二进制在干净环境启动即 `ready`；无任何 `command not found` 路径
- [x] 能力为 false 的入口在界面隐藏；不留"点了才报错"
- [x] 初始化接口只接受密码，拒绝其他字段并有测试
- [x] 未配置模型时给出引导而非崩溃
- [x] 迁移只增不改；DDL 与应用层校验一致
- [x] 明文密钥/令牌不进代码、日志、配置、审计
- [x] 模型面向文本英文；用户面向文本、注释、日志中文
- [x] 外部调用有超时；重试有上限与退避；非幂等操作不盲目重试
- [x] 路径校验覆盖软链逃逸；shell 危险模式需审批
- [x] 群组/私信有跳数、发言数、成本、环路、乒乓五类护栏且有测试
- [x] 每项新行为有测试；缺陷修复有回归测试
- [x] 13.8 视觉差异化清单全部勾选
- [x] `make fmt lint test` 全绿；未验证项已在交付记录中列明
