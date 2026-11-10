# 官方离线浏览器包

这里是 Chrome for Testing 157.0.8103.38 的四个平台包。引导直接使用它们，不会在运行时从网络下载。

| 平台 | 文件 |
| --- | --- |
| linux/amd64 | `chrome-linux64.zip` |
| linux/arm64 | `chrome-linux-arm64.zip` |
| darwin/amd64 | `chrome-mac-x64.zip` |
| darwin/arm64 | `chrome-mac-arm64.zip` |

每个包约 190–200MB，超过普通 Git 的单文件上限，因此用 Git LFS 存放。克隆后如果文件只有几行 `oid sha256:` 文本，在仓库根目录执行：

```sh
git lfs install
git lfs pull
```

摘要必须与 `server/internal/provision/browser.go` 中的固定值一致。升级版本时同时更换这四个文件和代码里的版本、摘要。
