//go:build !linux

package dockernode

import "errors"

func pinLoopDevice(loopDevice, *storageRecord) (func() error, error) {
	return nil, errors.New("持久磁盘后端仅支持 Linux")
}

func availableStorageBytes(string) (uint64, error) {
	return 0, errors.New("持久磁盘后端仅支持 Linux")
}
func mountedFilesystemSize(string) (int64, error) {
	return 0, errors.New("持久磁盘后端仅支持 Linux")
}
