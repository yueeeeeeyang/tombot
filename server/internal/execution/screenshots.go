package execution

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"io.yueyang/tom/internal/botagent"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/provider"
)

// ImageOptions 限制服务端临时视觉观察的空间与上下文开销，不限制用户电脑中的业务文件。
type ImageOptions struct {
	MaxUserBytes  int64
	MaxUserImages int
	ContextImages int
	Retention     time.Duration
}

func (options ImageOptions) Validate() error {
	if options.MaxUserBytes < domain.MaxVisionImageBytes || options.MaxUserBytes > 4<<30 || options.MaxUserImages < 1 || options.MaxUserImages > 4096 || options.ContextImages < 1 || options.ContextImages > 4 || options.Retention < 48*time.Hour || options.Retention > 30*24*time.Hour {
		return ErrInvalidInput
	}
	return nil
}

func validateOperationScreenshot(lease Lease, id string, result OperationResult) error {
	var returned botagent.ToolResult
	if json.Unmarshal(result.Result, &returned) != nil {
		return ErrInvalidInput
	}
	ref := result.Screenshot
	if (ref == nil) != (returned.Artifact == nil) {
		return ErrInvalidInput
	}
	if ref == nil {
		return nil
	}
	if result.Status != "succeeded" || ref.ID != id || ref.BotID != lease.BotID || (botagent.ToolResult{Artifact: ref}).Validate(0) != nil || botagent.ValidateScreenshotPixels(ref) != nil || returned.Validate(24<<10) != nil {
		return ErrInvalidInput
	}
	left, err := json.Marshal(ref)
	right, other := json.Marshal(returned.Artifact)
	if err != nil || other != nil || string(left) != string(right) {
		return ErrInvalidInput
	}
	return nil
}

// saveScreenshot 与操作终态共用事务；用户级事务锁保证并发 Bot 不超额写入。
func (service *Service) saveScreenshot(ctx context.Context, transaction *sql.Tx, lease Lease, operation Operation, ref *domain.ArtifactRef) error {
	if operation.Kind != "computer" {
		return ErrInvalidInput
	}
	if _, err := transaction.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('tombot.screenshots.'||$1,0))`, lease.UserID); err != nil {
		return err
	}
	var count int
	var size int64
	if err := transaction.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(octet_length(content)),0) FROM tombot.run_screenshots WHERE user_id=$1`, lease.UserID).Scan(&count, &size); err != nil {
		return err
	}
	if count >= service.options.Images.MaxUserImages || size+int64(ref.Bytes) > service.options.Images.MaxUserBytes {
		return ErrImageCapacity
	}
	digest, err := hex.DecodeString(ref.SHA256)
	if err != nil {
		return ErrInvalidInput
	}
	_, err = transaction.ExecContext(ctx, `INSERT INTO tombot.run_screenshots(user_id,id,run_id,bot_id,fencing_token,attempt,sha256,content,expires_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,clock_timestamp()+$9::bigint*interval '1 microsecond')`, lease.UserID, operation.ID, lease.RunID, lease.BotID, operation.FencingToken, operation.Attempt, digest, ref.Content, service.options.Images.Retention.Microseconds())
	return err
}

// ResolveScreenshots 只装配当前运行签发且已持久提交的截图；最近若干张附带像素，旧引用保留文字身份。
// 返回独立上下文，不修改 Agent 提交的轨迹或把二进制放入后续控制帧。
func (service *Service) ResolveScreenshots(ctx context.Context, lease Lease, input modelgateway.Input) (modelgateway.Input, error) {
	output := input
	output.Messages = append([]provider.Message(nil), input.Messages...)
	refs := map[string][]*domain.ArtifactRef{}
	var order []string
	for messageIndex, message := range input.Messages {
		output.Messages[messageIndex].Blocks = append([]domain.MessageBlock(nil), message.Blocks...)
		for blockIndex, block := range message.Blocks {
			if block.Artifact != nil {
				return modelgateway.Input{}, ErrInvalidInput
			}
			if block.ToolResult == nil || block.ToolResult.Artifact == nil {
				continue
			}
			result := *block.ToolResult
			ref := *result.Artifact
			if ref.BotID != lease.BotID || (botagent.ToolResult{Artifact: &ref, IsError: result.IsError}).Validate(0) != nil || len(ref.Content) != 0 {
				return modelgateway.Input{}, ErrInvalidInput
			}
			result.Artifact = &ref
			output.Messages[messageIndex].Blocks[blockIndex].ToolResult = &result
			if _, exists := refs[ref.ID]; !exists {
				order = append(order, ref.ID)
			}
			refs[ref.ID] = append(refs[ref.ID], &ref)
		}
	}
	if len(order) == 0 {
		return output, nil
	}
	if len(order) > service.options.MaxOperations {
		return modelgateway.Input{}, ErrOperationLimit
	}
	selected := order[max(0, len(order)-service.options.Images.ContextImages):]
	err := service.worker(ctx, lease, false, func(transaction *sql.Tx) (failure error) {
		rows, err := transaction.QueryContext(ctx, `SELECT s.id,s.bot_id,encode(s.sha256,'hex'),octet_length(s.content),CASE WHEN s.id=ANY($4::text[]) THEN s.content ELSE NULL END
			FROM tombot.run_screenshots s JOIN tombot.run_operations o ON o.user_id=s.user_id AND o.id=s.id
			WHERE s.user_id=$1 AND s.run_id=$2 AND s.id=ANY($3::text[]) AND o.status='succeeded' AND o.kind='computer'`, lease.UserID, lease.RunID, order, selected)
		if err != nil {
			return err
		}
		defer func() { failure = errors.Join(failure, rows.Close()) }()
		found := 0
		for rows.Next() {
			var id, botID, digest string
			var size int
			var content []byte
			if err := rows.Scan(&id, &botID, &digest, &size, &content); err != nil {
				return err
			}
			for _, ref := range refs[id] {
				if ref.BotID != botID || ref.SHA256 != digest || ref.Bytes != size {
					return ErrConflict
				}
				ref.Content = content
				if len(content) > 0 && botagent.ValidateScreenshotPixels(ref) != nil {
					return ErrInvalidInput
				}
			}
			found++
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if found != len(order) {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		return modelgateway.Input{}, err
	}
	return output, nil
}

// PruneScreenshots 分批回收已结束任务的过期视觉观察，跳过其他清理者持有的行；运行及操作审计保留。
func (service *Service) PruneScreenshots(ctx context.Context, limit int) (int64, error) {
	if limit < 1 || limit > 1000 {
		return 0, ErrInvalidInput
	}
	var count int64
	err := service.dispatch.Platform(ctx, func(transaction *sql.Tx) error {
		result, err := transaction.ExecContext(ctx, `WITH expired AS (
			SELECT s.user_id,s.id FROM tombot.run_screenshots s JOIN tombot.jobs j ON j.user_id=s.user_id AND j.run_id=s.run_id
			WHERE s.expires_at<clock_timestamp() AND j.status='finished' ORDER BY s.expires_at,s.user_id,s.id LIMIT $1 FOR UPDATE OF s SKIP LOCKED)
			DELETE FROM tombot.run_screenshots s USING expired e WHERE s.user_id=e.user_id AND s.id=e.id`, limit)
		if err != nil {
			return err
		}
		count, err = result.RowsAffected()
		return err
	})
	return count, translate(err)
}
