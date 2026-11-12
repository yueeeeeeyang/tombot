package computer

// 持久盘只按服务用途划分，所有机器人仍共享同一业务目录和同一浏览器账户。
// 不挂载磁盘根目录，避免业务进程通过重命名父目录替换浏览器服务的私有配置。
const (
	WorkspaceSubdirectory = "workspace"
	BrowserSubdirectory   = "browser"
	BrowserDirectory      = "/var/lib/tombot-browser"
	BrowserRuntimeRoot    = "/run/tombot-browser"
	StorageLayoutLabel    = "io.tombot.computer.storage-layout"
	StorageLayoutVersion  = "2"
)
