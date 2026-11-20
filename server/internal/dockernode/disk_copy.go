package dockernode

import (
	"context"
	"encoding/json"
	"errors"
	"io.yueyang/tom/internal/computer"
	"os"
	"runtime"
	"strconv"
	"time"
)

// copyRecord 是目标节点的复制游标；只有数据和摘要复读一致且刷盘后才推进。
type copyRecord struct {
	Source, Target           computer.Specification
	Container, EditID, Stage string
	Device, Inode            uint64
	Copied                   int64
}

func (n *Runtime) saveCopy(p *copyRecord) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return n.journal.writeFile(".disk-copy-"+p.EditID, b)
}
func (n *Runtime) loadCopy(q computer.DiskCopy) (*copyRecord, error) {
	b, err := n.journal.readFile(".disk-copy-" + q.EditID)
	if err != nil || b == nil {
		return nil, err
	}
	var p copyRecord
	if err = json.Unmarshal(b, &p); err != nil || p.EditID != q.EditID || !immutableEqual(p.Source, q.Source) || !immutableEqual(p.Target, q.Target) || p.Container != q.Container || p.Copied < 0 || p.Copied > q.Source.Resources.DiskBytes {
		return nil, failure("disk_copy_record_invalid")
	}
	return &p, nil
}

// CopyDisk 仅处理节点间经认证的固定动作，不允许请求指定路径。原盘始终保留。
func (n *Runtime) CopyDisk(ctx context.Context, q computer.DiskCopy) (receipt computer.DiskReceipt, result error) {
	remain := time.Until(q.Target.LeaseExpiresAt)
	if q.Validate() != nil || q.NodeID() != n.options.NodeID || remain <= 0 || remain > n.options.MaximumLease {
		return receipt, failure("disk_copy_request_invalid")
	}
	ctx, cancel := context.WithTimeout(ctx, remain)
	defer cancel()
	release, err := n.journal.lock(ctx, q.Source.UserID)
	if err != nil {
		return receipt, err
	}
	defer func() { result = errors.Join(result, release()) }()
	store, ok := n.storage.(*FileStorage)
	if !ok {
		return receipt, failure("disk_copy_unavailable")
	}
	state, err := n.journal.read(q.Source.UserID)
	if err != nil {
		return receipt, err
	}
	receipt.EditID = q.EditID
	if q.Action == "freeze" || q.Action == "read" {
		if state == nil || !immutableEqual(state.Specification, q.Source) || state.ContainerID != q.Container || state.restarting() || state.ExportID != "" && state.ExportID != q.EditID || state.Specification.FencingToken >= q.Target.FencingToken {
			return receipt, failure("disk_source_changed")
		}
		res := n.withStorage(n.resource(state.Specification), state)
		disk := n.diskResource(res, state)
		if q.Action == "freeze" {
			if state.ExportFrozen {
				return receipt, nil
			}
			if err = n.journal.requireClosedSessions(ctx, q.Source.UserID); err != nil {
				return receipt, err
			}
			if state.ExportID == "" {
				if state.Phase != "ready" {
					return receipt, failure("disk_source_unavailable")
				}
				state.ExportID = q.EditID
				if err = n.journal.save(state); err != nil {
					return receipt, err
				}
			}
			current, err := n.load(ctx, res, state)
			if err != nil {
				return receipt, err
			}
			if err = n.retireRuntime(ctx, res, current, q.Container); err != nil {
				return receipt, err
			}
			storageState, err := store.load(disk)
			if err != nil {
				return receipt, err
			}
			if storageState == nil || storageState.Phase != "ready" {
				return receipt, failure("disk_source_unavailable")
			}
			if err = store.verifyImage(disk, storageState, true); err != nil {
				return receipt, err
			}
			mounted, err := store.mounted(ctx, disk, storageState)
			if err != nil {
				return receipt, err
			}
			if err = store.unmountDisk(ctx, disk, storageState, mounted); err != nil {
				return receipt, err
			}
			// 同节点改容或以后迁回本机时，活动磁盘记录会被新代替换；保存原盘归属供恢复核验。
			snapshot, err := json.Marshal(struct {
				EditID   string         `json:"edit_id"`
				Computer *record        `json:"computer"`
				Disk     *storageRecord `json:"disk"`
			}{q.EditID, state, storageState})
			if err != nil {
				return receipt, err
			}
			if err = n.journal.writeFile(".disk-source-"+q.EditID, snapshot); err != nil {
				return receipt, err
			}
			state.ExportFrozen = true
			return receipt, n.journal.save(state)
		}
		if state.ExportID != q.EditID || !state.ExportFrozen {
			return receipt, failure("disk_source_not_frozen")
		}
		storageState, err := store.load(disk)
		if err != nil {
			return receipt, err
		}
		if storageState == nil {
			return receipt, failure("disk_source_unavailable")
		}
		if err = store.verifyImage(disk, storageState, true); err != nil {
			return receipt, err
		}
		f, err := store.openImage(disk, false)
		if err != nil {
			return receipt, err
		}
		defer func() { result = errors.Join(result, f.Close()) }()
		receipt.Offset = q.Offset
		receipt.Chunk, err = readDiskChunk(f, q.Offset, q.Source.Resources.DiskBytes)
		return receipt, err
	}
	// 目标节点可能保留了这台电脑早期迁出的只读原盘。只允许更高代次重新迁入。
	if state != nil {
		sameSource := immutableEqual(state.Specification, q.Source) && state.ContainerID == q.Container
		sameTarget := state.ExportID == "" && state.ImportID == q.EditID && immutableEqual(state.Specification, q.Target)
		departed := state.ExportFrozen && state.ExportID != "" && state.Specification.ComputerID == q.Target.ComputerID && state.Specification.Generation < q.Target.Generation
		if !(sameTarget || departed || q.Action == "preflight" && sameSource) {
			return receipt, failure("disk_target_in_use")
		}
	}
	p, err := n.loadCopy(q)
	if err != nil {
		return receipt, err
	}
	resource := n.resource(q.Target)
	switch q.Action {
	case "preflight":
		image, err := n.engine.inspectImage(ctx, q.Target.ImageReference)
		if err != nil {
			return receipt, err
		}
		if err = image.validate(q.Target.ImageReference, "linux/"+runtime.GOARCH); err != nil {
			return receipt, err
		}
		if err = n.verifyProgramImage(image, resource); err != nil {
			return receipt, err
		}
		if p == nil {
			if state == nil {
				previous, err := store.journal.readFile(q.Target.UserID + ".storage.json")
				if err != nil {
					return receipt, err
				}
				if previous != nil {
					return receipt, failure("disk_target_in_use")
				}
			}
			if err = store.requireAbsentDirectory(resource); err != nil {
				return receipt, err
			}
			p = &copyRecord{Source: q.Source, Target: q.Target, EditID: q.EditID, Container: q.Container, Stage: "allocating"}
			if err = n.saveCopy(p); err != nil {
				return receipt, err
			}
		}
		if p.Stage == "allocating" {
			return receipt, n.allocateCopy(ctx, store, resource, p)
		}
		return receipt, nil
	case "write":
		if p == nil || p.Stage != "copying" {
			return receipt, failure("disk_target_not_prepared")
		}
		end := q.Offset + int64(q.Chunk.Size)
		if q.Offset != p.Copied && end != p.Copied {
			return receipt, failure("disk_copy_offset_invalid")
		}
		f, err := copyFile(store, resource, p)
		if err != nil {
			return receipt, err
		}
		defer func() { result = errors.Join(result, f.Close()) }()
		if q.Offset == p.Copied && len(q.Chunk.Data) > 0 {
			if _, err = f.WriteAt(q.Chunk.Data, q.Offset); err != nil {
				return receipt, err
			}
		}
		verify := make([]byte, q.Chunk.Size)
		if _, err = f.ReadAt(verify, q.Offset); err != nil {
			return receipt, err
		}
		if computer.DiskDigest(verify) != q.Chunk.SHA256 {
			return receipt, failure("disk_copy_digest_mismatch")
		}
		if q.Offset == p.Copied {
			if err = f.Sync(); err != nil {
				return receipt, err
			}
			p.Copied = end
			if err = n.saveCopy(p); err != nil {
				return receipt, err
			}
		}
		receipt.Offset = end
		return receipt, nil
	case "install":
		if p == nil || (p.Stage != "copying" && p.Stage != "installed") || p.Copied != q.Source.Resources.DiskBytes {
			return receipt, failure("disk_copy_incomplete")
		}
		if state != nil && state.ImportID == q.EditID && !validUpgradeFence(state.Specification, q.Target) {
			return receipt, failure("disk_copy_stale")
		}
		if p.Stage != "installed" {
			if err = n.installCopy(ctx, store, resource, p); err != nil {
				return receipt, err
			}
		}
		if state == nil || state.ImportID != q.EditID {
			state = &record{Version: lifecycleVersion, Specification: q.Target, Phase: "creating", ImportID: q.EditID}
		}
		state.Specification = q.Target
		if err = n.journal.save(state); err != nil {
			return receipt, err
		}
		obs, err := n.ensure(ctx, resource, state)
		receipt.Observation = &obs
		return receipt, err
	}
	return receipt, failure("disk_copy_request_invalid")
}

