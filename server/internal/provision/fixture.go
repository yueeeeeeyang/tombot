package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
	"unicode/utf8"
)

// FixturePath 返回测试夹具路径。未设置时产品走完整引导，会安装字体并准备浏览器。
func FixturePath() string {
	return os.Getenv("TOM_BOOTSTRAP_FIXTURE")
}

type fixtureFile struct {
	Steps []fixtureSpec `json:"steps"`
}

type fixtureSpec struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Phase     string `json:"phase"`
	Blocking  *bool  `json:"blocking,omitempty"`
	FailFirst bool   `json:"fail_first,omitempty"`
	Detail    string `json:"detail,omitempty"`
	DelayMS   int    `json:"delay_ms,omitempty"`
}

// LoadFixture 把测试夹具变成引导步骤。步骤没有安装函数，因此失败会停住，
// 下一次执行才通过；这样客户端能看到失败和重试，又不会下载浏览器。
func LoadFixture(path string) ([]Step, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("无法读取引导夹具: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var file fixtureFile
	if err = decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("引导夹具格式不正确: %w", err)
	}
	if err = decoder.Decode(new(struct{})); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("引导夹具含有多余内容")
	}
	if len(file.Steps) == 0 {
		return nil, fmt.Errorf("引导夹具至少要有一个步骤")
	}
	seen := map[string]bool{}
	steps := make([]Step, 0, len(file.Steps))
	for _, spec := range file.Steps {
		if err = spec.validate(); err != nil {
			return nil, err
		}
		if seen[spec.ID] {
			return nil, fmt.Errorf("引导夹具步骤重复: %s", spec.ID)
		}
		seen[spec.ID] = true
		steps = append(steps, spec.step())
	}
	return steps, nil
}

func (spec fixtureSpec) validate() error {
	if spec.ID == "" || utf8.RuneCountInString(spec.ID) > 64 {
		return fmt.Errorf("引导夹具步骤标识无效")
	}
	if spec.Title == "" || utf8.RuneCountInString(spec.Title) > 80 {
		return fmt.Errorf("引导夹具步骤标题无效: %s", spec.ID)
	}
	switch spec.Phase {
	case "detecting", "preparing", "selfcheck":
	default:
		return fmt.Errorf("引导夹具阶段无效: %s", spec.ID)
	}
	if spec.DelayMS < 0 || spec.DelayMS > 30_000 {
		return fmt.Errorf("引导夹具等待时间无效: %s", spec.ID)
	}
	if spec.FailFirst && spec.Detail == "" {
		return fmt.Errorf("引导夹具的失败步骤需要说明原因: %s", spec.ID)
	}
	if utf8.RuneCountInString(spec.Detail) > 400 {
		return fmt.Errorf("引导夹具失败说明过长: %s", spec.ID)
	}
	return nil
}

func (spec fixtureSpec) step() Step {
	attempts := 0
	blocking := true
	if spec.Blocking != nil {
		blocking = *spec.Blocking
	}
	return Step{
		ID: spec.ID, Title: spec.Title, Phase: spec.Phase, Blocking: blocking,
		Verify: func(ctx context.Context) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if spec.DelayMS > 0 {
				timer := time.NewTimer(time.Duration(spec.DelayMS) * time.Millisecond)
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
				}
			}
			attempts++
			// 第一次只失败、不安装。重试会再次验证，这时才通过。
			if spec.FailFirst && attempts == 1 {
				return errors.New(spec.Detail)
			}
			return nil
		},
	}
}
