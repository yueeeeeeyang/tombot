package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/infra/bus"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/term"
	"io.yueyang/tom/internal/testkit"
	"io.yueyang/tom/internal/workspace"
)

func terminalServiceFixture(t *testing.T) (*Terminals, domain.Bot, domain.Run) {
	t.Helper()
	db := testkit.Database(t)
	events := bus.New(db)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "shared"), 0700); err != nil {
		t.Fatal(err)
	}
	bots := NewBots(db, events, root)
	bot, err := bots.Create(context.Background(), BotInput{Template: "writer"}, "user")
	if err != nil {
		t.Fatal(err)
	}
	files, err := workspace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := files.Close(); err != nil {
			t.Error(err)
		}
	})
	service, err := NewTerminals(context.Background(), db, events, bots, files, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := service.Shutdown(ctx); err != nil {
			t.Error("终端服务关闭失败", err)
		}
	})
	run := domain.Run{ID: ids.New(), TurnID: ids.New(), ConversationID: bot.ConversationID, BotID: bot.ID}
	if _, err = db.Exec(`INSERT INTO turns(id,conversation_id,seq,source,status,created_at) VALUES(?,?,1,'user','running',0)`, run.TurnID, run.ConversationID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO runs(id,turn_id,conversation_id,bot_id,source,status,model_id,prompt,created_at) VALUES(?,?,?,?,'chat','running','fixture','Use the terminal.',0)`, run.ID, run.TurnID, run.ConversationID, run.BotID); err != nil {
		t.Fatal(err)
	}
	return service, bot, run
}

func waitTerminalStatus(t *testing.T, service *Terminals, id, status string) Terminal {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	for {
		wake := service.events.Wake()
		terminal, err := service.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if terminal.Status == status {
			return terminal
		}
		select {
		case <-wake:
		case <-ctx.Done():
			t.Fatal("终端状态未完成", status)
		}
	}
}

func TestTerminalServiceOwnershipTakeoverAndIncrementalRead(t *testing.T) {
	t.Parallel()
	s, bot, run := terminalServiceFixture(t)
	ctx := context.Background()
	terminal, err := s.Open(ctx, TerminalOpenInput{}, &run, "bot:"+bot.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(terminal.Name, "tom-bot-"+bot.ID[:8]+"-") || terminal.Controller != "bot" || terminal.RunID != run.ID {
		t.Fatal("会话身份不符合契约")
	}
	other := run
	other.BotID = "other"
	if _, err = s.ReadBot(ctx, terminal.ID, other, 1024); err == nil {
		t.Fatal("其他 Bot 可以读取终端")
	}
	if err = s.Write(ctx, terminal.ID, "true\r", nil, 0, "user"); err == nil {
		t.Fatal("用户没有接管仍能输入")
	}
	oldRevision := terminal.Revision
	terminal, err = s.SetController(ctx, terminal.ID, "user", "user")
	if err != nil || terminal.RunID != "" {
		t.Fatal("接管失败或仍绑定回收时限", err)
	}
	terminal, err = s.SetController(ctx, terminal.ID, "bot", "user")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Write(ctx, terminal.ID, "false\r", &run, oldRevision, "bot:"+bot.ID); err == nil {
		t.Fatal("接管后旧审批版本仍可执行")
	}
	if err = s.Write(ctx, terminal.ID, "printf 'result%s' 'complete'; exit 9\r", &run, terminal.Revision, "bot:"+bot.ID); err != nil {
		t.Fatal(err)
	}
	terminal = waitTerminalStatus(t, s, terminal.ID, "exited")
	if terminal.ExitCode == nil || *terminal.ExitCode != 9 {
		t.Fatal("退出码未保存")
	}
	output, err := s.ReadBot(ctx, terminal.ID, run, 4096)
	if err != nil || !strings.Contains(output.Data, "resultcomplete") || !output.Exited {
		t.Fatal("模型读取缺少结果", err)
	}
	again, err := s.ReadBot(ctx, terminal.ID, run, 4096)
	if err != nil || again.Data != "" || again.Cursor != output.Cursor {
		t.Fatal("模型游标没有递增", err)
	}
	replay, err := s.Output(ctx, terminal.ID, 0, 4096, false)
	if err != nil || replay.Data != output.Data {
		t.Fatal("用户观察者消耗了模型游标", err)
	}
	if err = s.Close(ctx, terminal.ID, &run, terminal.Revision, "bot:"+bot.ID); err != nil {
		t.Fatal("模型不能关闭已退出会话", err)
	}
	terminal = waitTerminalStatus(t, s, terminal.ID, "closed")
	if terminal.ExitCode == nil || *terminal.ExitCode != 9 {
		t.Fatal("关闭丢失退出码")
	}
	if _, err = s.manager.Get(terminal.ID); err == nil {
		t.Fatal("关闭仍保留输出缓冲")
	}
}

func TestTerminalServiceUserPathsValidationAndAudit(t *testing.T) {
	t.Parallel()
	s, bot, run := terminalServiceFixture(t)
	ctx := context.Background()
	for _, input := range []TerminalOpenInput{{Cwd: "../outside"}, {Cwd: "/tmp"}, {Columns: 501}, {Title: "含\n换行"}, {BotID: "missing"}} {
		if _, err := s.Open(ctx, input, nil, "user"); err == nil {
			t.Fatal("无效终端参数被接受")
		}
	}
	if _, err := s.Open(ctx, TerminalOpenInput{BotID: "another"}, &run, "bot"); err == nil {
		t.Fatal("模型可创建其他 Bot 会话")
	}
	terminal, err := s.Open(ctx, TerminalOpenInput{Title: "共享终端"}, nil, "user")
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Owner != "user" || filepath.Base(terminal.Cwd) != "shared" || terminal.BotID != "" {
		t.Fatal("用户工作区不正确")
	}
	if _, err = s.SetController(ctx, terminal.ID, "bot", "user"); err == nil {
		t.Fatal("没有关联 Bot 的终端可交还控制")
	}
	if err = s.Resize(ctx, terminal.ID, 121, 41, nil, 0, "user"); err != nil {
		t.Fatal(err)
	}
	if err = s.Write(ctx, terminal.ID, "exit 0\r", nil, 0, "user"); err != nil {
		t.Fatal(err)
	}
	waitTerminalStatus(t, s, terminal.ID, "exited")
	if err = s.Close(ctx, terminal.ID, nil, 0, "user"); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(ctx, terminal.ID, nil, 0, "user"); err != nil {
		t.Fatal("重复关闭不是幂等操作", err)
	}
	var details string
	if err = s.db.QueryRow(`SELECT detail_json FROM audit_log WHERE action='terminal.input' AND target=?`, terminal.ID).Scan(&details); err != nil || strings.Contains(details, "exit") || !strings.Contains(details, "bytes") {
		t.Fatal("输入审计记录了原始命令或缺少元数据", err)
	}
	items, err := s.List(ctx, bot.ID, "", 50)
	if err != nil || len(items) != 0 {
		t.Fatal("Bot 列表错误地包含共享终端", err)
	}
	items, err = s.List(ctx, "", "", 50)
	if err != nil || len(items) != 1 || items[0].Columns != 121 || items[0].Rows != 41 {
		t.Fatal("列表或尺寸持久化错误", err)
	}
	items, err = s.List(ctx, "", terminal.ID, 50)
	if err != nil || len(items) != 0 {
		t.Fatal("分页游标重复", err)
	}
}

func TestTerminalServiceExpiryAndBotDeletionGuard(t *testing.T) {
	t.Parallel()
	s, bot, run := terminalServiceFixture(t)
	ctx := context.Background()
	terminal, err := s.Open(ctx, TerminalOpenInput{}, &run, "bot")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE runs SET status='succeeded',finished_at=? WHERE id=?`, time.Now().Add(-TerminalRetention-time.Minute).UnixMilli(), run.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.bots.Delete(ctx, bot.ID, bot.Name, "user", false); err == nil {
		t.Fatal("删除仍拥有终端的 Bot")
	}
	if _, err = s.Output(ctx, terminal.ID, 0, 1024, false); err == nil {
		t.Fatal("保留期限届满仍接受访问")
	}
	if err = s.cleanupExpired(ctx); err != nil {
		t.Fatal(err)
	}
	waitTerminalStatus(t, s, terminal.ID, "closed")
	if err = s.bots.Delete(ctx, bot.ID, bot.Name, "user", false); err != nil {
		t.Fatal("关闭后不能删除 Bot", err)
	}
	var count int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM terminal_sessions WHERE id=?`, terminal.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("Bot 删除未清理终端元数据", err)
	}
}

func TestTerminalServiceRestartMarksUnavailable(t *testing.T) {
	t.Parallel()
	s, bot, _ := terminalServiceFixture(t)
	ctx := context.Background()
	terminal, err := s.Open(ctx, TerminalOpenInput{BotID: bot.ID}, nil, "user")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Write(ctx, terminal.ID, "exit 0\r", nil, 0, "user"); err != nil {
		t.Fatal(err)
	}
	waitTerminalStatus(t, s, terminal.ID, "exited")
	shutdown, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err = s.Shutdown(shutdown); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewTerminals(ctx, s.db, s.events, s.bots, s.files, s.logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := restarted.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	terminal, err = restarted.Get(ctx, terminal.ID)
	if err != nil || terminal.Status != "interrupted" || terminal.ErrorCode != "TERMINAL_INTERRUPTED" {
		t.Fatal("重启没有标记终端中断", err)
	}
	if _, err = restarted.Output(ctx, terminal.ID, 0, 1024, false); err == nil {
		t.Fatal("重启后伪造可恢复输出")
	}
	if err = restarted.Close(ctx, terminal.ID, nil, 0, "user"); err != nil {
		t.Fatal("无法删除中断记录", err)
	}
}

func TestTerminalServiceAuditFailureAndInactiveRunPreventEffects(t *testing.T) {
	t.Parallel()
	s, _, run := terminalServiceFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`CREATE TRIGGER reject_terminal_audit BEFORE INSERT ON audit_log WHEN NEW.action='terminal.open' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(ctx, TerminalOpenInput{}, nil, "user"); err == nil {
		t.Fatal("审计失败仍创建终端")
	}
	if len(s.entries) != 0 {
		t.Fatal("审计失败仍创建进程")
	}
	if _, err := s.db.Exec(`DROP TRIGGER reject_terminal_audit`); err != nil {
		t.Fatal(err)
	}
	terminal, err := s.Open(ctx, TerminalOpenInput{}, &run, "bot")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE runs SET status='canceled',finished_at=0 WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.Write(ctx, terminal.ID, "touch forbidden\r", &run, terminal.Revision, "bot"); err == nil {
		t.Fatal("取消的 Run 仍可输入")
	}
	if _, err = os.Stat(filepath.Join(terminal.Cwd, "forbidden")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("失败操作产生了副作用", err)
	}
	if _, err = s.ReadBot(ctx, terminal.ID, run, term.MaxSessionReadBytes); err == nil {
		t.Fatal("取消的 Run 仍可读取并延长保留期限")
	}
	if err = s.Close(ctx, terminal.ID, nil, 0, "user"); err != nil {
		t.Fatal(err)
	}
}

func TestTerminalExitPersistenceFailureIsVisibleAndCanBeClosed(t *testing.T) {
	t.Parallel()
	s, _, _ := terminalServiceFixture(t)
	ctx := context.Background()
	terminal, err := s.Open(ctx, TerminalOpenInput{}, nil, "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`CREATE TRIGGER reject_terminal_exit BEFORE UPDATE ON terminal_sessions WHEN NEW.status='exited' BEGIN SELECT RAISE(ABORT,'state unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if err = s.Write(ctx, terminal.ID, "exit 0\r", nil, 0, "user"); err != nil {
		t.Fatal(err)
	}
	entry, err := s.entry(terminal.ID)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for entry.check() == nil {
		if time.Now().After(deadline) {
			t.Fatal("退出状态保存失败未暴露")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err = s.Output(ctx, terminal.ID, 0, 1024, false); err == nil {
		t.Fatal("状态失败仍伪装正常输出")
	}
	if err = s.Close(ctx, terminal.ID, nil, 0, "user"); err != nil {
		t.Fatal("状态失败后无法显式关闭", err)
	}
}
