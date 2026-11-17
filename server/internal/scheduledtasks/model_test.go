package scheduledtasks

import (
	"testing"
	"time"
)

func TestTaskSchedules(t *testing.T) {
	now := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	base := Definition{Name: "提醒", Prompt: "提醒喝水", Cron: "0 9 * * *", Timezone: "Asia/Shanghai", IsEnabled: true}
	next, err := base.Next(now)
	if err != nil || !next.Equal(future) {
		t.Fatal("时区计算错误", next, err)
	}
	once := base
	once.Cron = ""
	once.RunAt = &future
	if next, err = once.Next(now); err != nil || !next.Equal(future) {
		t.Fatal("一次性任务错误", next, err)
	}
	for _, change := range []func(*Definition){
		func(d *Definition) { d.Cron = "* * * * * *" }, func(d *Definition) { d.Cron = "@every 1s" }, func(d *Definition) { d.Timezone = "Local" }, func(d *Definition) { d.Timezone = "Unknown/Zone" }, func(d *Definition) { d.RunAt = &future }, func(d *Definition) { d.Cron = "0 0 30 2 *" }, func(d *Definition) { d.Name = "" }, func(d *Definition) { d.Prompt = "\x00" },
	} {
		d := base
		change(&d)
		if _, err = d.Next(now); err != ErrInput {
			t.Fatal("接受了非法任务", err)
		}
	}
	if _, err = once.Next(future); err != ErrInput {
		t.Fatal("接受了过期时间")
	}
	once.IsEnabled = false
	if next, err = once.Next(future); err != nil || next != nil {
		t.Fatal("禁用的一次性任务不能保存", err)
	}
	// 夏令时缺失的墙钟时间不能执行两次或回退。
	base.Timezone = "America/New_York"
	base.Cron = "30 2 * * *"
	next, err = base.Next(time.Date(2026, 3, 8, 6, 0, 0, 0, time.UTC))
	if err != nil || next.Day() != 9 {
		t.Fatal("夏令时不存在的时间未跳过", next, err)
	}
}
