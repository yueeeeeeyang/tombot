package domain

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ResolveGroupMentions 识别完整成员名；同名必须由客户端显式携带标识，避免投递给错误对象。
func ResolveGroupMentions(text string, members []GroupMember, preferred ...string) ([]string, error) {
	names := map[string][]string{"用户": {"user"}}
	for _, member := range members {
		if !member.IsArchived {
			names[member.Name] = append(names[member.Name], member.BotID)
		}
	}
	ordered := []string{}
	for name := range names {
		ordered = append(ordered, name)
	}
	slices.SortFunc(ordered, func(a, b string) int { return len(b) - len(a) })
	result := []string{}
	for index := 0; index < len(text); index++ {
		if text[index] != '@' {
			continue
		}
		if index > 0 {
			prior, _ := utf8.DecodeLastRuneInString(text[:index])
			// 中文正文通常不以空格分词；仅排除邮箱地址中的 ASCII 前缀。
			if prior <= unicode.MaxASCII && (unicode.IsLetter(prior) || unicode.IsDigit(prior) || strings.ContainsRune("._%+-", prior)) {
				continue
			}
		}
		matched := false
		for _, name := range ordered {
			end := index + 1 + len(name)
			if !strings.HasPrefix(text[index+1:], name) {
				continue
			}
			if end < len(text) {
				next, _ := utf8.DecodeRuneInString(text[end:])
				if !unicode.IsSpace(next) && !unicode.IsPunct(next) && next != '@' {
					continue
				}
			}
			choices := names[name]
			if len(choices) > 1 {
				chosen := []string{}
				for _, id := range choices {
					if slices.Contains(preferred, id) {
						chosen = append(chosen, id)
					}
				}
				if len(chosen) == 1 {
					choices = chosen
				}
			}
			if len(choices) != 1 {
				return nil, NewError(400, "AMBIGUOUS_GROUP_MENTION", "Select the intended member from the mention menu; this name is shared by multiple members.")
			}
			if !slices.Contains(result, choices[0]) {
				result = append(result, choices[0])
			}
			matched = true
			index = end - 1
			break
		}
		if !matched && index+1 < len(text) {
			next, _ := utf8.DecodeRuneInString(text[index+1:])
			if unicode.IsLetter(next) {
				return nil, NewError(400, "INVALID_GROUP_MENTION", "Choose an active group member from the mention menu.")
			}
		}

	}
	return result, nil
}
