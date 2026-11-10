package term

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/creack/pty"
	"io.yueyang/tom/internal/domain"
)

const DefaultSessionLimit = 32
const DefaultOwnerSessionLimit = 8
const DefaultSessionBufferBytes = 1 << 20
const MaxSessionInputBytes = 16 << 10
const MaxSessionReadBytes = 64 << 10
const sessionWriteTimeout = 2 * time.Second
const sessionDrainTimeout = time.Second

type SessionOptions struct {
	ID      string
	Owner   string
	Cwd     string
	Home    string
	Columns int
	Rows    int
}

type SessionOutput struct {
	Data      string `json:"data"`
	Cursor    int64  `json:"cursor"`
	Truncated bool   `json:"truncated"`
	HasMore   bool   `json:"has_more"`
	Exited    bool   `json:"exited"`
	ExitCode  int    `json:"exit_code"`
	ErrorCode string `json:"error_code,omitempty"`
}

// Manager 管理根上下文内的 PTY；请求取消和客户端断连不会关闭已创建的会话。
// 元数据、归属校验和保留期限交由终端业务服务处理。
type Manager struct {
	root             context.Context
	mu               sync.Mutex
	sessions         map[string]*Session
	closed           bool
	maximumSessions  int
	maximumPerOwner  int
	bufferBytes      int
	terminationGrace time.Duration
}

func NewManager(root context.Context) *Manager {
	return &Manager{root: root, sessions: map[string]*Session{}, maximumSessions: DefaultSessionLimit, maximumPerOwner: DefaultOwnerSessionLimit, bufferBytes: DefaultSessionBufferBytes, terminationGrace: 5 * time.Second}
}

// Session 对写入和尺寸变更串行化；输出通过独立游标读取，多个观察者互不消耗输出。
type Session struct {
	owner            string
	lifetime         context.Context
	master           *os.File
	command          *exec.Cmd
	terminationGrace time.Duration
	stop             context.CancelFunc
	done             chan struct{}
	input            chan struct{}
	mu               sync.Mutex
	output           outputBuffer
	changed          chan struct{}
	exited           bool
	exitCode         int
	errorCode        string
	failure          error
}

func ValidateSize(columns, rows int) error {
	if columns < 2 || columns > 500 || rows < 2 || rows > 300 {
		return domain.NewError(400, "INVALID_TERMINAL_SIZE", "Terminal size must be 2 to 500 columns and 2 to 300 rows.")
	}
	return nil
}