func readDiskChunk(f *os.File, offset, total int64) (*computer.DiskChunk, error) {
	size := min(int64(computer.DiskChunkBytes), total-offset)
	b := make([]byte, size)
	if _, err := f.ReadAt(b, offset); err != nil {
		return nil, err
	}
	if !allZero(b) {
		return &computer.DiskChunk{Size: len(b), Data: b, SHA256: computer.DiskDigest(b)}, nil
	}
	// 连续零区间在协议中仅携带长度与摘要，大磁盘的空闲部分不占用网络带宽。
	maxSize := min(int64(computer.ZeroChunkBytes), total-offset)
	span := make([]byte, maxSize)
	if _, err := f.ReadAt(span, offset); err != nil {
		return nil, err
	}
	for i := size; i < maxSize; i += computer.DiskChunkBytes {
		if !allZero(span[i:min(i+computer.DiskChunkBytes, maxSize)]) {
			span = span[:i]
			break
		}
	}
	return &computer.DiskChunk{Size: len(span), SHA256: computer.DiskDigest(span)}, nil
}
func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

func copyFile(s *FileStorage, r Resource, p *copyRecord) (*os.File, error) {
	f, err := s.openImage(r, false)
	if err != nil {
		return nil, err
	}
	dev, ino, _, _, err := imageIdentity(f)
	if err != nil || dev != p.Device || ino != p.Inode {
		return nil, errors.Join(failure("disk_target_changed"), err, f.Close())
	}
	return f, nil
}

