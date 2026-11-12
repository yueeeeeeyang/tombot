package dockernode

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"syscall"

	"github.com/oklog/ulid/v2"
	"golang.org/x/sys/unix"
)

type storageRecord struct {
	Version       int    `json:"version"`
	Name          string `json:"name"`
	UserID        string `json:"user_id"`
	ComputerID    string `json:"computer_id"`
	NodeID        string `json:"node_id"`
	OperationID   string `json:"operation_id"`
	Generation    int64  `json:"generation"`
	CapacityBytes int64  `json:"capacity_bytes"`
	UUID          string `json:"uuid"`
	ImageDevice   uint64 `json:"image_device"`
	ImageInode    uint64 `json:"image_inode"`
	Phase         string `json:"phase"`
}

func initialStorageRecord(resource Resource) *storageRecord {
	specification := resource.Specification
	return &storageRecord{Version: 3, Name: resource.Name, UserID: specification.UserID, ComputerID: specification.ComputerID, NodeID: specification.NodeID, OperationID: specification.OperationID,
		Generation: specification.Generation, CapacityBytes: specification.Resources.DiskBytes, UUID: filesystemUUID(resource), Phase: "reserved"}
}

func (storage *FileStorage) load(resource Resource) (*storageRecord, error) {
	encoded, err := storage.journal.readFile(resource.Specification.UserID + ".storage.json")
	if err != nil || encoded == nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var state storageRecord
	if err = decoder.Decode(&state); err != nil {
		return nil, errors.New("持久磁盘记录格式无效")
	}
	if err = decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("持久磁盘记录包含多份内容")
	}
	// 每用户只保留最高代次；核验旧身份后才能解释为下一代尚未创建，不能忽略损坏的关闭记录。
	previous := resource
	previous.Specification.Generation = state.Generation
	previous.Specification.Resources.DiskBytes = state.CapacityBytes
	previous.Name = resourceName(previous.Specification)
	if state.Generation < 1 || state.CapacityBytes < 1 {
		return nil, failure("storage_record_identity_mismatch")
	}
	expected := initialStorageRecord(previous)
	expected.Phase = state.Phase
	expected.ImageDevice = state.ImageDevice
	expected.ImageInode = state.ImageInode
	if state != *expected {
		return nil, failure("storage_record_identity_mismatch")
	}
	switch state.Phase {
	case "reserved", "allocating", "formatting", "formatted", "ready", "removing", "removed":
	default:
		return nil, failure("storage_record_phase_invalid")
	}
	if state.Generation != resource.Specification.Generation {
		if state.Phase != "removed" || state.Generation >= resource.Specification.Generation || resource.Specification.Generation-state.Generation != 1 {
			return nil, failure("storage_generation_not_released")
		}
		if err := storage.requireAbsentDirectory(previous); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if state.CapacityBytes != resource.Specification.Resources.DiskBytes {
		return nil, failure("storage_record_identity_mismatch")
	}
	return &state, nil
}

func (storage *FileStorage) save(state *storageRecord) error {
	encoded, err := json.Marshal(state)
	if err != nil {
		return errors.New("编码持久磁盘记录失败")
	}
	return storage.journal.writeFile(state.UserID+".storage.json", encoded)
}

// validateStorageLayout 拒绝旧按代次分散的记录，防止忽略旧屏障后重新创建资源；不自动迁移或删除。
func (storage *FileStorage) validateStorageLayout() (result error) {
	directory, err := storage.journal.root.Open(".")
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, directory.Close()) }()
	for {
		entries, readErr := directory.ReadDir(100)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		for _, entry := range entries {
			name := entry.Name()
			if name == "loop-allocation.lock" || strings.HasPrefix(name, ".write-") && canonicalRecordID(strings.TrimPrefix(name, ".write-")) {
				continue
			}
			userID := strings.TrimSuffix(name, ".storage.json")
			if userID == name {
				userID = strings.TrimSuffix(name, ".storage.lock")
			}
			if userID == name || !canonicalRecordID(userID) {
				return failure("storage_journal_layout_invalid")
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
	}
}

func canonicalRecordID(value string) bool {
	id, err := ulid.ParseStrict(value)
	return err == nil && id.String() == value
}

func (storage *FileStorage) openImage(resource Resource, create bool) (*os.File, error) {
	flags := os.O_RDWR | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
	if create {
		flags |= os.O_CREATE | os.O_EXCL
	}
	file, err := os.OpenFile(storage.image(resource), flags, 0600)
	if err != nil {
		return nil, err
	}
	if err = storage.journal.checkFile(file); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

func imageIdentity(file *os.File) (device, inode uint64, size, allocated int64, result error) {
	stat, err := file.Stat()
	if err != nil {
		return 0, 0, 0, 0, errors.New("读取持久镜像属性失败")
	}
	identity, ok := stat.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, 0, 0, errors.New("持久镜像身份不可用")
	}
	return uint64(identity.Dev), identity.Ino, stat.Size(), identity.Blocks * 512, nil
}

func (storage *FileStorage) verifyImage(resource Resource, state *storageRecord, formatted bool) (result error) {
	file, err := storage.openImage(resource, false)
	if err != nil {
		return errors.New("持久镜像不存在或不可安全打开")
	}
	defer func() { result = errors.Join(result, file.Close()) }()
	device, inode, size, _, err := imageIdentity(file)
	if err != nil {
		return err
	}
	if state.ImageInode == 0 || state.ImageDevice != device || state.ImageInode != inode || size != state.CapacityBytes {
		return failure("storage_image_identity_mismatch")
	}
	if !formatted {
		return nil
	}
	return verifyExt4(file, state.UUID)
}

func verifyExt4(file io.ReaderAt, uuid string) error {
	var superblock [120]byte
	if _, err := file.ReadAt(superblock[:], 1024); err != nil {
		return failure("storage_superblock_invalid")
	}
	if binary.LittleEndian.Uint16(superblock[0x38:0x3a]) != 0xef53 || hex.EncodeToString(superblock[0x68:0x78]) != strings.ReplaceAll(uuid, "-", "") {
		return failure("storage_filesystem_identity_mismatch")
	}
	return nil
}

func deviceIdentity(device uint64) string { return fmtDevice(unix.Major(device), unix.Minor(device)) }
