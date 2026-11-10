package memory

import (
	"strings"
	"testing"

	"io.yueyang/tom/internal/domain"
)

func TestDocumentRoundTripPreservesUnknownMetadata(t *testing.T) {
	source := "---\ntitle: 信源偏好\ntags: [research, sources]\nimportance: 4\nupdated_at: 2026-11-05T10:00:00Z\ncustom: |\n  保留第一行\n  保留第二行\n---\n\n优先官方资料。\n更多正文。\n"
	doc, err := Parse("sources.md", source)
	if err != nil || doc.Title != "信源偏好" || doc.Summary != "优先官方资料。" || doc.Importance != 4 || len(doc.Tags) != 2 {
		t.Fatal(doc, err)
	}
	rendered, err := Render(&doc, domain.MemoryInput{Title: "更新后的偏好", Content: "新的正文。", Tags: []string{"官方"}, Importance: 5})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered, "updated_at: 2026-11-05T10:00:00Z\ncustom: |\n  保留第一行\n  保留第二行\n") {
		t.Fatal("未知元数据发生改变", rendered)
	}
	updated, err := Parse("sources.md", rendered)
	if err != nil || updated.Title != "更新后的偏好" || updated.Body != "新的正文。" || updated.Importance != 5 || updated.Tags[0] != "官方" {
		t.Fatal(updated, err)
	}
}

func TestDocumentFallbackSummaryAndSizeFlags(t *testing.T) {
	doc, err := Parse("项目说明.md", "\n首行作为摘要\n正文")
	if err != nil || doc.Title != "项目说明" || doc.Summary != "首行作为摘要" || doc.Importance != 3 {
		t.Fatal(doc, err)
	}
	encoded, err := Render(nil, domain.MemoryInput{Title: "项目说明", Summary: "用户明确的摘要", Content: "其余正文", Importance: 3})
	if err != nil {
		t.Fatal(err)
	}
	doc, err = Parse("project.md", encoded)
	if err != nil || doc.Summary != "用户明确的摘要" || !strings.Contains(doc.Body, "其余正文") {
		t.Fatal(doc, err)
	}
	for _, body := range []string{strings.Repeat("line\n", 201), strings.Repeat("长", 22000)} {
		doc, err = Parse("long.md", body)
		if err != nil || !doc.TooLong {
			t.Fatal("超长记忆未标注", err)
		}
	}
}

func TestInvalidMetadataAndPathBoundaries(t *testing.T) {
	for _, header := range []string{"title: first\ntitle: second", "title: [wrong]", "importance: 6", "importance: nope", "tags: string", "tags: [same, same]", "title: &name okay\ntags: [*name]"} {
		if _, err := Parse("test.md", "---\n"+header+"\n---\n\n正文"); err == nil {
			t.Fatal("无效元数据被接受", header)
		}
	}
	for _, path := range []string{"../memory.md", "shared/test.md", ".tom/memory/../test.md", ".tom/memory/nested/test.md", ".tom/memory/.md"} {
		if ValidPath(path) {
			t.Fatal(path)
		}
	}
	if slug := Slug("中文 Research / Project"); slug != ".tom/memory/中文-research-project.md" || !ValidPath(slug) {
		t.Fatal(slug)
	}
}
