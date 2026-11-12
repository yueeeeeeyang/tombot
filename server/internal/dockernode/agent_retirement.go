package dockernode

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"io.yueyang/tom/internal/agentchannel"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/computeragent"
)

// controlRetirement 固定已关闭目录的内核身份；删除中断后仍能核验目录，不能改认同名替代目录。
type controlRetirement struct {
	Device uint64                 `json:"device"`
	Inode  uint64                 `json:"inode"`
	Next   computer.Specification `json:"next"`
}

// retireAgent 仅在用户生命周期独占锁内、下一代尚未发布时执行。最高代次和宿主关闭屏障始终保留。
func (node *Runtime) retireAgent(ctx context.Context, state *record, next computer.Specification) (result error) {
	resource := node.resource(state.Specification)
	if state.Phase != "absent" || state.ContainerID != "" {
		return failure("agent_retirement_not_closed")
	}
	if err := node.journal.requireClosedSessions(ctx, state.Specification.UserID); err != nil {
		return err
	}
	current, err := node.load(ctx, resource, state)
	if err != nil {
		return err
	}
	if !current.absent() {
		return failure("closed_generation_has_resources")
	}
	if err = node.browsers.retire(ctx, resource); err != nil {
		return err
	}
	if err = node.probes.retire(ctx, resource); err != nil {
		return err
	}
	if err = node.egress.stop(ctx, resource); err != nil {
		return err
	}
	if err = node.requireUnmountedControl(resource.ControlPath); err != nil {
		return err
	}
	name := filepath.Base(resource.ControlPath)
	stat, err := node.journal.root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		// 删除已完成但下一代记录未提交时，重复同步父目录后才能推进。
		return syncJournalDirectory(node.journal)
	}
	if err != nil {
		return err
	}
	identity, ok := stat.Sys().(*syscall.Stat_t)
	if !ok || !stat.IsDir() || identity.Uid != node.journal.owner || stat.Mode().Perm() != 0700 {
		return failure("agent_retirement_directory_unsafe")
	}
	retirement := controlRetirement{Device: uint64(identity.Dev), Inode: identity.Ino, Next: next}
	if state.Retirement != nil && (state.Retirement.Device != retirement.Device || state.Retirement.Inode != retirement.Inode) {
		return failure("agent_retirement_directory_replaced")
	}
	directory, err := node.agentDirectory(resource, false)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, directory.root.Close()) }()
	pinned, err := directory.root.Stat(".")
	if err != nil || !os.SameFile(stat, pinned) {
		return failure("agent_retirement_directory_replaced")
	}
	release, err := lockRetiredAgent(directory)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, release()) }()
	entries, err := retiredAgentEntries(ctx, directory)
	if err != nil {
		return err
	}
	if state.Retirement == nil {
		if err = node.verifyRetiredAgent(resource, directory, entries); err != nil {
			return err
		}
		state.Retirement = &retirement
		if err = node.journal.save(state); err != nil {
			return err
		}
	}
	for _, entry := range entries {
		if err = ctx.Err(); err != nil {
			return err
		}
		actual, err := directory.root.Lstat(entry.Name())
		if err != nil || !os.SameFile(entry, actual) || validateRetiredEntry(actual, directory.owner) != nil {
			return failure("agent_retirement_entry_replaced")
		}
		// 仅删除已核验的平面控制文件，不递归、不跟随链接，也不接触用户工作目录。
		if err = directory.root.Remove(entry.Name()); err != nil {
			return err
		}
	}
	if err = syncJournalDirectory(directory); err != nil {
		return err
	}
	if err = node.requireUnmountedControl(resource.ControlPath); err != nil {
		return err
	}
	actual, err := node.journal.root.Lstat(name)
	if err != nil || !os.SameFile(stat, actual) {
		return failure("agent_retirement_directory_replaced")
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = node.journal.root.Remove(name); err != nil {
		return err
	}
	return syncJournalDirectory(node.journal)
}

