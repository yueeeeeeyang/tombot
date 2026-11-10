# 备份、还原与 Linux 部署

## 取得可恢复的归档

使用与服务相同的操作系统账户、目录配置和主密钥环境。停止服务，等独立的工作区写入程序和使用同一 profile 的 Chromium 退出。备份命令持有服务的目录排他锁，运行中的服务会使备份立即失败。把输出放在数据和工作区之外已经存在的私有目录。

```sh
export TOM_DATA_DIR=/var/lib/tom/data
export TOM_WORKSPACE_DIR=/var/lib/tom/workspace
tomd backup -o /var/backups/tom/before-upgrade.tar.gz
```

省略 `-o` 时归档写入当前目录，文件名包含 UTC 时间。命令通过 SQLite 备份接口读出一致快照，其中包含已提交的 WAL 页面。归档收录认证文件、密文、历史会话、FTS、工作区、浏览器安装包、运行时与 profile。重建时会重新取得实例锁并创建 SQLite 临时文件。Chromium 的 Singleton 锁、套接字、Cookie 链接、运行版本链接与 DevTools 临时端口文件一并排除。

- 文件主密钥模式会收录私有的 `data/master.key`。归档说明和命令结果均提示“此归档含解密密钥，需自行加密保存”。
- 环境主密钥模式从服务环境注入相同的 `TOM_MASTER_KEY`。归档排除磁盘上可能遗留的文件密钥，恢复时验证原环境主密钥。请通过单独的密钥管理渠道保存它。
- 两种模式的归档均包含工作区和浏览器登录资料。使用私有存储、独立加密和受信任的传输渠道。文件主密钥随包时，持有归档者也能重算完整性校验值。

## 恢复到新目录

选择已有父目录中的**新路径**。命令在同一文件系统的私有暂存目录中验证并写入数据和工作区，再用一次排他改名同时发布两棵树。既有路径（含空目录或符号链接）会导致命令退出。归档所在的文件系统可以与还原目标不同。

```sh
tomd restore --input /var/backups/tom/before-upgrade.tar.gz --target /var/lib/tom/restored
export TOM_DATA_DIR=/var/lib/tom/restored/data
export TOM_WORKSPACE_DIR=/var/lib/tom/restored/workspace
tomd
```

从原文件密钥恢复时，移除进程环境中冲突的 `TOM_MASTER_KEY`。从环境密钥恢复时，先通过部署的机密注入方式设置原值。即使数据库尚无加密记录，也会验证环境密钥。还原同时检查归档内容、SQLite 完整性、外键、迁移账本与现存密文。使用原设备访问工作区并验证历史会话，确认健康页已就绪后再切换流量。使用 systemd 时，在服务环境文件中切换两个目录并重启。

普通文件保留内容、权限和修改时间，相对符号链接必须能在自己的目录树内解析。硬链接按独立文件恢复。文件归属为运行还原命令的账户，需用服务账户执行。这种可移植归档只保存普通 Unix 权限，另行管理 ACL、扩展属性等主机策略。设备文件、FIFO、断链、指向树外或排除文件的链接会导致备份失败。跨平台恢复时，引导会检查本机架构对应的运行时。归档里摘要已经匹配的浏览器包会直接使用；否则把该平台的官方 zip 放到数据目录同级的 `browser/`，或用 `TOM_BROWSER_ARCHIVE` 指定路径。引导不会从网络下载浏览器。

## 容量、停机与回滚

归档版本 1 最多记录 100 万项，总展开内容上限 256 GiB，目录深度上限 128 层，所有链接名称合计上限 32 MiB。归档按页读取数据库，分批枚举目录并流式压缩文件。数据库校验、全量压缩、解压及完整性检查所需时间随大小增长。备份停机窗口应覆盖整次归档。输出盘应容纳一份数据库快照和压缩包，还原盘应容纳整个展开实例。提前用实际数据测量耗时与磁盘余量。

失败时命令返回非零，日常错误会清理暂存，校验未通过时不会发布实例。进程被强制终止或机器断电后，先确认没有活动的备份或还原，再检查目标父目录里的 `.tom-backup-*` 和 `.tom-restore-*` 私有暂存目录。重新运行时使用新的空闲目标名。保留旧实例直至验证完成，切换后出现故障时先停新服务，再切回成对保存的目录与匹配的旧二进制。切换后的新增数据需单独处理。

本次没有新增数据库迁移。启动和归档校验共用迁移账本校验，遇到缺口、名称或摘要被改动以及未来版本时会失败。升级前应保留旧版本与完整快照，回滚涉及数据结构升级时必须使用升级前的快照。文件主密钥必须是仅所有者可读写的普通文件，使用链接提供密钥的既有实例需改用受保护的普通文件或原值的环境注入。

