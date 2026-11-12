package execution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"sync"
	"testing"

	"io.yueyang/tom/internal/botagent"
	"io.yueyang/tom/internal/bots"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/modelgateway"
	"io.yueyang/tom/internal/provider"
	"io.yueyang/tom/internal/testkit/pgtest"
)

func screenshotPixels(t *testing.T) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 1280, 800))); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func screenshotReference(operation Operation, botID string, pixels []byte) *domain.ArtifactRef {
	digest := sha256.Sum256(pixels)
	return &domain.ArtifactRef{ID: operation.ID, BotID: botID, AttachmentInput: domain.AttachmentInput{SHA256: hex.EncodeToString(digest[:])}, Bytes: len(pixels), MediaType: "image/png", Width: 1280, Height: 800, Content: pixels}
}

func screenshotResult(t *testing.T, ref *domain.ArtifactRef) OperationResult {
	t.Helper()
	encoded, err := json.Marshal(botagent.ToolResult{Content: "Desktop screenshot.", Artifact: ref})
	if err != nil {
		t.Fatal(err)
	}
	return OperationResult{Status: "succeeded", Result: encoded, Screenshot: ref}
}

func screenshotContext(refs ...*domain.ArtifactRef) modelgateway.Input {
	input := modelgateway.Input{}
	for _, original := range refs {
		ref := *original
		ref.Content = nil
		input.Messages = append(input.Messages, provider.Message{Role: "user", Blocks: []domain.MessageBlock{{Kind: "tool_result", ToolResult: &domain.ToolResultBlock{ToolCallID: ref.ID, Content: "Desktop screenshot.", Artifact: &ref}}}})
	}
	return input
}

