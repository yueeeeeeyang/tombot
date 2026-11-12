package deployment

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"unicode/utf8"
)

// validatePreparationJSON 只核对对象结构和字段拼写；具体字段值交给所属组件的正式解析器。
func validatePreparationJSON(encoded []byte, target any) error {
	invalid := errors.New("部署参数包含歧义字段或无效 JSON")
	trimmed := bytes.TrimSpace(encoded)
	if len(trimmed) == 0 || len(encoded) > 256<<10 || trimmed[0] != '{' || !utf8.Valid(encoded) {
		return invalid
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if readPreparationValue(decoder, reflect.TypeOf(target), 0) != nil {
		return invalid
	}
	if _, err := decoder.Token(); err != io.EOF {
		return invalid
	}
	return nil
}

func readPreparationValue(decoder *json.Decoder, shape reflect.Type, depth int) error {
	invalid := errors.New("部署参数结构无效")
	if depth > 16 {
		return invalid
	}
	for shape != nil && shape.Kind() == reflect.Pointer {
		shape = shape.Elem()
	}
	if shape == reflect.TypeFor[json.RawMessage]() {
		shape = nil
	}
	value, err := decoder.Token()
	if err != nil || value == nil {
		return invalid
	}
	delimiter, nested := value.(json.Delim)
	if !nested {
		return nil
	}
	switch delimiter {
	case '{':
		fields := map[string]reflect.Type{}
		if shape != nil && shape.Kind() == reflect.Struct {
			for index := 0; index < shape.NumField(); index++ {
				field := shape.Field(index)
				name := strings.Split(field.Tag.Get("json"), ",")[0]
				if name != "" && name != "-" {
					fields[name] = field.Type
				}
			}
		}
		seen := map[string]bool{}
		for decoder.More() {
			key, err := decoder.Token()
			name, ok := key.(string)
			if err != nil || !ok || seen[name] {
				return invalid
			}
			seen[name] = true
			var child reflect.Type
			if shape != nil {
				switch shape.Kind() {
				case reflect.Struct:
					child = fields[name]
					if child == nil {
						return invalid
					}
				case reflect.Map:
					child = shape.Elem()
				default:
					return invalid
				}
			}
			if readPreparationValue(decoder, child, depth+1) != nil {
				return invalid
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return invalid
		}
	case '[':
		var child reflect.Type
		if shape != nil {
			if shape.Kind() != reflect.Slice && shape.Kind() != reflect.Array {
				return invalid
			}
			child = shape.Elem()
		}
		for decoder.More() {
			if readPreparationValue(decoder, child, depth+1) != nil {
				return invalid
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return invalid
		}
	default:
		return invalid
	}
	return nil
}
