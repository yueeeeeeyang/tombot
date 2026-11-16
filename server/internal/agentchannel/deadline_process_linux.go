//go:build linux

package agentchannel

import (
	"errors"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type processClock struct {
	namespace uint64
	offsetNS  int64
}

// FromDeadlineForProcess 把已有期限换算到已核验目标进程的时间命名空间。
// 调用方须在换算前后复验进程归属；本函数固定 proc 目录，拒绝退出、命名空间漂移和数值溢出。
// 不进入目标命名空间、不重置租期，也不改变接收方的命名空间与过期校验。
func FromDeadlineForProcess(deadline time.Time, pid int) (result Deadline, failure error) {
	if pid <= 0 {
		return result, errors.New("电脑代理目标时钟进程无效")
	}
	source, err := openProcessClock(os.Getpid())
	if err != nil {
		return result, err
	}
	defer func() { failure = errors.Join(failure, source.Close()) }()
	target, err := openProcessClock(pid)
	if err != nil {
		return result, err
	}
	defer func() { failure = errors.Join(failure, target.Close()) }()
	sourceClock, err := readProcessClock(source)
	if err != nil {
		return result, err
	}
	targetClock, err := readProcessClock(target)
	if err != nil {
		return result, err
	}
	stamp, err := FromDeadline(deadline)
	if err != nil {
		return result, err
	}
	sourceAfter, err := readProcessClock(source)
	if err != nil {
		return result, err
	}
	targetAfter, err := readProcessClock(target)
	if err != nil || sourceAfter != sourceClock || targetAfter != targetClock || stamp.TimeNamespace != sourceClock.namespace {
		return result, errors.Join(errors.New("电脑代理换算期间时间命名空间已变化"), err)
	}
	return translateDeadline(stamp, sourceClock.offsetNS, targetClock)
}

func openProcessClock(pid int) (*os.File, error) {
	fd, err := unix.Open("/proc/"+strconv.Itoa(pid), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("打开电脑代理目标进程时钟失败")
	}
	file := os.NewFile(uintptr(fd), "process-clock")
	var filesystem unix.Statfs_t
	if unix.Fstatfs(fd, &filesystem) != nil || filesystem.Type != unix.PROC_SUPER_MAGIC {
		return nil, errors.Join(errors.New("电脑代理目标时钟不属于进程文件系统"), file.Close())
	}
	return file, nil
}

func readTimeNamespace(process *os.File, name string) (namespace uint64, failure error) {
	fd, err := unix.Openat(int(process.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, errors.New("读取电脑代理目标时间命名空间失败")
	}
	defer func() { failure = errors.Join(failure, unix.Close(fd)) }()
	var stat unix.Stat_t
	var filesystem unix.Statfs_t
	kind, err := unix.IoctlRetInt(fd, unix.NS_GET_NSTYPE)
	if err != nil || kind != unix.CLONE_NEWTIME || unix.Fstatfs(fd, &filesystem) != nil || filesystem.Type != unix.NSFS_MAGIC || unix.Fstat(fd, &stat) != nil || stat.Ino == 0 {
		return 0, errors.New("电脑代理目标时间命名空间类型无效")
	}
	return stat.Ino, nil
}

func readProcessClock(process *os.File) (clock processClock, failure error) {
	namespace, err := readTimeNamespace(process, "ns/time")
	if err != nil {
		return clock, err
	}
	// timens_offsets 属于子进程将进入的命名空间；两者不一致时不能把它当作当前进程偏移。
	children, err := readTimeNamespace(process, "ns/time_for_children")
	if err != nil || children != namespace {
		return clock, errors.Join(errors.New("电脑代理目标进程正在切换时间命名空间"), err)
	}
	fd, err := unix.Openat(int(process.Fd()), "timens_offsets", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return clock, errors.New("读取电脑代理时间命名空间偏移失败")
	}
	file := os.NewFile(uintptr(fd), "clock-offsets")
	defer func() { failure = errors.Join(failure, file.Close()) }()
	encoded, err := io.ReadAll(io.LimitReader(file, 1025))
	if err != nil || len(encoded) > 1024 {
		return clock, errors.New("电脑代理时间命名空间偏移无法读取或超限")
	}
	offset, err := parseClockOffsets(string(encoded))
	if err != nil {
		return clock, err
	}
	return processClock{namespace: namespace, offsetNS: offset}, nil
}

func parseClockOffsets(encoded string) (int64, error) {
	seen := map[string]bool{}
	var monotonic int64
	for _, line := range strings.Split(strings.TrimSpace(encoded), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[0] != "monotonic" && fields[0] != "boottime" || seen[fields[0]] {
			return 0, errors.New("电脑代理时间命名空间偏移格式无效")
		}
		seconds, err := strconv.ParseInt(fields[1], 10, 64)
		nanoseconds, nanoErr := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || nanoErr != nil || seconds < math.MinInt64/int64(time.Second) || seconds > math.MaxInt64/int64(time.Second) || nanoseconds < 0 || nanoseconds >= int64(time.Second) || seconds*int64(time.Second) > math.MaxInt64-nanoseconds {
			return 0, errors.New("电脑代理时间命名空间偏移数值无效")
		}
		seen[fields[0]] = true
		if fields[0] == "monotonic" {
			monotonic = seconds*int64(time.Second) + nanoseconds
		}
	}
	if len(seen) != 2 {
		return 0, errors.New("电脑代理时间命名空间偏移缺失")
	}
	return monotonic, nil
}

func translateDeadline(stamp Deadline, sourceOffset int64, target processClock) (Deadline, error) {
	// 先还原到初始命名空间，再加目标偏移；分步检查，不能让整数回绕产生更长的执行权。
	if stamp.ExpiresNS <= 0 || stamp.TimeNamespace == 0 || target.namespace == 0 || sourceOffset < 0 && stamp.ExpiresNS > math.MaxInt64+sourceOffset {
		return Deadline{}, errors.New("电脑代理源时钟期限无法换算")
	}
	initial := stamp.ExpiresNS - sourceOffset
	if initial <= 0 || target.offsetNS > 0 && initial > math.MaxInt64-target.offsetNS || target.offsetNS < 0 && target.offsetNS <= -initial {
		return Deadline{}, errors.New("电脑代理目标时钟期限无法换算")
	}
	return Deadline{ExpiresNS: initial + target.offsetNS, TimeNamespace: target.namespace}, nil
}
