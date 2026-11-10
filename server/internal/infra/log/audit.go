package log

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// 数字串须完整匹配，避免截断普通业务标识；审计文本中的凭据和个人号码按同一规则处理。
var privateNumber = regexp.MustCompile(`\b(?:1[3-9][0-9]{9}|[0-9]{15,19}[Xx]?)\b`)
var privateAPIKey = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9_]{20,})`)

const MaxAuditDetails = 64 << 10

// SafeText 保留业务标识，清除可识别的设备凭据、请求鉴权、供应商令牌和个人号码。
func SafeText(value string) string {
	return privateNumber.ReplaceAllString(privateAPIKey.ReplaceAllString(redactText(value), "[已脱敏]"), "[已脱敏]")
}

func privateField(key string) bool {
	key = strings.Map(func(r rune) rune {
		switch r {
		case '_', '-', '.', ' ':
			return -1
		}
		return r
	}, strings.ToLower(key))
	for _, word := range []string{"password", "token", "secret", "authorization", "apikey", "masterkey", "phone", "idcard", "bankcard", "setupcode", "privatekey", "cookie", "credential", "ciphertext", "nonce", "密码", "口令", "密钥", "令牌", "手机号", "身份证", "银行卡"} {
		if strings.Contains(key, word) {
			return true
		}
	}
	return false
}

// SafeDetails 在进入审计表和对外输出时都处理结构化字段，保护旧记录，保留整数精度。
func SafeDetails(raw []byte) (json.RawMessage, error) {
	if len(raw) > MaxAuditDetails || !json.Valid(raw) {
		return nil, fmt.Errorf("审计详情损坏或超出容量")
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	clean, err := safeValue(value, 0)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(clean)
	if len(encoded) > MaxAuditDetails {
		return nil, fmt.Errorf("脱敏后的审计详情超出容量")
	}
	return encoded, err
}

func safeValue(value any, depth int) (any, error) {
	if depth > 24 {
		return nil, fmt.Errorf("审计详情嵌套超限")
	}
	switch v := value.(type) {
	case map[string]any:
		clean := make(map[string]any, len(v))
		for key, child := range v {
			name := SafeText(key)
			if _, duplicate := clean[name]; duplicate {
				return nil, fmt.Errorf("脱敏后的审计字段冲突")
			}
			// 数量占用、费用与密钥关联标识是可审计元数据。只允许这些已知字段传递非机密结构。
			count, number := child.(json.Number)
			if number && (key == "secret_count" || key == "input_tokens" || key == "output_tokens" || key == "cached_tokens") {
				clean[name] = count
				continue
			}
			if privateField(key) {
				clean[name] = "[已脱敏]"
				continue
			}
			sanitized, err := safeValue(child, depth+1)
			if err != nil {
				return nil, err
			}
			clean[name] = sanitized
		}
		return clean, nil
	case []any:
		for i, item := range v {
			clean, err := safeValue(item, depth+1)
			if err != nil {
				return nil, err
			}
			v[i] = clean
		}
		return v, nil
	case string:
		return SafeText(v), nil
	default:
		return value, nil
	}
}