## systemd 与 Caddy

仓库提供非 root 单元 `deploy/tomd.service` 和代理配置 `deploy/Caddyfile`。在 Ubuntu 22.04 上先由管理员安装下列运行库与字体，再以专用账户运行服务。使用已存在的同名账户时先核对其用途与主目录。

```sh
sudo apt-get update
sudo apt-get install --no-install-recommends ca-certificates fontconfig fonts-noto-cjk \
  libnss3 libatk1.0-0 libatk-bridge2.0-0 libcups2 libdrm2 libxkbcommon0 \
  libxcomposite1 libxdamage1 libxfixes3 libxrandr2 libgbm1 libasound2 libpango-1.0-0 libcairo2
sudo useradd --system --user-group --home-dir /var/lib/tom --shell /usr/sbin/nologin tom
sudo install -m 0755 bin/tomd-linux-arm64 /usr/local/bin/tomd
sudo install -d -o tom -g tom -m 0755 /var/lib/tom/browser
sudo install -o tom -g tom -m 0644 chrome-linux-arm64.zip /var/lib/tom/browser/chrome-linux-arm64.zip
sudo install -m 0644 deploy/tomd.service /etc/systemd/system/tomd.service
sudo install -d -m 0700 /etc/tom
sudo systemctl daemon-reload
sudo systemctl enable --now tomd
```

上例二进制适用于 arm64，amd64 主机使用对应的发布文件，并把离线包换成 `chrome-linux64.zip`。压缩包是 Chrome for Testing 157.0.8103.38，须事先放到数据目录同级的 `browser/`，服务不会自己下载。服务仅监听 `127.0.0.1:8787`。持久状态位于 `/var/lib/tom`，可选环境文件为 `/etc/tom/tom.env`，由管理员以 0600 权限保管。在服务器本机通过私有文件 `/run/tom/console` 取得一次性初始化码，服务诊断写入 journal。配置 `TOM_MASTER_KEY` 的部署必须让停机备份命令也从同一安全渠道获得该值。初始化、备份、还原和代理验证过程均无需在命令行参数中传递密钥。

安装 Caddy 2 后，复制示例到 Caddy 的活动配置路径，在 **Caddy 进程**的环境中设置 `TOM_PUBLIC_HOST` 为自己的域名。让域名指向该主机，并确保签发证书所需的网络条件满足。先在相同环境下运行 `caddy validate --config /etc/caddy/Caddyfile`，再重载 Caddy。变更服务的环回端口时同步更新代理上游。代理使用默认的 SSE 刷新与 WebSocket 升级行为。[Caddy 反向代理文档](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy#streaming)

## 服务自身提供 HTTPS

同时设置 `TOM_TLS_CERT_FILE` 和 `TOM_TLS_KEY_FILE`，分别指向 PEM 证书链与匹配的私钥，将 `TOM_ADDR` 配置为客户端可达的监听地址。私钥由服务账户经受控的文件权限读取，建议把部署用证书放在业务数据与工作区之外。服务在打开业务目录前加载证书，证书缺失、格式错误或密钥不匹配会阻止启动。客户端使用证书所含域名连接。证书续期后在维护窗口重启服务。切回 Caddy 时移除这两个变量，恢复环回监听，重启服务后验证代理入口。两种部署方式使用同一套授权、SSE 与 WebSocket 路由。

## 复验

活动任务与调度可通过 `python3 scripts/verify_active_recovery.py bin/tomd /路径/官方浏览器包.zip` 复验。脚本在临时目录启动正式服务，向受鉴权的环回模型端点发送请求。它在一条可见流式消息与另一条文件审批同时存在时强制终止临时进程组，让服务在每日例程到期时保持离线，再连续启动两次。检查原运行标识、部分正文、工具失败回执、审批过期、未写入的目标文件、例程实际完成次数、两位 Bot 的中英文私有检索，以及进程日志和 API 响应中的密钥脱敏。整个过程按真实墙钟等待，不调整系统时间。

`go test -race ./internal/backup ./cmd/tomd` 覆盖归档、错误密钥、损坏和并发发布。运行 `python3 scripts/verify_restore.py bin/tomd /路径/官方浏览器包.zip` 可在临时目录验证真实强制终止、重启、备份、移除原实例、还原及再次登录。添加 `--environment-key` 可验证环境密钥路径。该脚本只在环回地址启动测试实例，凭据在运行时生成。