// Open 启动独立会话的交互式 Shell；失败不保留会话名或配额。
func (m *Manager) Open(ctx context.Context, options SessionOptions) (*Session, error) {
	if err := ValidateSize(options.Columns, options.Rows); err != nil {
		return nil, err
	}
	if options.ID == "" || options.Owner == "" || !filepath.IsAbs(options.Cwd) || !filepath.IsAbs(options.Home) {
		return nil, domain.NewError(400, "INVALID_TERMINAL", "Provide a terminal ID, owner and absolute workspace directories.")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.closed || m.root.Err() != nil {
		return nil, domain.NewError(503, "TERMINALS_STOPPING", "The terminal manager is stopping.")
	}
	if _, exists := m.sessions[options.ID]; exists {
		return nil, domain.NewError(409, "TERMINAL_EXISTS", "A terminal with this ID already exists.")
	}
	owned := 0
	for _, session := range m.sessions {
		if session.owner == options.Owner {
			owned++
		}
	}
	if len(m.sessions) >= m.maximumSessions || owned >= m.maximumPerOwner {
		return nil, domain.NewError(429, "TERMINAL_LIMIT", "Close an existing terminal before opening another.")
	}
	shell, err := exec.LookPath("bash")
	// 保留交互式行编辑，但禁止历史展开意外改写模型输入；历史仅留在进程内。
	arguments := []string{"--noprofile", "--norc", "-i", "+H"}
	if errors.Is(err, exec.ErrNotFound) {
		shell, err = exec.LookPath("sh")
		arguments = []string{"-i"}
	}
	if err != nil {
		return nil, domain.NewError(503, "SHELL_UNAVAILABLE", "No supported shell is installed.")
	}
	command := exec.Command(shell, arguments...)
	command.Dir = options.Cwd
	command.Env = []string{"HOME=" + options.Home, "TMPDIR=" + filepath.Join(options.Home, ".tom", "tmp"), "TERM=xterm-256color", "HISTFILE=/dev/null"}
	for _, name := range []string{"PATH", "LANG", "LC_ALL"} {
		if value, exists := os.LookupEnv(name); exists {
			command.Env = append(command.Env, name+"="+value)
		}
	}
	master, err := pty.StartWithSize(command, &pty.Winsize{Cols: uint16(options.Columns), Rows: uint16(options.Rows)})
	if err != nil {
		return nil, domain.NewError(500, "TERMINAL_START_FAILED", "The terminal could not be started. Check its workspace and executable permissions.")
	}
	master, err = pollablePTY(master)
	if err != nil {
		killErr := signalSession(command.Process.Pid, syscall.SIGKILL)
		waitErr := command.Wait()
		var exit *exec.ExitError
		if errors.As(waitErr, &exit) {
			waitErr = nil
		}
		return nil, errors.Join(err, killErr, waitErr)
	}
	lifetime, stop := context.WithCancel(m.root)
	session := &Session{owner: options.Owner, lifetime: lifetime, master: master, command: command, terminationGrace: m.terminationGrace, stop: stop, done: make(chan struct{}), input: make(chan struct{}, 1), output: newOutputBuffer(m.bufferBytes), changed: make(chan struct{}), exitCode: -1}
	m.sessions[options.ID] = session
	go session.run(lifetime)
	return session, nil
}

func (m *Manager) Get(id string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, exists := m.sessions[id]
	if !exists {
		return nil, domain.NewError(404, "TERMINAL_NOT_RUNNING", "This terminal is no longer available. Terminal processes are not restored after a restart.")
	}
	return session, nil
}

// Remove 先回收进程，再释放输出缓冲与配额；等待超时后仍可再次调用确认关闭结果。
func (m *Manager) Remove(ctx context.Context, id string) error {
	m.mu.Lock()
	session := m.sessions[id]
	m.mu.Unlock()
	if session == nil {
		return nil
	}
	if err := session.Close(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions[id] == session {
		delete(m.sessions, id)
	}
	return nil
}

// Close 同时通知全部会话停止，避免关闭耗时随会话数累加；错误包含未完成的回收。
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	sessions := make([]*Session, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
		session.stop()
	}
	m.mu.Unlock()
	var failures error
	for _, session := range sessions {
		failures = errors.Join(failures, session.Close(ctx))
	}
	return failures
}

func (s *Session) notifyLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *Session) readOutput() error {
	chunk := make([]byte, 8192)
	pending := make([]byte, 0, len(chunk)+utf8.UTFMax)
	for {
		count, err := s.master.Read(chunk)
		pending = append(pending, chunk[:count]...)
		complete := 0
		for complete < len(pending) && utf8.FullRune(pending[complete:]) {
			_, size := utf8.DecodeRune(pending[complete:])
			complete += size
		}
		if err != nil {
			complete = len(pending)
		}
		if complete > 0 {
			clean := strings.ToValidUTF8(string(pending[:complete]), "�")
			s.mu.Lock()
			s.output.append([]byte(clean))
			s.notifyLocked()
			s.mu.Unlock()
			pending = append(pending[:0], pending[complete:]...)
		}
		if err != nil {
			// Linux 在最后一个从端关闭后返回 EIO；macOS 通常返回 EOF。
			if errors.Is(err, io.EOF) || errors.Is(err, syscall.EIO) || errors.Is(err, os.ErrClosed) {
				return nil
			}
			return err
		}
	}
}

func (s *Session) run(ctx context.Context) {
	defer s.stop()
	readDone := make(chan error, 1)
	waitDone := make(chan error, 1)
	go func() { readDone <- s.readOutput() }()
	go func() { waitDone <- s.command.Wait() }()
	var failure, waitErr, readErr error
	readFinished := false
	select {
	case waitErr = <-waitDone:
	case <-ctx.Done():
		failure = s.terminate(waitDone, &waitErr)
	case readErr = <-readDone:
		readFinished = true
		// 主端异常结束后不留下无人管理的 Shell；同样回收后台作业。
		failure = s.terminate(waitDone, &waitErr)
	}
	failure = errors.Join(failure, signalSession(s.command.Process.Pid, syscall.SIGKILL))
	if !readFinished {
		timer := time.NewTimer(sessionDrainTimeout)
		select {
		case readErr = <-readDone:
			readFinished = true
		case <-timer.C:
		}
		timer.Stop()
	}
	failure = errors.Join(failure, s.master.Close())
	if !readFinished {
		readErr = <-readDone
	}
	var exit *exec.ExitError
	if errors.As(waitErr, &exit) {
		waitErr = nil
	}
	failure = errors.Join(failure, readErr, waitErr)
	s.mu.Lock()
	s.exited, s.exitCode, s.failure = true, s.command.ProcessState.ExitCode(), failure
	if failure != nil {
		s.errorCode = "TERMINAL_IO_FAILED"
	}
	s.notifyLocked()
	close(s.done)
	s.mu.Unlock()
}

