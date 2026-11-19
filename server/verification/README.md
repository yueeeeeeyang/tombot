# 并行桌面浏览器验证

在 `server` 下运行 `make computer-image` 和 `make test-computer-image`。验收覆盖真实 RFB 并发输入、窗口和登录隔离、关闭单个 Chrome 后两桌面的可用性、原生沙箱及重建后的网站状态。可用 `IMAGE_TEST_ARGS="--image 本地镜像标签 --report 新目录"` 指定目标和输出。详见[桌面 Chrome 与电脑就绪](../docs/browser-readiness.md)。
