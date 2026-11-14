# 客户端与管理平台共享组件和样式

`theme.css` 保存从客户端提取的浅色和深色语义变量；`controls.css` 保存主次按钮、表单反馈、输入控件和模态框外观。两端直接导入同一份文件，避免主题和基础组件样式各自演化。

`Select.tsx` 与 `select.css` 提供两端唯一的自绘单选菜单实现，支持方向键、首尾定位、按名称定位、Enter/Space 确认、Escape 取消、Tab/外部点击关闭，以及禁用项和读屏标签。菜单使用浏览器 Popover 顶层，随滚动和窗口变化重新定位，不会被模态框或列表滚动区域裁剪。

管理平台的 `src/components/Select.tsx` 只增加表单适配：保留 FormData 值、必填检查与错误焦点；隐藏的文本控件承接原生约束校验，页面没有原生下拉菜单。桌面原路径已删除，所有使用方直接引用共享组件。两端构建通过 `resolve.dedupe` 使用自身 React 和图标依赖，TypeScript 路径与之对应；管理端静态检查同时覆盖共享源文件。

共享文件不包含 Electron 窗口拖拽、IPC、品牌动画或业务页面布局。网页与桌面可以用组件尺寸变量调整弹窗宽度和内边距；平台特有行为保留在各自目录。

修改后执行 `admin` 的 `npm run check` 与 `desktop` 的 `npm run check`、`npm run build`，并检查对应主题下的实际界面。管理平台浏览器检查入口为 `admin` 中的 `npm run test:e2e`。