func (s *Session) terminate(waitDone <-chan error, waitErr *error) error {
	failure := signalSession(s.command.Process.Pid, syscall.SIGTERM)
	timer := time.NewTimer(s.terminationGrace)
	defer timer.Stop()
	select {
	case *waitErr = <-waitDone:
	case <-timer.C:
		failure = errors.Join(failure, signalSession(s.command.Process.Pid, syscall.SIGKILL))
		*waitErr = <-waitDone
	}
	return failure
}

// Read 从独立字节游标返回完整 UTF-8 分片；wait 为真时最多等待调用上下文的期限。
func (s *Session) Read(ctx context.Context, cursor int64, limit int, wait bool) (SessionOutput, error) {
	if cursor < 0 || limit < utf8.UTFMax || limit > MaxSessionReadBytes {
		return SessionOutput{}, domain.NewError(400, "INVALID_TERMINAL_CURSOR", "Use a nonnegative output cursor and a read limit between 4 and 65536 bytes.")
	}
	for {
		if err := ctx.Err(); err != nil {
			return SessionOutput{}, err
		}
		s.mu.Lock()
		if cursor > s.output.end {
			s.mu.Unlock()
			return SessionOutput{}, domain.NewError(409, "TERMINAL_CURSOR_AHEAD", "The output cursor exceeds the terminal output. Reset the cursor to zero.")
		}
		text, next, truncated := s.output.read(cursor, limit)
		result := SessionOutput{Data: text, Cursor: next, Truncated: truncated, HasMore: next < s.output.end, Exited: s.exited, ExitCode: s.exitCode, ErrorCode: s.errorCode}
		changed := s.changed
		s.mu.Unlock()
		if !wait || text != "" || result.Exited {
			return result, nil
		}
		select {
		case <-ctx.Done():
			return SessionOutput{}, ctx.Err()
		case <-changed:
		}
	}
}

func (s *Session) acquire(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case s.input <- struct{}{}:
	}
	if err := ctx.Err(); err != nil {
		<-s.input
		return err
	}
	s.mu.Lock()
	exited := s.exited || s.lifetime.Err() != nil
	s.mu.Unlock()
	if exited {
		<-s.input
		return domain.NewError(409, "TERMINAL_EXITED", "The terminal process has exited. Open a new terminal.")
	}
	return nil
}

// Write 串行写入且限定等待时间；失败可能已写入前缀，调用方不得自动重试输入。
func (s *Session) Write(ctx context.Context, text string) error {
	if err := ValidateSessionInput(text); err != nil {
		return err
	}
	operation, cancel := context.WithTimeout(ctx, sessionWriteTimeout)
	defer cancel()
	if err := s.acquire(operation); err != nil {
		return err
	}
	defer func() { <-s.input }()
	deadline, _ := operation.Deadline()
	if err := s.master.SetWriteDeadline(deadline); err != nil {
		return err
	}
	// 主动取消也唤醒阻塞写入；等待回调结束后才能让下一位写入者设置自己的期限。
	canceled := make(chan struct{})
	var cancelErr error
	stopCancel := context.AfterFunc(operation, func() {
		cancelErr = s.master.SetWriteDeadline(time.Now())
		if cancelErr != nil && !errors.Is(cancelErr, os.ErrClosed) {
			s.stop()
		}
		close(canceled)
	})
	count, err := io.WriteString(s.master, text)
	if !stopCancel() {
		<-canceled
	}
	// 写期限和上下文期限是同一时刻。内核先返回超时时，上下文可能还没被标记完成，调用方就认不出取消。
	if err != nil && errors.Is(err, os.ErrDeadlineExceeded) {
		<-operation.Done()
	}
	if err != nil || cancelErr != nil {
		return errors.Join(err, cancelErr, operation.Err())
	}
	if count != len(text) {
		return io.ErrShortWrite
	}
	return nil
}

func ValidateSessionInput(text string) error {
	if text == "" || len(text) > MaxSessionInputBytes || !utf8.ValidString(text) {
		return domain.NewError(400, "INVALID_TERMINAL_INPUT", "Provide 1 to 16384 bytes of UTF-8 terminal input.")
	}
	return nil
}

func (s *Session) Resize(ctx context.Context, columns, rows int) error {
	if err := ValidateSize(columns, rows); err != nil {
		return err
	}
	operation, cancel := context.WithTimeout(ctx, sessionWriteTimeout)
	defer cancel()
	if err := s.acquire(operation); err != nil {
		return err
	}
	defer func() { <-s.input }()
	return resizePTY(s.master, columns, rows)
}

func (s *Session) Close(ctx context.Context) error {
	s.stop()
	return s.Wait(ctx)
}

// Wait 等待进程和读端完成回收，不影响会话生命周期。
func (s *Session) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.failure
	}
}
