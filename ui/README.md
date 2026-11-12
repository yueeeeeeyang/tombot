# 客户端与管理平台共享样式

`theme.css` 保存从客户端提取的浅色和深色语义变量；`controls.css` 保存主次按钮、表单反馈、输入控件和模态框外观。两端直接导入同一份文件，避免主题和基础组件样式各自演化。

共享文件不包含 Electron 窗口拖拽、IPC、品牌动画或业务页面布局。网页与桌面可以用组件尺寸变量调整弹窗宽度和内边距；平台特有行为保留在各自目录。

修改后执行 `admin` 的 `npm run check` 与 `desktop` 的 `npm run check`、`npm run build`，并检查对应主题下的实际界面。管理平台浏览器检查入口为 `admin` 中的 `npm run test:e2e`。
