// Package memory 实现私有记忆的文件格式与检索规则，不依赖接口或模型运行时。
package memory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
	"io.yueyang/tom/internal/domain"
)

const Directory = ".tom/memory/"
const MaxSummaryRunes = 240

type Document struct {
	Title      string
	Summary    string
	Tags       []string
	Importance int
	Body       string
	TooLong    bool
	header     string
	mapping    *yaml.Node
}

func invalid(message string) error { return domain.NewError(400, "MEMORY_INVALID", message) }

// ValidPath 限定平铺私有 Markdown 路径，拒绝目录跳转、共享路径和无效文件名。
func ValidPath(value string) bool {
	name := strings.TrimPrefix(value, Directory)
	return strings.HasPrefix(value, Directory) && name != ".md" && strings.HasSuffix(name, ".md") && len(name) <= 240 && !strings.ContainsAny(name, "/\\\x00") && path.Clean(value) == value
}

// Slug 保留中英文语义，限制文件名字节；同名冲突交给创建时的条件写入处理。
func Slug(title string) string {
	var out strings.Builder
	separator := false
	for _, r := range strings.ToLower(strings.TrimSpace(title)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if separator && out.Len() > 0 {
				out.WriteByte('-')
			}
			separator = false
			if out.Len()+utf8.RuneLen(r) > 180 {
				break
			}
			out.WriteRune(r)
		} else {
			separator = true
		}
	}
	name := strings.Trim(out.String(), "-")
	if name == "" {
		name = "memory"
	}
	return Directory + name + ".md"
}

func firstLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			runes := []rune(line)
			return string(runes[:min(len(runes), MaxSummaryRunes)])
		}
	}
	return ""
}

// Validate 校验已知元数据与正文的格式和容量，返回可向模型展示的英文错误。
func Validate(input domain.MemoryInput) error {
	if strings.TrimSpace(input.Title) == "" || utf8.RuneCountInString(input.Title) > 200 || strings.ContainsAny(input.Title, "\r\n\x00") || !utf8.ValidString(input.Title) {
		return invalid("Memory titles must contain 1 to 200 characters on one line.")
	}
	if input.Importance < 1 || input.Importance > 5 {
		return invalid("Memory importance must be between 1 and 5.")
	}
	if len(input.Tags) > 16 {
		return invalid("A memory supports at most 16 tags.")
	}
	seen := map[string]bool{}
	for _, tag := range input.Tags {
		if strings.TrimSpace(tag) != tag || tag == "" || utf8.RuneCountInString(tag) > 64 || strings.ContainsAny(tag, "\r\n\x00") || !utf8.ValidString(tag) || seen[tag] {
			return invalid("Memory tags must be unique non-empty strings of at most 64 characters.")
		}
		seen[tag] = true
	}
	if strings.TrimSpace(input.Content) == "" || len(input.Content) > domain.MaxMemoryFileBytes || !utf8.ValidString(input.Content) || strings.ContainsRune(input.Content, 0) {
		return invalid("Memory content must be non-empty UTF-8 text within one MiB.")
	}
	if utf8.RuneCountInString(input.Summary) > MaxSummaryRunes || strings.ContainsAny(input.Summary, "\r\n\x00") || !utf8.ValidString(input.Summary) {
		return invalid("Memory summaries must fit on one line of at most 240 characters.")
	}
	return nil
}

