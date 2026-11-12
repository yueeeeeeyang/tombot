package nodeapp

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"unicode/utf8"
)

// decodeConfig 允许缩进及字段重排，拒绝重复键、空值、大小写别名和转换后改变含义的值。
func decodeConfig(encoded []byte, target *Config) error {
	invalid := errors.New("节点配置 JSON 无效或包含歧义字段")
	if len(encoded) == 0 || len(encoded) > 16<<10 || !utf8.Valid(encoded) {
		return invalid
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	source, err := readConfigValue(decoder, 0)
	if err != nil {
		return invalid
	}
	if _, ok := source.(map[string]any); !ok {
		return invalid
	}
	if _, err := decoder.Token(); err != io.EOF {
		return invalid
	}
	decoder = json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return invalid
	}
	canonical, err := json.Marshal(target)
	if err != nil {
		return invalid
	}
	decoder = json.NewDecoder(bytes.NewReader(canonical))
	decoder.UseNumber()
	var normalized any
	if decoder.Decode(&normalized) != nil || !matchesConfigFields(source, normalized) {
		return invalid
	}
	return nil
}

func readConfigValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > 8 {
		return nil, errors.New("节点配置嵌套过深")
	}
	value, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, errors.New("节点配置不接受空值")
	}
	if delimiter, ok := value.(json.Delim); ok {
		switch delimiter {
		case '{':
			fields := map[string]any{}
			for decoder.More() {
				key, err := decoder.Token()
				name, ok := key.(string)
				if err != nil || !ok {
					return nil, errors.New("节点配置字段无效")
				}
				if _, exists := fields[name]; exists {
					return nil, errors.New("节点配置字段重复")
				}
				child, err := readConfigValue(decoder, depth+1)
				if err != nil {
					return nil, err
				}
				fields[name] = child
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim('}') {
				return nil, errors.New("节点配置对象未结束")
			}
			return fields, nil
		case '[':
			values := []any{}
			for decoder.More() {
				child, err := readConfigValue(decoder, depth+1)
				if err != nil {
					return nil, err
				}
				values = append(values, child)
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim(']') {
				return nil, errors.New("节点配置数组未结束")
			}
			return values, nil
		default:
			return nil, errors.New("节点配置分隔符无效")
		}
	}
	return value, nil
}
func matchesConfigFields(source, target any) bool {
	if fields, ok := source.(map[string]any); ok {
		normalized, ok := target.(map[string]any)
		if !ok {
			return false
		}
		for name, value := range fields {
			other, ok := normalized[name]
			if !ok || !matchesConfigFields(value, other) {
				return false
			}
		}
		return true
	}
	if values, ok := source.([]any); ok {
		normalized, ok := target.([]any)
		if !ok || len(values) != len(normalized) {
			return false
		}
		for index, value := range values {
			if !matchesConfigFields(value, normalized[index]) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(source, target)
}
