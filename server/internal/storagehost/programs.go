// Package storagehost 定义节点持久磁盘使用的宿主程序契约，供安装引导和运行时共同核验。
package storagehost

// Program 描述固定命令入口及无副作用的启动检查参数，不接受外部程序路径。
type Program struct {
	Name            string
	Path            string
	VersionArgument string
}

// Programs 返回独立副本，避免部署检查与运行时的磁盘工具要求发生漂移。
func Programs() []Program {
	return []Program{
		{Name: "fallocate", Path: "/usr/bin/fallocate", VersionArgument: "--version"},
		{Name: "mkfs", Path: "/usr/sbin/mkfs.ext4", VersionArgument: "-V"},
		{Name: "fsck", Path: "/usr/sbin/e2fsck", VersionArgument: "-V"},
		{Name: "losetup", Path: "/usr/sbin/losetup", VersionArgument: "--version"},
		{Name: "mount", Path: "/usr/bin/mount", VersionArgument: "--version"},
		{Name: "umount", Path: "/usr/bin/umount", VersionArgument: "--version"},
	}
}