func syncJournalDirectory(journal *journal) error {
	directory, err := journal.root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func (node *Runtime) requireUnmountedControl(path string) error {
	if node.mounts == nil {
		return failure("agent_retirement_mount_check_missing")
	}
	mounts, err := node.mounts()
	if err != nil {
		return err
	}
	for _, mount := range mounts {
		if mount.target == path || strings.HasPrefix(mount.target, path+string(filepath.Separator)) {
			return failure("agent_retirement_directory_mounted")
		}
	}
	return nil
}

func lockRetiredAgent(directory *journal) (release func() error, result error) {
	files := make([]*os.File, 0, 2)
	closeFiles := func() error {
		var err error
		for _, file := range files {
			err = errors.Join(err, file.Close())
		}
		return err
	}
	defer func() {
		if release == nil {
			result = errors.Join(result, closeFiles())
		}
	}()
	for _, name := range []string{"agent.lock", computeragent.EgressSocketName + ".lock"} {
		file, err := directory.openFile(name, os.O_RDWR, 0)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		files = append(files, file)
		if err = directory.checkFile(file); err != nil {
			return nil, err
		}
		if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			return nil, failure("agent_retirement_directory_busy")
		}
	}
	return closeFiles, nil
}

func retiredAgentEntries(ctx context.Context, directory *journal) (entries []os.FileInfo, result error) {
	file, err := directory.root.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { result = errors.Join(result, file.Close()) }()
	// 除全部稳定桌面记录外，为配置、端点和崩溃临时记录预留有限余量。
	const maximumEntries = computer.MaxDesktops + 64
	for {
		batch, readErr := file.ReadDir(100)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, readErr
		}
		for _, entry := range batch {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if len(entries) >= maximumEntries {
				return nil, failure("agent_retirement_entry_limit")
			}
			stat, err := directory.root.Lstat(entry.Name())
			if err != nil {
				return nil, err
			}
			if err = validateRetiredEntry(stat, directory.owner); err != nil {
				return nil, err
			}
			entries = append(entries, stat)
		}
		if errors.Is(readErr, io.EOF) {
			return entries, nil
		}
	}
}

func validateRetiredEntry(stat os.FileInfo, owner uint32) error {
	name := stat.Name()
	identity, ok := stat.Sys().(*syscall.Stat_t)
	if !ok || identity.Uid != owner || identity.Nlink != 1 || stat.Mode().Perm() != 0600 {
		return failure("agent_retirement_entry_unsafe")
	}
	if name == computeragent.SocketName || name == computeragent.EgressSocketName {
		if stat.Mode()&os.ModeSocket == 0 {
			return failure("agent_retirement_socket_invalid")
		}
		return nil
	}
	known := name == computeragent.ConfigName || name == "agent.lock" || name == computeragent.EgressSocketName+".lock" || name == "probe.json" || name == "browser.json"
	known = known || strings.HasSuffix(name, ".session.json") && canonicalRecordID(strings.TrimSuffix(name, ".session.json"))
	known = known || strings.HasPrefix(name, ".write-") && canonicalRecordID(strings.TrimPrefix(name, ".write-"))
	known = known || strings.HasPrefix(name, ".session-") && strings.HasSuffix(name, ".tmp") && canonicalRecordID(strings.TrimSuffix(strings.TrimPrefix(name, ".session-"), ".tmp"))
	if !known || !stat.Mode().IsRegular() {
		return failure("agent_retirement_entry_unknown")
	}
	return nil
}

func (node *Runtime) verifyRetiredAgent(resource Resource, directory *journal, entries []os.FileInfo) error {
	encoded, err := directory.readFile(computeragent.ConfigName)
	if err != nil {
		return err
	}
	if encoded == nil {
		// 创建配置前崩溃只可能留下空目录或节点自己的未提交写入。
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), ".write-") && entry.Name() != computeragent.EgressSocketName+".lock" {
				return failure("agent_retirement_configuration_missing")
			}
		}
		return nil
	}
	var configuration computeragent.Configuration
	if agentchannel.Decode(encoded, &configuration) != nil || configuration.Validate() != nil || configuration.Identity != node.agentConfiguration(resource).Identity {
		return failure("agent_retirement_configuration_mismatch")
	}
	encodedBrowser, err := directory.readFile("browser.json")
	if err != nil {
		return err
	}
	if encodedBrowser != nil {
		var observed computeragent.BrowserSnapshot
		if agentchannel.Decode(encodedBrowser, &observed) != nil || observed.Validate() != nil || observed.Grant.Identity != configuration.Identity {
			return failure("agent_retirement_browser_invalid")
		}
		host, err := node.journal.readBrowser(resource.Specification.UserID)
		if err != nil {
			return err
		}
		if host == nil || host.Phase != "closed" || host.Grant.Identity != configuration.Identity || host.Grant.Revision < observed.Grant.Revision || host.Grant.Revision == observed.Grant.Revision && host.Grant != observed.Grant {
			return failure("agent_retirement_browser_unconfirmed")
		}
	}
	return nil
}
