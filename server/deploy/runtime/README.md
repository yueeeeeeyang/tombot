# 跨发行版宿主安装预算

`linux.json` 随电脑镜像包写入 `node-release.json`。清单版本 2 仅配置总超时和磁盘余量，不指定 Ubuntu 版本、Docker 补丁版本或系统工具包版本。amd64/arm64 使用同一预算，电脑镜像仍需与宿主架构匹配。

`linuxhost` 按 `/etc/os-release` 选择 APT 或 DNF、软件包名称、Docker 官方仓库地址与签名指纹。安装结果通过真实包状态、工具权限、Docker API 和隔离能力确认。详细条件及旧安装迁移见[节点部署说明](../NODE.md)。
