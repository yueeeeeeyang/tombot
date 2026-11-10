# 官方离线浏览器包

这里是 Chrome for Testing 157.0.8103.38 的四个平台包。引导直接使用它们，不会在运行时从网络下载。

| 平台 | 文件 |
| --- | --- |
| linux/amd64 | `chrome-linux64.zip` |
| linux/arm64 | `chrome-linux-arm64.zip` |
| darwin/amd64 | `chrome-mac-x64.zip` |
| darwin/arm64 | `chrome-mac-arm64.zip` |

每个包约 190–200MB，超过普通 Git 的单文件上限，因此用 Git LFS 存放。使用前必须先安装 Git LFS。没安装时 `git lfs` 不是 git 子命令。macOS 执行 `brew install git-lfs`；Debian/Ubuntu 执行 `sudo apt-get install git-lfs`；Fedora 执行 `sudo dnf install git-lfs`；也可以按 [Git LFS](https://git-lfs.com) 安装。装好后在仓库根目录执行：

```sh
git lfs install
git lfs pull
```

工作区里如果还是指针，引导会直接读 `.git/lfs/objects` 中的对应对象，不会把指针当 zip 校验。本地没有对象或系统没有 `git-lfs` 时，引导会先说明需要安装，再给出上面的步骤。

摘要必须与 `server/internal/provision/browser.go` 中的固定值一致。升级版本时同时更换这四个文件和代码里的版本、摘要。
