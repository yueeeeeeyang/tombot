package execution

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"time"

	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/identity"
	"io.yueyang/tom/internal/infra/ids"
)

// Console 仅返回界面需要的控制状态，不暴露容器、节点、运行身份或桌面认证。
type Console struct {
	ID        string    `json:"id"`
	BotID     string    `json:"bot_id"`
	State     string    `json:"state"`
	ErrorCode string    `json:"error_code"`
	ExpiresAt time.Time `json:"expires_at"`
}
type consoleRecord struct {
	Console
	runID, sessionID, deviceID string
	revision                   int64
}

const consoleColumns = `id,bot_id,state,error_code,expires_at,run_id,COALESCE(computer_session_id,''),device_id,control_revision`

func scanConsole(row scanner) (record consoleRecord, err error) {
	err = row.Scan(&record.ID, &record.BotID, &record.State, &record.ErrorCode, &record.ExpiresAt, &record.runID, &record.sessionID, &record.deviceID, &record.revision)
	return
}

// OpenConsole 持久受理只读观看；观看不暂停任务，空闲 Bot 创建不调用模型的电脑会话。
func (service *Service) OpenConsole(ctx context.Context, principal identity.Principal, botID, requestID string) (Console, error) {
	if !validID(botID) || !validID(requestID) || !validID(principal.DeviceID) {
		return Console{}, ErrInvalidInput
	}
	var result consoleRecord
	err := service.user(ctx, principal, true, func(tx *sql.Tx) error {
		if err := lockDispatch(ctx, tx, principal.UserID); err != nil {
			return err
		}
		var err error
		result, err = scanConsole(tx.QueryRowContext(ctx, `SELECT `+consoleColumns+` FROM tombot.desktop_consoles WHERE user_id=$1 AND client_request_id=$2`, principal.UserID, requestID))
		if err == nil {
			if result.BotID != botID || result.deviceID != principal.DeviceID {
				return ErrConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var available bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.bots WHERE user_id=$1 AND id=$2 AND NOT is_archived)`, principal.UserID, botID).Scan(&available); err != nil {
			return err
		}
		if !available {
			return ErrNotFound
		}
		var active bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tombot.desktop_consoles WHERE user_id=$1 AND bot_id=$2 AND state IN ('opening','viewing','waiting','pausing','active','resuming'))`, principal.UserID, botID).Scan(&active); err != nil {
			return err
		}
		if active {
			return ErrConflict
		}
		// 优先接管真正持有桌面的运行，不能把正在等待同一桌面的后台任务当作当前画面。
		var runID string
		err = tx.QueryRowContext(ctx, `SELECT r.id FROM tombot.runs r LEFT JOIN tombot.computer_sessions s ON s.user_id=r.user_id AND s.run_id=r.id AND s.status='active' WHERE r.user_id=$1 AND r.bot_id=$2 AND r.accepts_console AND r.status IN ('queued','running') ORDER BY (s.id IS NOT NULL) DESC,r.created_at,r.id LIMIT 1 FOR UPDATE OF r`, principal.UserID, botID).Scan(&runID)
		if errors.Is(err, sql.ErrNoRows) {
			runID, err = service.createConsoleRun(ctx, tx, principal.UserID, botID, requestID)
		}
		if err != nil {
			return err
		}
		var revision int64
		if err = tx.QueryRowContext(ctx, `INSERT INTO tombot.desktop_console_heads(user_id,bot_id,revision) VALUES($1,$2,2) ON CONFLICT(user_id,bot_id) DO UPDATE SET revision=tombot.desktop_console_heads.revision+2 RETURNING revision`, principal.UserID, botID).Scan(&revision); err != nil {
			return err
		}
		result, err = scanConsole(tx.QueryRowContext(ctx, `INSERT INTO tombot.desktop_consoles(user_id,id,bot_id,device_id,client_request_id,run_id,control_revision,state) VALUES($1,$2,$3,$4,$5,$6,$7,'opening') RETURNING `+consoleColumns, principal.UserID, ids.New(), botID, principal.DeviceID, requestID, runID, revision))
		return err
	})
	return result.Console, err
}

