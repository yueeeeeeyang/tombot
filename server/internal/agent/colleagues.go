package agent

import (
	"context"
	"encoding/json"
	"strings"

	"io.yueyang/tom/internal/provider"
)

const maxColleagues = 64
const maxColleagueBytes = 8192

// colleagues 只公开可联系成员的身份与职责，按人数和字节限制提示词开销；角色指令和记忆不出私有域。
func (r *Runtime) colleagues(ctx context.Context, j *job, tools []provider.ToolSchema) (string, error) {
	if !j.bot.MessagingPolicy.CanInitiate {
		return "", nil
	}
	enabled := false
	for _, tool := range tools {
		if tool.Name == "message_bot" || tool.Name == "delegate_task" {
			enabled = true
			break
		}
	}
	if !enabled {
		return "", nil
	}
	rows, err := r.deps.DB.QueryContext(ctx, `SELECT id,name,role_title FROM bots WHERE id!=? AND is_archived=0 AND json_extract(messaging_policy_json,'$.accept_dm')=1 ORDER BY id LIMIT ?`, j.bot.ID, maxColleagues+1)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var out strings.Builder
	out.WriteString("\n\nColleagues (public identity only; use bot_id for messaging, subject to policy and approval):\n")
	count := 0
	for rows.Next() {
		var colleague struct {
			ID   string `json:"bot_id"`
			Name string `json:"name"`
			Role string `json:"role"`
		}
		if err = rows.Scan(&colleague.ID, &colleague.Name, &colleague.Role); err != nil {
			return "", err
		}
		encoded, err := json.Marshal(colleague)
		if err != nil {
			return "", err
		}
		if count == maxColleagues || out.Len()+len(encoded) > maxColleagueBytes {
			out.WriteString("The directory was truncated to fit the context budget. Ask the user for a specific bot ID if the intended colleague is absent.\n")
			break
		}
		out.Write(encoded)
		out.WriteByte('\n')
		count++
	}
	return out.String(), rows.Err()
}
