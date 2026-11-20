package accounts

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"io"
	"io.yueyang/tom/internal/computer"
	"io.yueyang/tom/internal/infra/ids"
	"io.yueyang/tom/internal/testkit/pgtest"
	"log/slog"
	"testing"
)

type editRuntime struct {
	*testRuntime
	disk []string
	fail bool
}

func (r *editRuntime) CheckEditSupport(context.Context, computer.NodeQuery) error {
	if r.fail {
		return errors.New("节点离线")
	}
	return nil
}
func (r *editRuntime) CopyDisk(_ context.Context, q computer.DiskCopy) (computer.DiskReceipt, error) {
	r.disk = append(r.disk, q.Action)
	v := computer.DiskReceipt{EditID: q.EditID}
	if r.fail {
		return v, &computer.Failure{Code: "disk_copy_test_failure"}
	}
	switch q.Action {
	case "read":
		v.Offset = q.Offset
		b := make([]byte, min(int64(computer.ZeroChunkBytes), q.Source.Resources.DiskBytes-q.Offset))
		v.Chunk = &computer.DiskChunk{Size: len(b), SHA256: computer.DiskDigest(b)}
	case "write":
		v.Offset = q.Offset + int64(q.Chunk.Size)
	case "install":
		o := observation(q.Target, "ready")
		o.ContainerID = computer.DiskDigest([]byte(q.EditID))
		v.Observation = &o
	}
	return v, nil
}
func (r *editRuntime) UpgradeComputer(_ context.Context, q computer.ImageUpgrade) (computer.Observation, error) {
	r.disk = append(r.disk, "retained")
	o := observation(q.Target, "ready")
	o.ContainerID = computer.DiskDigest([]byte(q.Target.NodeID))
	return o, q.Validate()
}
func TestEditDisabledAccountAndMoveComputer(t *testing.T) {
	ctx := t.Context()
	f := newFixture(t, pgtest.Start(t), true)
	o := f.reserve(t)
	f.process(t)
	user := o.UserID
	execute(t, f.pool, `UPDATE tombot.users SET status='active' WHERE id=$1`, user)
	if _, err := f.service.UserSettings(ctx, f.actor, user); !errors.Is(err, ErrEditConflict) {
		t.Fatal("启用账户允许编辑", err)
	}
	var revision int64
	if err := f.pool.QueryRow("SELECT revision FROM tombot.users WHERE id=$1", user).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if err := f.service.SetUserEnabled(ctx, f.actor, user, ids.New(), false, revision); err != nil {
		t.Fatal(err)
	}
	get := func() UserSettings {
		v, err := f.service.UserSettings(ctx, f.actor, user)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	edit := func(v UserSettings) UserEdit {
		return UserEdit{Revision: v.Revision, Generation: v.Generation, DisplayName: v.DisplayName, Limits: v.Limits, Computer: v.Computer}
	}
	original := get()
	req := edit(original)
	req.DisplayName = " 新名称 "
	req.Limits.Desktops = 4
	tokens := int64(9007199254740993)
	req.Limits.Monthly = &tokens
	var picture bytes.Buffer
	if err := png.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	req.Avatar = picture.Bytes()
	save := func(req UserEdit) (UserSettings, error) {
		return f.service.EditUser(ctx, f.actor, user, ids.New(), req)
	}
	v, err := save(req)
	if err != nil {
		t.Fatal("保存资料失败", err)
	}
	if !v.HasAvatar || v.Username != original.Username || v.DisplayName != "新名称" || v.Limits.Desktops != 4 || *v.Limits.Monthly != tokens || v.Progress != nil {
		t.Fatal("资料或额度不一致")
	}
	if _, err = save(req); !errors.Is(err, ErrEditConflict) {
		t.Fatal("旧页面覆盖成功", err)
	}
	req = edit(v)
	req.RemoveAvatar = true
	req.Limits.Monthly = nil
	req.Limits.Models = 0
	if _, err = save(req); !errors.Is(err, ErrInvalidInput) || !get().HasAvatar {
		t.Fatal("错误额度导致部分写入", err)
	}
	req.Limits.Models = 2
	req.Computer.Resources.CPUMillis += 500
	rt := &editRuntime{testRuntime: f.runtime}
	f.service.runtime = rt
	rt.fail = true
	if _, err = save(req); !errors.Is(err, ErrEditNodeUnavailable) || get().Progress != nil || !get().HasAvatar {
		t.Fatal("协议检查失败却修改了用户或电脑", err)
	}
	rt.fail = false
	v, err = save(req)
	if err != nil || v.Progress == nil || v.HasAvatar || v.Limits.Monthly != nil {
		t.Fatal("同节点变配失败", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	process := func() {
		t.Helper()
		if work, err := f.service.processEdit(ctx, logger); err != nil || !work {
			t.Fatal("迁移未完成", work, err)
		}
	}
	process()
	v = get()
	if v.Generation != 2 || v.Computer.Resources.CPUMillis != req.Computer.Resources.CPUMillis || v.Progress != nil || len(rt.disk) != 1 || rt.disk[0] != "retained" {
		t.Fatal("同节点变配未复用原盘", v, rt.disk)
	}
	destination := ids.New()
	execute(t, f.pool, `INSERT INTO tombot.computer_nodes(id,name,status,capacity,runtime_id,control_sequence,last_seen_at,control_expires_at,verified_image_id,capability_expires_at) VALUES($1,$1,'ready',1,$1,1,clock_timestamp(),clock_timestamp()+interval '5 minutes',$2,clock_timestamp()+interval '15 minutes')`, destination, f.imageID)
	req = edit(v)
	req.Computer.NodeID = destination
	req.Computer.Resources.DiskBytes *= 2
	v, err = save(req)
	if err != nil || v.Progress == nil || v.Computer.NodeID != f.nodeID {
		t.Fatal("过早切换节点", err)
	}
	if err = f.service.SetUserEnabled(ctx, f.actor, user, ids.New(), true, v.Revision); !errors.Is(err, ErrEditConflict) {
		t.Fatal("迁移期间开放了用户", err)
	}
	rt.fail = true
	process()
	v = get()
	if v.Progress.Failure == nil || v.Computer.NodeID != f.nodeID {
		t.Fatal("失败后丢失源归属")
	}
	for range maxEditAttempts - 1 {
		execute(t, f.pool, "UPDATE tombot.computer_edits SET next_attempt_at=clock_timestamp() WHERE user_id=$1", user)
		process()
	}
	v = get()
	if !v.Progress.Paused {
		t.Fatal("超过重试上限仍未暂停")
	}
	if work, err := f.service.processEdit(ctx, logger); err != nil || work {
		t.Fatal("领取了暂停的迁移", err)
	}
	retry := edit(v)
	retry.Computer = v.Progress.Target
	if _, err = save(retry); err != nil {
		t.Fatal("原计划无法重试", err)
	}
	rt.fail = false
	process()
	process()
	if v = get(); v.Progress.Copied != computer.ZeroChunkBytes {
		t.Fatal("游标未提交", v)
	}
	// 快进已验证的中间块，在真实数据库重领最后一块，确保切换在节点最终回执之后。
	execute(t, f.pool, "UPDATE tombot.computer_edits SET copied=$2 WHERE user_id=$1", user, v.Progress.Total-1)
	process()
	process()
	v = get()
	if v.Progress != nil || v.Generation != 3 || v.Computer != req.Computer {
		t.Fatal("新配置未生效", v)
	}
	for node, want := range map[string]int{f.nodeID: 0, destination: 1} {
		var got int
		if err = f.pool.QueryRow("SELECT reserved FROM tombot.computer_nodes WHERE id=$1", node).Scan(&got); err != nil || got != want {
			t.Fatal("容量预留不符", got, err)
		}
	}
	if err = f.service.SetUserEnabled(ctx, f.actor, user, ids.New(), true, v.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err = save(edit(v)); !errors.Is(err, ErrEditConflict) {
		t.Fatal("账户已启用仍允许写入", err)
	}
}
