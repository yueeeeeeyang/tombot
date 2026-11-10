package tools

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"time"

	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/policy"
	"io.yueyang/tom/internal/store"
	"io.yueyang/tom/internal/term"
	"io.yueyang/tom/internal/workspace"
)

func shellDefinition() definition {
	return definition{
		description: "Run one shell command in your workspace or shared/. Timeout defaults to 120 seconds, maximum 600. Use env_secrets [{name, secret_id}] for secret environment injection; never put secret values in commands. Output is redacted. Privileged commands are denied by default. This is a real host shell, not a filesystem sandbox.",
		properties: map[string]any{
			"command":         map[string]any{"type": "string", "maxLength": 4096},
			"cwd":             map[string]string{"type": "string"},
			"timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 600},
			"env_secrets":     map[string]any{"type": "array", "maxItems": 16, "items": map[string]any{"type": "object", "properties": map[string]any{"name": map[string]string{"type": "string"}, "secret_id": map[string]string{"type": "string"}}, "required": []string{"name", "secret_id"}, "additionalProperties": false}},
		}, required: []string{"command"},
	}
}
func (e *Executor) shellAction(bot domain.Bot, input term.ShellInput) (policy.Action, error) {
	absolute, err := e.files.Directory(bot.ID, input.Cwd)
	if err != nil {
		return policy.Action{}, err
	}
	target, _, err := e.files.Describe(bot.ID, absolute)
	if err != nil {
		return policy.Action{}, err
	}
	input.Cwd = target
	if input.TimeoutSeconds == 0 {
		input.TimeoutSeconds = int(term.DefaultShellTimeout / time.Second)
	}
	sort.Slice(input.EnvSecrets, func(i, j int) bool { return input.EnvSecrets[i].Name < input.EnvSecrets[j].Name })
	encoded, err := json.Marshal(input)
	if err != nil {
		return policy.Action{}, err
	}
	digest := sha256.Sum256(encoded)
	class := policy.ShellClass(input.Command)
	risk := "medium"
	if class == "shell.privileged" {
		risk = "high"
	} else if len(input.EnvSecrets) > 0 {
		class = "secret.use"
		risk = "high"
	}
	names := make([]string, 0, len(input.EnvSecrets))
	for _, ref := range input.EnvSecrets {
		names = append(names, ref.Name)
	}
	return policy.Action{Class: class, Pattern: "sha256:" + hex.EncodeToString(digest[:]), Risk: risk, Title: "执行 Shell 命令", Details: map[string]any{"tool": "computer_shell", "command": input.Command, "cwd": target, "timeout_seconds": input.TimeoutSeconds, "environment_names": names}}, nil
}

func (e *Executor) invokeShell(ctx context.Context, run domain.Run, step int, callID string, input term.ShellInput, audit workspace.BeforeWrite) (any, error) {
	cwd, err := e.files.Directory(run.BotID, input.Cwd)
	if err != nil {
		return nil, err
	}
	home, err := e.files.CommandHome(run.BotID)
	if err != nil {
		return nil, err
	}
	environment := map[string][]byte{}
	var result term.ShellResult
	var inject func(int) error
	inject = func(index int) error {
		if index == len(input.EnvSecrets) {
			digest := sha256.Sum256([]byte(input.Command))
			if err := store.Transact(ctx, e.db, func(tx *sql.Tx) error {
				return store.Audit(ctx, tx, "bot:"+run.BotID, "shell.exec", callID, map[string]any{"run_id": run.ID, "cwd": input.Cwd, "command_sha256": hex.EncodeToString(digest[:]), "secret_count": len(input.EnvSecrets)})
			}); err != nil {
				return err
			}
			var runErr error
			result, runErr = e.shell.Run(ctx, input, cwd, home, environment)
			return runErr
		}
		ref := input.EnvSecrets[index]
		return e.secrets.Use(ctx, ref.SecretID, "bot:"+run.BotID, run.ID, func(value []byte) error {
			environment[ref.Name] = value
			defer delete(environment, ref.Name)
			return inject(index + 1)
		})
	}
	err = inject(0)
	defer clear(result.Output)
	// JSON 转义也计入结果容量；预留文件路径和状态字段，保留尾部而不丢失退出码。
	for {
		encoded, encodeErr := json.Marshal(result)
		if encodeErr != nil {
			return result, encodeErr
		}
		if len(encoded) <= term.TailBytes-512 {
			break
		}
		tail := []rune(result.StdoutTail)
		if len(tail) == 0 {
			break
		}
		result.StdoutTail = string(tail[max(1, (len(encoded)-(term.TailBytes-512))/6):])
		result.Truncated = true
	}
	if result.Truncated {
		// 进程已经退出；独立短上下文允许取消时仍保存已脱敏的输出，文件落盘失败必须上抛。
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		written, saveErr := e.files.SaveShellOutput(cleanup, run.BotID, run.ID+"-"+strconv.Itoa(step)+"-"+callID, string(result.Output), audit)
		if saveErr != nil {
			return result, saveErr
		}
		result.OutputPath = written.Path
		if _, publishErr := e.events.Publish(cleanup, "artifact.created", run.ConversationID, map[string]string{"run_id": run.ID, "bot_id": run.BotID, "path": written.Path, "kind": "file"}); publishErr != nil {
			return result, publishErr
		}
	}
	clear(result.Output)
	result.Output = nil
	return result, err
}
