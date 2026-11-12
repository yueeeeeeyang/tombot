package agent

import "encoding/json"

// groupSystem 只提供平等成员名册与当前消息，不附加协调者身份或分工。
func groupSystem(j *job) (string, error) {
	type member struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	members := []member{{ID: "user", Name: "用户"}}
	for _, cat := range j.group.Group.Members {
		if !cat.IsArchived {
			members = append(members, member{cat.BotID, cat.Name})
		}
	}
	encoded, err := json.Marshal(struct {
		ID      string   `json:"group_id"`
		Members []member `json:"members"`
	}{j.group.Group.ID, members})
	if err != nil {
		return "", err
	}
	return "\n\nGroup conversation\n" + string(encoded) + "\nAll cats are equal members; the user is the group administrator. Reply naturally to the current message, without inventing a coordinator or assignments. Only messages visible to you are supplied. Never infer or disclose messages addressed to other members. Do not copy group transcripts into private memory automatically. To address another member or the user, use post_to_group with mentions containing their exact IDs (user means the administrator), or write @ followed by their full name and a space in your final reply. Mentioned cats will receive that explicit message and may reply; do not claim they have replied before they do. Avoid repeated acknowledgements or mention loops. Group deliverables belong in shared/. Current message:\n" + j.brief, nil
}
