package dockernode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
)

type filesystemMount struct {
	target, source, root, kind, device string
	options                            map[string]bool
}

func mountField(value string) (string, error) {
	var decoded strings.Builder
	for index := 0; index < len(value); index++ {
		if value[index] != '\\' {
			decoded.WriteByte(value[index])
			continue
		}
		if index+3 >= len(value) {
			return "", errors.New("挂载路径转义无效")
		}
		number, err := strconv.ParseUint(value[index+1:index+4], 8, 8)
		if err != nil || (number != 32 && number != 9 && number != 10 && number != 92) {
			return "", errors.New("挂载路径转义无效")
		}
		decoded.WriteByte(byte(number))
		index += 3
	}
	return decoded.String(), nil
}

func parseMounts(encoded []byte) ([]filesystemMount, error) {
	mounts := make([]filesystemMount, 0)
	for _, line := range strings.Split(string(encoded), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		separator := -1
		for index, field := range fields {
			if field == "-" {
				separator = index
				break
			}
		}
		if separator < 6 || len(fields) != separator+4 {
			return nil, errors.New("系统挂载记录格式无效")
		}
		target, err := mountField(fields[4])
		if err != nil {
			return nil, err
		}
		source, err := mountField(fields[separator+2])
		if err != nil {
			return nil, err
		}
		root, err := mountField(fields[3])
		if err != nil {
			return nil, err
		}
		options := make(map[string]bool)
		for _, option := range strings.Split(fields[5], ",") {
			options[option] = true
		}
		mounts = append(mounts, filesystemMount{target: target, source: source, root: root, kind: fields[separator+1], device: fields[2], options: options})
	}
	return mounts, nil
}

func readMounts() (mounts []filesystemMount, result error) {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, errors.New("读取系统挂载记录失败")
	}
	defer func() { result = errors.Join(result, file.Close()) }()
	const maximum = 8 << 20
	encoded, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || len(encoded) > maximum {
		return nil, errors.New("系统挂载记录过大或不可读取")
	}
	return parseMounts(encoded)
}

type loopDevice struct {
	Name          string `json:"name"`
	BackingFile   string `json:"back-file"`
	BackingInode  uint64 `json:"back-ino"`
	BackingDevice string `json:"back-maj:min"`
	Device        string `json:"maj:min"`
	Offset        int64  `json:"offset"`
	SizeLimit     int64  `json:"sizelimit"`
	ReadOnly      bool   `json:"ro"`
}

func validLoopDevice(name string) bool {
	if !strings.HasPrefix(name, "/dev/loop") {
		return false
	}
	number, err := strconv.ParseUint(strings.TrimPrefix(name, "/dev/loop"), 10, 20)
	return err == nil && name == "/dev/loop"+strconv.FormatUint(number, 10)
}

func fmtDevice(major, minor uint32) string {
	return strconv.FormatUint(uint64(major), 10) + ":" + strconv.FormatUint(uint64(minor), 10)
}

func (storage *FileStorage) loops(ctx context.Context, state *storageRecord) ([]loopDevice, error) {
	if state.ImageInode == 0 {
		return nil, nil
	}
	encoded, err := storage.command(ctx, "losetup", "--list", "--json", "--output", "NAME,BACK-FILE,BACK-INO,BACK-MAJ:MIN,MAJ:MIN,OFFSET,SIZELIMIT,RO")
	if err != nil {
		return nil, err
	}
	var response struct {
		Devices *[]loopDevice `json:"loopdevices"`
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	if err = decoder.Decode(&response); err != nil {
		return nil, errors.New("读取磁盘设备身份失败")
	}
	if err = decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("磁盘设备记录包含多份内容")
	}
	if response.Devices == nil {
		return nil, errors.New("磁盘设备记录缺少明确列表")
	}
	var matching []loopDevice
	for _, device := range *response.Devices {
		if device.BackingInode != state.ImageInode || device.BackingDevice != deviceIdentity(state.ImageDevice) {
			continue
		}
		if !validLoopDevice(device.Name) || device.Device != "7:"+strings.TrimPrefix(device.Name, "/dev/loop") || device.Offset != 0 || device.SizeLimit != state.CapacityBytes || device.ReadOnly {
			return nil, failure("storage_loop_identity_mismatch")
		}
		matching = append(matching, device)
	}
	if len(matching) > 1 {
		return nil, failure("storage_multiple_loop_devices")
	}
	return matching, nil
}

func (storage *FileStorage) mounted(ctx context.Context, resource Resource, state *storageRecord) (bool, error) {
	mounts, err := storage.mounts()
	if err != nil {
		return false, err
	}
	var matching []filesystemMount
	for _, mount := range mounts {
		if strings.HasPrefix(mount.target, resource.WorkspacePath+"/") {
			return false, failure("storage_nested_mount")
		}
		if mount.target == resource.WorkspacePath {
			matching = append(matching, mount)
		}
	}
	if len(matching) == 0 {
		return false, nil
	}
	if len(matching) != 1 {
		return false, failure("storage_stacked_mounts")
	}
	mount := matching[0]
	if mount.kind != "ext4" || mount.root != "/" || !mount.options["rw"] || !mount.options["nosuid"] || !mount.options["nodev"] || mount.options["ro"] || mount.options["noexec"] {
		return false, failure("storage_mount_invalid")
	}
	if err = storage.verifyImage(resource, state, true); err != nil {
		return false, err
	}
	devices, err := storage.loops(ctx, state)
	if err != nil {
		return false, err
	}
	if len(devices) != 1 || mount.source != devices[0].Name || mount.device != devices[0].Device {
		return false, failure("storage_mount_identity_mismatch")
	}
	size, err := storage.filesystemSize(resource.WorkspacePath)
	if err != nil {
		return false, err
	}
	if size <= 0 || size > state.CapacityBytes {
		return false, failure("storage_capacity_mismatch")
	}
	return true, nil
}
