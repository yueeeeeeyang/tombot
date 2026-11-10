# 系统链接与部署

## 打开工作室

`tom://bot/<id>` 打开 Bot 单聊，`tom://group/<id>` 打开群组，`tom://dm/<conversationId>` 打开 Bot 私信旁观页。标识取自当前实例，使用服务端生成的 26 位大写 ULID。冷启动时先恢复设备会话。需要登录时保留最近一次会话链接。布局恢复后打开指定会话。热启动时在当前窗口切换会话。

`tom://server?addr=<编码后的地址>` 适合放入二维码。将服务地址用 `Uri.encodeComponent` 编码，例如 `tom://server?addr=https%3A%2F%2Fstudio.example`。打开链接会在连接页填入地址，点击「连接服务器」后才访问目标。已有客户端时先显示待使用的地址，允许取消。切换到新实例时清除表单和内存凭据，目标实例独立登录。地址支持 HTTPS 或环回 HTTP，拒绝带用户信息、查询、片段或任意路径的地址。

客户端现在将服务器地址和设备凭据作为一条系统安全存储记录写入，避免切换中断导致错配。首次运行会迁移已有的完整会话，写入成功后再清除旧键。中断后优先使用已提交的新记录。降回旧客户端需重新输入地址并登录。此变更只涉及设备存储。

## 五个平台的自定义协议

- macOS 和 iOS 在 Info.plist 声明 `tom`，Android 由主 Activity 接受可浏览的 VIEW 意图。
- Windows 在解压后的发布目录运行随附的 `Register-TomLink.ps1`，向当前账户注册协议。移动应用目录后重新运行脚本。卸载时，确认 `HKCU:\Software\Classes\tom\shell\open\command` 指向本次安装，再删除该协议键。
- Linux 将完整 bundle 放入固定目录，将其中的 `tom` 加入桌面会话的 PATH，或将随包的 `io.yueyang.tom.desktop` 的 Exec 项改为带 `%u` 的绝对路径。在桌面会话执行以下命令。卸载时移除该桌面入口，并在系统设置中恢复以前选用的协议处理程序。

```sh
xdg-desktop-menu install --novendor io.yueyang.tom.desktop
xdg-mime default io.yueyang.tom.desktop x-scheme-handler/tom
xdg-open 'tom://server?addr=https%3A%2F%2Fstudio.example'
```

## 移动端 HTTPS 关联

使用自己控制的 HTTPS 域名。分发方负责该域名和移动应用签名。四类路径为 `/bot/<id>`、`/group/<id>`、`/dm/<id>`、`/server?addr=...`。同一个域名同时传给原生关联和 Dart 路由。默认构建只接受自定义协议。以下以保留域名说明构建方式。

```sh
# 从仓库根目录构建。正式签名前，在开发者账号配置匹配的应用标识与 Associated Domains 能力。
python3 scripts/build_ios_links.py links.example
# 本机无签名编译验证可追加 --mode debug --no-codesign。
cd client
flutter build apk --debug --dart-define=TOM_LINK_HOST=links.example
```

iOS 在 `https://<域名>/.well-known/apple-app-site-association` 提供下列结构的 JSON。将占位值换成本次签名的 App ID 前缀，签名后的应用必须含该域名的 associated-domains entitlement。

```json
{"applinks":{"details":[{"appIDs":["<APP_ID_PREFIX>.io.yueyang.tom"],"components":[{"/":"/bot/*"},{"/":"/group/*"},{"/":"/dm/*"},{"/":"/server"}]}]}}
```

Android 在 `https://<域名>/.well-known/assetlinks.json` 提供以下 JSON，将占位值换成**实际安装包签名证书**的 SHA-256 指纹。通过应用商店分发时使用应用签名证书指纹。两个文件均需提供有效 TLS、HTTP 200 和 JSON 内容类型，直接从对应路径响应。正式域名仅关联实际分发的应用签名。

```json
[{"relation":["delegate_permission/common.handle_all_urls"],"target":{"namespace":"android_app","package_name":"io.yueyang.tom","sha256_cert_fingerprints":["<SHA256_CERT_FINGERPRINT>"]}}]
```

安装签名包后，从邮件或其他应用点开真实域名上的链接。Android 12 及以上可用 `adb shell pm verify-app-links --re-verify io.yueyang.tom` 后执行 `adb shell pm get-app-links io.yueyang.tom` 检查域名是否为 verified。撤销部署时更新站点关联文件，再以普通构建替换客户端。已安装设备遵循操作系统的关联缓存周期。[Apple 域名关联文档](https://developer.apple.com/documentation/xcode/supporting-associated-domains) · [Android 域名验证文档](https://developer.android.com/training/app-links/verify-android-applinks)

## 复验

```sh
cd client
flutter test test/deep_link_test.dart
flutter test -d macos integration_test/deep_link_test.dart
flutter test -d macos integration_test/secure_session_test.dart
```

组件测试覆盖四类入口、可信域名、恶意输入、布局恢复、反复切换、迁移中断和跨实例凭据隔离。macOS 原生测试通过 Launch Services 指定本次应用包。Linux 使用 GIO 调起随包的桌面入口。两端均验证 Dart 开始监听前的缓冲和随后同一连接页的再次接收。Debian 12 ARM64 的正式 Release 还通过系统辅助功能树核对了全新进程冷启动与再次打开链接后的输入框内容。Linux 用户可在带会话 D-Bus 的 GTK 桌面运行 `flutter test -d linux integration_test/deep_link_test.dart` 和 `flutter test -d linux integration_test/window_placement_test.dart`。以 `flutter test -d linux integration_test/secure_session_test.dart` 可复验实际钥匙串的读写和清理。Linux 本机构建需安装 Flutter 的桌面工具链及 `libsecret-1-dev`。设备授权依赖当前桌面的 Secret Service。[Flutter Linux 环境文档](https://docs.flutter.dev/platform-integration/linux/setup)

移动端编译和清单检查可在无签名域名的环境进行。系统域名关联需在实际签名包和部署域名上验证。