func (service *Service) createConsoleRun(ctx context.Context, tx *sql.Tx, userID, botID, requestID string) (string, error) {
	var conversation string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM tombot.conversations WHERE user_id=$1 AND bot_id=$2 AND kind='direct' AND NOT is_archived FOR UPDATE`, userID, botID).Scan(&conversation); err != nil {
		return "", err
	}
	var maximum, count int
	if err := tx.QueryRowContext(ctx, `SELECT queued_runs,(SELECT count(*) FROM tombot.jobs WHERE user_id=$1 AND status='queued') FROM tombot.user_limits WHERE user_id=$1`, userID).Scan(&maximum, &count); err != nil {
		return "", err
	}
	if count >= maximum {
		return "", ErrCapacity
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx, `UPDATE tombot.conversations SET turn_sequence=turn_sequence+1 WHERE user_id=$1 AND id=$2 RETURNING turn_sequence`, userID, conversation).Scan(&sequence); err != nil {
		return "", err
	}
	turn, run := ids.New(), ids.New()
	digest := sha256.Sum256([]byte("desktop-console:" + botID + ":" + requestID))
	if _, err := tx.ExecContext(ctx, `INSERT INTO tombot.turns(user_id,id,conversation_id,sequence,client_request_id,request_digest,source,status) VALUES($1,$2,$3,$4,$5,$6,'console','queued')`, userID, turn, conversation, sequence, ids.New(), digest[:]); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO tombot.runs(user_id,id,turn_id,conversation_id,bot_id,bot_revision,bot_snapshot,status,progress_summary) SELECT user_id,$3,$4,$5,id,revision,'{}'::jsonb,'queued','正在等待用户操作电脑' FROM tombot.bots WHERE user_id=$1 AND id=$2`, userID, botID, run, turn, conversation); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO tombot.jobs(user_id,id,run_id,bot_id,conversation_id,turn_id,status) VALUES($1,$2,$3,$4,$5,$6,'queued')`, userID, ids.New(), run, botID, conversation, turn); err != nil {
		return "", err
	}
	return run, event(ctx, tx, userID, "run.queued", run, map[string]string{"bot_id": botID, "conversation_id": conversation, "turn_id": turn})
}

// ConsoleStatus 的心跳只延长当前设备的有效接管，总时长最多十分钟；终态不会被心跳复活。
func (service *Service) ConsoleStatus(ctx context.Context, principal identity.Principal, id string) (Console, error) {
	var record consoleRecord
	if !validID(id) {
		return Console{}, ErrInvalidInput
	}
	err := service.user(ctx, principal, true, func(tx *sql.Tx) error {
		var err error
		record, err = scanConsole(tx.QueryRowContext(ctx, `UPDATE tombot.desktop_consoles SET expires_at=CASE WHEN state IN ('opening','viewing','waiting','pausing','active') AND expires_at>clock_timestamp() THEN LEAST(clock_timestamp()+interval '30 seconds',created_at+interval '10 minutes') ELSE expires_at END WHERE user_id=$1 AND id=$2 AND device_id=$3 RETURNING `+consoleColumns, principal.UserID, id, principal.DeviceID))
		return err
	})
	return record.Console, err
}

// CloseConsole 先撤销用户输入，恢复 Bot 必须由工作器确认解除内核冻结后完成。
func (service *Service) CloseConsole(ctx context.Context, principal identity.Principal, id string) (Console, error) {
	var record consoleRecord
	if !validID(id) {
		return Console{}, ErrInvalidInput
	}
	err := service.user(ctx, principal, true, func(tx *sql.Tx) error {
		var err error
		record, err = scanConsole(tx.QueryRowContext(ctx, `UPDATE tombot.desktop_consoles SET return_to_view=false,state=CASE WHEN state IN ('opening','viewing','waiting') THEN 'closed' WHEN state IN ('pausing','active') THEN 'resuming' ELSE state END WHERE user_id=$1 AND id=$2 AND device_id=$3 RETURNING `+consoleColumns, principal.UserID, id, principal.DeviceID))
		return err
	})
	return record.Console, err
}

// consoleSession 将服务端已保存的桌面身份组装为节点协议，客户端不能选择 DISPLAY 或容器。
func consoleSession(ctx context.Context, tx *sql.Tx, userID string, record consoleRecord) (session computer.ExecutionSession, err error) {
	err = tx.QueryRowContext(ctx, `SELECT user_id,computer_id,bot_id,desktop_id,linux_uid,shared_gid,display_number,node_id,container_id,generation,id,run_id,fencing_token FROM tombot.computer_sessions WHERE user_id=$1 AND id=$2 AND run_id=$3 AND bot_id=$4 AND status='active' AND lease_expires_at>clock_timestamp()`, userID, record.sessionID, record.runID, record.BotID).Scan(&session.UserID, &session.ComputerID, &session.BotID, &session.DesktopID, &session.UID, &session.GID, &session.DisplayNumber, &session.NodeID, &session.ContainerID, &session.Generation, &session.SessionID, &session.RunID, &session.FencingToken)
	if err == nil {
		err = session.Validate()
	}
	return
}