func (n *Runtime) allocateCopy(ctx context.Context, s *FileStorage, r Resource, p *copyRecord) (result error) {
	if err := s.checkDirectory(r, true); err != nil {
		return err
	}
	f, err := s.openImage(r, true)
	if errors.Is(err, os.ErrExist) {
		f, err = s.openImage(r, false)
	}
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, f.Close()) }()
	dev, ino, size, allocated, err := imageIdentity(f)
	if err != nil {
		return err
	}
	if p.Inode == 0 {
		if size != 0 {
			return failure("disk_target_changed")
		}
		p.Device, p.Inode = dev, ino
		if err = n.saveCopy(p); err != nil {
			return err
		}
	}
	if p.Device != dev || p.Inode != ino {
		return failure("disk_target_changed")
	}
	needed := max(p.Target.Resources.DiskBytes, p.Source.Resources.DiskBytes)
	free, err := s.freeBytes(s.options.Directory)
	if err != nil {
		return err
	}
	if free < s.options.MinimumFreeBytes || uint64(max(int64(0), needed-allocated)) > free-s.options.MinimumFreeBytes {
		return failure("storage_capacity_unavailable")
	}
	if _, err = s.command(ctx, "fallocate", "--length", strconv.FormatInt(needed, 10), "--", s.image(r)); err != nil {
		return err
	}
	_, _, size, allocated, err = imageIdentity(f)
	if err != nil || size != needed || allocated < needed {
		return errors.Join(failure("storage_allocation_unconfirmed"), err)
	}
	if err = f.Sync(); err != nil {
		return err
	}
	for _, path := range []string{s.directory(r), s.options.Directory} {
		dir, e := os.Open(path)
		if e != nil {
			return e
		}
		if e = errors.Join(dir.Sync(), dir.Close()); e != nil {
			return e
		}
	}
	p.Stage = "copying"
	return n.saveCopy(p)
}

func (n *Runtime) installCopy(ctx context.Context, s *FileStorage, r Resource, p *copyRecord) (result error) {
	f, err := copyFile(s, r, p)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, f.Close()) }()
	// 此文件直到落盘 installed 才允许挂载。离线工具永远只操作目标副本。
	check := func() error {
		out, err := s.run(ctx, "fsck", "-f", "-p", s.image(r))
		if err != nil {
			return err
		}
		if out.exitCode > 1 {
			return failure("disk_copy_fsck_failed")
		}
		return nil
	}
	if err = check(); err != nil {
		return err
	}
	target := p.Target.Resources.DiskBytes
	if target != p.Source.Resources.DiskBytes {
		if _, err = s.command(ctx, "resizefs", s.image(r), strconv.FormatInt(target/1024, 10)+"K"); err != nil {
			return failure("disk_copy_resize_failed")
		}
	}
	if err = verifyExt4(f, filesystemUUID(r)); err != nil {
		if _, err = s.command(ctx, "tune", "-U", filesystemUUID(r), s.image(r)); err != nil {
			return failure("disk_copy_uuid_failed")
		}
	}
	if err = f.Truncate(target); err != nil {
		return err
	}
	if err = check(); err != nil {
		return err
	}
	if _, err = s.command(ctx, "fallocate", "--length", strconv.FormatInt(target, 10), "--", s.image(r)); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	disk := initialStorageRecord(r)
	disk.Phase = "ready"
	disk.DesktopProfiles = true
	disk.ImageDevice = p.Device
	disk.ImageInode = p.Inode
	if err = s.verifyImage(r, disk, true); err != nil {
		return err
	}
	if err = s.save(disk); err != nil {
		return err
	}
	p.Stage = "installed"
	return n.saveCopy(p)
}