func (fixture fixture) desktopBot(t *testing.T, bot bots.Bot) bots.Bot {
	t.Helper()
	spec := bot.Spec
	spec.ToolPermissions = []string{"computer_screenshot", "computer_input"}
	updated, err := fixture.catalog.Update(context.Background(), fixture.a, bot.ID, bots.UpdateRequest{ExpectedRevision: bot.Revision, Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func (fixture fixture) screenshotOperation(t *testing.T, lease Lease, session computer.ExecutionSession, step int) Operation {
	t.Helper()
	key := fmt.Sprintf("tool.%d.1", step)
	digest := sha256.Sum256([]byte(key))
	operation, created, err := fixture.service.BeginOperation(context.Background(), lease, OperationRequest{StepKey: key, Kind: "computer", ToolName: "computer_screenshot", RequestDigest: digest[:], Computer: &session})
	if err != nil || !created || operation.Sequence < 1 {
		t.Fatal("截图操作未持久记账", err)
	}
	return operation
}

func TestScreenshotPersistence(t *testing.T) {
	cluster := pgtest.Start(t)
	ctx := context.Background()
	pixels := screenshotPixels(t)
	t.Run("原子保存与归属校验，模型仅装配最近截图", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		computers := fixture.computers(t, &sessionNode{}, computerOptions())
		fixture.send(t, fixture.a, fixture.desktopBot(t, fixture.first))
		lease := fixture.claim(t)
		fixture.start(t, lease)
		session, err := computers.Acquire(ctx, computerReference(lease))
		if err != nil {
			t.Fatal(err)
		}
		var refs []*domain.ArtifactRef
		var previous int64
		for step := 1; step <= 3; step++ {
			operation := fixture.screenshotOperation(t, lease, session, step)
			if operation.Sequence <= previous {
				t.Fatal("操作序号未递增")
			}
			previous = operation.Sequence
			ref := screenshotReference(operation, lease.BotID, pixels)
			result := screenshotResult(t, ref)
			for range 2 {
				if _, err := fixture.service.RecordOperation(ctx, lease, operation.ID, result); err != nil {
					t.Fatal("截图提交或重复确认失败", err)
				}
			}
			refs = append(refs, ref)
		}
		input := screenshotContext(refs...)
		resolved, err := fixture.service.ResolveScreenshots(ctx, lease, input)
		if err != nil {
			t.Fatal(err)
		}
		for index := range input.Messages {
			if len(input.Messages[index].Blocks[0].ToolResult.Artifact.Content) != 0 {
				t.Fatal("装配污染 Agent 轨迹")
			}
			loaded := resolved.Messages[index].Blocks[0].ToolResult.Artifact.Content
			if index == 0 && len(loaded) != 0 || index > 0 && !bytes.Equal(loaded, pixels) {
				t.Fatal("截图数量上限或真实像素错误")
			}
		}
		for _, change := range []func(*domain.ArtifactRef){func(ref *domain.ArtifactRef) { ref.BotID = fixture.second.ID }, func(ref *domain.ArtifactRef) { ref.Bytes++ }, func(ref *domain.ArtifactRef) { ref.Content = pixels }, func(ref *domain.ArtifactRef) { ref.ID = fixture.second.ID }} {
			altered := screenshotContext(refs[0])
			change(altered.Messages[0].Blocks[0].ToolResult.Artifact)
			if _, err := fixture.service.ResolveScreenshots(ctx, lease, altered); err == nil {
				t.Fatal("接受伪造截图引用")
			}
		}
		if err := fixture.tenant.Tenant(ctx, fixture.b.UserID, func(tx *sql.Tx) error {
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tombot.run_screenshots`).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return errors.New("跨用户读取了截图")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		for _, statement := range []string{`SELECT content FROM tombot.run_screenshots`, `UPDATE tombot.run_screenshots SET expires_at=expires_at+interval '1 hour'`} {
			if err := fixture.dispatch.Platform(ctx, func(tx *sql.Tx) error { _, err := tx.ExecContext(ctx, statement); return err }); err == nil {
				t.Fatal("调度权限可读取像素或改写截图")
			}
		}
		if _, err := fixture.pool.Exec(`UPDATE tombot.run_operations SET sequence=sequence+1 WHERE id=$1`, refs[0].ID); err == nil {
			t.Fatal("操作序号可改写")
		}
		// 同一 Bot 后续回合也不能借用旧运行的图片。
		fixture.completeModel(t, lease)
		if err := computers.Release(ctx, computerReference(lease)); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.service.Complete(ctx, lease, Outcome{Status: "succeeded", Answer: "完成"}); err != nil {
			t.Fatal(err)
		}
		fixture.send(t, fixture.a, fixture.first)
		next := fixture.claim(t)
		fixture.start(t, next)
		_, err = fixture.service.ResolveScreenshots(ctx, next, input)
		expect(t, err, ErrNotFound)
	})
	t.Run("并行 Bot 共用截图额度，失败事务不留下引用", func(t *testing.T) {
		fixture := newFixture(t, cluster)
		fixture.service.options.Images.MaxUserImages = 1
		computers := fixture.computers(t, &sessionNode{}, computerOptions())
		leases := make([]Lease, 2)
		operations := make([]Operation, 2)
		for index, bot := range []bots.Bot{fixture.first, fixture.second} {
			fixture.send(t, fixture.a, fixture.desktopBot(t, bot))
			leases[index] = fixture.claim(t)
			fixture.start(t, leases[index])
			session, err := computers.Acquire(ctx, computerReference(leases[index]))
			if err != nil {
				t.Fatal(err)
			}
			operations[index] = fixture.screenshotOperation(t, leases[index], session, 1)
		}
		bad := screenshotReference(operations[0], leases[0].BotID, pixels)
		bad.Content = bytes.Clone(pixels)
		bad.Content[0] ^= 1
		_, err := fixture.service.RecordOperation(ctx, leases[0], operations[0].ID, screenshotResult(t, bad))
		expect(t, err, ErrInvalidInput)
		var wait sync.WaitGroup
		failures := make([]error, 2)
		for index := range leases {
			result := screenshotResult(t, screenshotReference(operations[index], leases[index].BotID, pixels))
			wait.Go(func() {
				_, failures[index] = fixture.service.RecordOperation(ctx, leases[index], operations[index].ID, result)
			})
		}
		wait.Wait()
		var success, capacity int
		for index, err := range failures {
			if err == nil {
				success++
			} else if errors.Is(err, ErrImageCapacity) {
				capacity++
				var state string
				if err := fixture.pool.QueryRow(`SELECT status FROM tombot.run_operations WHERE id=$1`, operations[index].ID).Scan(&state); err != nil || state != "started" {
					t.Fatal("容量拒绝未回滚操作终态", state, err)
				}
			} else {
				t.Fatal(err)
			}
		}
		var count int
		if err := fixture.pool.QueryRow(`SELECT count(*) FROM tombot.run_screenshots`).Scan(&count); err != nil || count != 1 || success != 1 || capacity != 1 {
			t.Fatal("并发额度不一致", count, success, capacity, err)
		}
	})
}

func TestScreenshotRetention(t *testing.T) {
	fixture := newFixture(t, pgtest.Start(t))
	ctx := context.Background()
	computers := fixture.computers(t, &sessionNode{}, computerOptions())
	fixture.send(t, fixture.a, fixture.desktopBot(t, fixture.first))
	lease := fixture.claim(t)
	fixture.start(t, lease)
	session, err := computers.Acquire(ctx, computerReference(lease))
	if err != nil {
		t.Fatal(err)
	}
	for step := 1; step <= 2; step++ {
		operation := fixture.screenshotOperation(t, lease, session, step)
		ref := screenshotReference(operation, lease.BotID, screenshotPixels(t))
		// 直接准备已过期的数据库夹具；生产写入只能由有时限的截图事务完成。
		result := screenshotResult(t, ref)
		execute(t, fixture.pool, `INSERT INTO tombot.run_screenshots(user_id,id,run_id,bot_id,fencing_token,attempt,sha256,content,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,decode($7,'hex'),$8,clock_timestamp()-interval '3 days',clock_timestamp()-interval '1 day')`, lease.UserID, operation.ID, lease.RunID, lease.BotID, lease.FencingToken, lease.Attempt, ref.SHA256, ref.Content)
		execute(t, fixture.pool, `UPDATE tombot.run_operations SET status='succeeded',result=$2,finished_at=clock_timestamp() WHERE id=$1`, operation.ID, result.Result)
	}
	if count, err := fixture.service.PruneScreenshots(ctx, 1); err != nil || count != 0 {
		t.Fatal("运行中截图被回收", count, err)
	}
	fixture.completeModel(t, lease)
	if err := computers.Release(ctx, computerReference(lease)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.Complete(ctx, lease, Outcome{Status: "succeeded", Answer: "完成"}); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []int64{1, 1, 0} {
		if count, err := fixture.service.PruneScreenshots(ctx, 1); err != nil || count != expected {
			t.Fatal("过期截图未有界回收", count, err)
		}
	}
}
