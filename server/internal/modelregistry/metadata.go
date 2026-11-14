package modelregistry

import (
	"encoding/json"
	"regexp"
	"slices"
)

// ModelMetadata 保存供应商明确提供的媒体类型和思考能力；缺失表示未知，不等于不支持。
type ModelMetadata struct {
	InputModalities  []string     `json:"input_modalities,omitempty"`
	OutputModalities []string     `json:"output_modalities,omitempty"`
	Effort           *ModelEffort `json:"effort,omitempty"`
}

// ModelEffort 保留上游推荐顺序与可选默认值，不根据模型名称推断。
type ModelEffort struct {
	SupportedLevels []string `json:"supported_levels"`
	DefaultLevel    string   `json:"default_level,omitempty"`
}

var metadataIdentifier = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// Validate 对元数据集合和默认值做有界校验，拒绝重复、空集合及不属于支持列表的默认级别。
func (metadata ModelMetadata) Validate() error {
	validate := func(values []string) bool {
		if values == nil {
			return true
		}
		if len(values) == 0 || len(values) > 16 {
			return false
		}
		seen := map[string]bool{}
		for _, value := range values {
			if !metadataIdentifier.MatchString(value) || seen[value] {
				return false
			}
			seen[value] = true
		}
		return true
	}
	if !validate(metadata.InputModalities) || !validate(metadata.OutputModalities) {
		return ErrInvalidInput
	}
	if effort := metadata.Effort; effort != nil {
		if len(effort.SupportedLevels) == 0 || !validate(effort.SupportedLevels) || effort.DefaultLevel != "" && !slices.Contains(effort.SupportedLevels, effort.DefaultLevel) {
			return ErrInvalidInput
		}
	}
	return nil
}

func (metadata ModelMetadata) accepts(reasoningEffort string, vision bool) bool {
	return metadata.Validate() == nil && (reasoningEffort == "" || metadata.Effort != nil && slices.Contains(metadata.Effort.SupportedLevels, reasoningEffort)) &&
		(!vision || metadata.InputModalities == nil || slices.Contains(metadata.InputModalities, "image")) &&
		(metadata.InputModalities == nil || slices.Contains(metadata.InputModalities, "text")) &&
		(metadata.OutputModalities == nil || slices.Contains(metadata.OutputModalities, "text"))
}

func decodeModelMetadata(encoded []byte) (ModelMetadata, error) {
	var metadata ModelMetadata
	if json.Unmarshal(encoded, &metadata) != nil || metadata.Validate() != nil {
		return metadata, ErrUnavailable
	}
	return metadata, nil
}
