package memory

import (
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"io.yueyang/tom/internal/domain"
)

type Query struct {
	Terms   []string
	Match   string
	IsCJK   bool
	IsShort bool
}

// ParseQuery 把用户文字转成转义后的词项，不允许输入 FTS 操作符；短中文用有界字面匹配。
func ParseQuery(text string) (Query, error) {
	query := Query{Terms: strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })}
	if !utf8.ValidString(text) || len(text) > 512 || len(query.Terms) == 0 || len(query.Terms) > 16 {
		return query, domain.NewError(400, "MEMORY_QUERY_INVALID", "Use 1 to 16 search terms within 512 UTF-8 bytes.")
	}
	quoted := []string{}
	for _, term := range query.Terms {
		cjk := false
		for _, r := range term {
			if unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) {
				query.IsCJK = true
				cjk = true
			}
		}
		if cjk && utf8.RuneCountInString(term) < 3 {
			query.IsShort = true
		}
		quoted = append(quoted, `"`+strings.ReplaceAll(term, `"`, `""`)+`"`)
	}
	if query.IsCJK {
		for _, term := range query.Terms {
			if utf8.RuneCountInString(term) < 3 {
				query.IsShort = true
			}
		}
	}
	query.Match = strings.Join(quoted, " AND ")
	return query, nil
}

// Score 将 SQLite 的负 bm25 转成正相关度，重要性加权后按更新时间作三十天半衰。
func Score(bm25 float64, importance int, updated, now time.Time) float64 {
	age := max(0, now.Sub(updated).Hours()/24)
	return max(-bm25, 1e-12) * (1 + 0.2*float64(importance)) * math.Exp2(-age/30)
}