// Parse 只解释标题、标签和重要性；其他 front matter 不执行、不展开，保留供后续编辑。
func Parse(filename, content string) (Document, error) {
	doc := Document{Title: strings.TrimSuffix(path.Base(filename), ".md"), Tags: []string{}, Importance: 3, Body: content}
	if len(content) > domain.MaxMemoryFileBytes || !utf8.ValidString(content) || strings.ContainsRune(content, 0) {
		return doc, invalid("Memory files must be UTF-8 text within one MiB.")
	}
	lines := strings.SplitAfter(content, "\n")
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		closing := -1
		for index := 1; index < len(lines); index++ {
			if strings.TrimSpace(lines[index]) == "---" {
				closing = index
				break
			}
		}
		if closing < 0 {
			return doc, invalid("Memory front matter is missing its closing delimiter.")
		}
		doc.header = strings.Join(lines[1:closing], "")
		doc.Body = strings.Join(lines[closing+1:], "")
		doc.Body = strings.TrimPrefix(strings.TrimPrefix(doc.Body, "\r\n"), "\n")
		if strings.TrimSpace(doc.header) != "" {
			var root yaml.Node
			if err := yaml.Unmarshal([]byte(doc.header), &root); err != nil {
				return doc, invalid("Memory front matter is not valid YAML.")
			}
			if len(root.Content) != 1 || root.Content[0].Kind != yaml.MappingNode {
				return doc, invalid("Memory front matter must be a mapping.")
			}
			doc.mapping = root.Content[0]
			seen := map[string]bool{}
			for index := 0; index < len(doc.mapping.Content); index += 2 {
				key, value := doc.mapping.Content[index], doc.mapping.Content[index+1]
				if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || seen[key.Value] {
					return doc, invalid("Memory front matter contains invalid or repeated keys.")
				}
				seen[key.Value] = true
				switch key.Value {
				case "title":
					if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
						return doc, invalid("Memory title must be a string.")
					}
					doc.Title = value.Value
				case "importance":
					if value.Kind != yaml.ScalarNode || value.Tag != "!!int" {
						return doc, invalid("Memory importance must be an integer.")
					}
					importance, err := strconv.Atoi(value.Value)
					if err != nil {
						return doc, invalid("Memory importance must be an integer.")
					}
					doc.Importance = importance
				case "tags":
					if value.Kind != yaml.SequenceNode {
						return doc, invalid("Memory tags must be a string array.")
					}
					for _, tag := range value.Content {
						if tag.Kind != yaml.ScalarNode || tag.Tag != "!!str" {
							return doc, invalid("Memory tags must contain only strings.")
						}
						doc.Tags = append(doc.Tags, tag.Value)
					}
				}
			}
		}
	}
	doc.Summary = firstLine(doc.Body)
	doc.TooLong = len(content) > 64<<10 || strings.Count(strings.TrimSuffix(content, "\n"), "\n")+1 > 200
	return doc, Validate(domain.MemoryInput{Title: doc.Title, Content: doc.Body, Tags: doc.Tags, Importance: doc.Importance})
}

// Render 只重写三个已知字段；未知元数据保留原始行，摘要作为正文首段使重建不依赖隐藏数据库状态。
func Render(previous *Document, input domain.MemoryInput) (string, error) {
	if err := Validate(input); err != nil {
		return "", err
	}
	fields := map[string]string{}
	for key, value := range map[string]any{"title": strings.TrimSpace(input.Title), "tags": input.Tags, "importance": input.Importance} {
		if key == "tags" && input.Tags == nil {
			value = []string{}
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return "", err
		}
		fields[key] = key + ": " + string(encoded) + "\n"
	}
	var header strings.Builder
	if previous != nil && previous.mapping != nil {
		mapping := previous.mapping
		if mapping.Style&yaml.FlowStyle != 0 {
			return "", invalid("Convert flow-style front matter to block style before editing metadata.")
		}
		lines := strings.SplitAfter(previous.header, "\n")
		if lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		if len(mapping.Content) > 0 {
			header.WriteString(strings.Join(lines[:mapping.Content[0].Line-1], ""))
		}
		for index := 0; index < len(mapping.Content); index += 2 {
			key := mapping.Content[index]
			end := len(lines)
			if index+2 < len(mapping.Content) {
				end = mapping.Content[index+2].Line - 1
			}
			if replacement, known := fields[key.Value]; known {
				header.WriteString(replacement)
				delete(fields, key.Value)
			} else {
				header.WriteString(strings.Join(lines[key.Line-1:end], ""))
			}
		}
	} else if previous != nil {
		header.WriteString(previous.header)
	}
	for _, key := range []string{"title", "tags", "importance"} {
		if value := fields[key]; value != "" {
			header.WriteString(value)
		}
	}
	body := input.Content
	if summary := strings.TrimSpace(input.Summary); summary != "" && firstLine(body) != summary {
		body = summary + "\n\n" + body
	}
	var out bytes.Buffer
	fmt.Fprintf(&out, "---\n%s---\n\n%s", header.String(), body)
	if out.Len() > domain.MaxMemoryFileBytes {
		return "", invalid("The complete memory file exceeds one MiB.")
	}
	return out.String(), nil
}
