package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/chromedp/cdproto/accessibility"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/chromedp"
	"io.yueyang/tom/internal/domain"
)

const MaxSnapshotLines = 300
const MaxSnapshotBytes = 32 << 10

type reference struct {
	node cdp.BackendNodeID
	role string
}
type Snapshot struct {
	Tab       Tab    `json:"tab"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
}

func boundedText(text string, maximum int) string {
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text)
	if len(text) <= maximum {
		return text
	}
	text = text[:maximum]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text
}
func axText(value *accessibility.Value) string {
	if value == nil {
		return ""
	}
	var text string
	if err := json.Unmarshal(value.Value, &text); err != nil {
		return ""
	}
	return strings.TrimSpace(boundedText(text, 1024))
}
func interactive(role string) bool {
	switch role {
	case "button", "link", "textbox", "searchbox", "checkbox", "radio", "combobox", "menuitem", "tab", "switch", "slider", "spinbutton":
		return true
	}
	return false
}

// renderSnapshot 只输出语义和名称，不导出输入框 value；每次生成新的引用，旧编号永不复用。
func (m *Manager) renderSnapshot(tab *entry, nodes []*accessibility.Node) Snapshot {
	result := Snapshot{Tab: tab.Tab}
	lines := []string{"URL: " + boundedText(tab.URL, 8192), "Title: " + boundedText(tab.Title, 512), "---"}
	size := len(strings.Join(lines, "\n"))
	tab.refs = map[string]reference{}
	previousText := ""
	for _, node := range nodes {
		if node == nil || node.Ignored {
			continue
		}
		role, name := axText(node.Role), axText(node.Name)
		isInteractive := interactive(role) && node.BackendDOMNodeID != 0
		if !isInteractive && role != "StaticText" && role != "heading" && role != "paragraph" {
			continue
		}
		if !isInteractive && (name == "" || name == previousText) {
			continue
		}
		label, ref := role, ""
		if isInteractive {
			m.refSequence++
			ref = fmt.Sprintf("e%d", m.refSequence)
			label = "[ref=" + ref + "] " + role
		}
		line := label + " " + strconv.Quote(name)
		if len(lines) >= MaxSnapshotLines || size+len(line)+1 > MaxSnapshotBytes {
			result.Truncated = true
			break
		}
		lines = append(lines, line)
		size += len(line) + 1
		if ref != "" {
			tab.refs[ref] = reference{node: node.BackendDOMNodeID, role: role}
			previousText = ""
		} else {
			previousText = name
		}
	}
	result.Text = strings.Join(lines, "\n")
	return result
}

// Snapshot 按主文档无障碍树生成有界快照；文档在读取期间变化则明确要求重新读取。
func (m *Manager) Snapshot(ctx context.Context, selected Selection) (Snapshot, error) {
	var result Snapshot
	err := m.withTab(ctx, selected, func(ctx context.Context, tab *entry) error {
		epoch := tab.epoch.Load()
		if err := m.metadata(ctx, tab); err != nil {
			return err
		}
		tree, err := cdp.Call(ctx, chromedp.FromContext(tab.ctx).Target, accessibility.GetFullAXTree, accessibility.GetFullAXTreeParams{})
		if err != nil {
			return err
		}
		if tab.epoch.Load() != epoch {
			tab.refs = nil
			return domain.NewError(409, "BROWSER_DOCUMENT_CHANGED", "The document changed while reading. Take another snapshot.")
		}
		tab.refsEpoch = epoch
		result = m.renderSnapshot(tab, tree.Nodes)
		return nil
	})
	return result, err
}

func findReference(tab *entry, ref string) (reference, error) {
	node, ok := tab.refs[ref]
	if !ok || tab.refsEpoch != tab.epoch.Load() {
		return reference{}, domain.NewError(409, "BROWSER_REF_STALE", "This reference is stale. Take a new snapshot before interacting.")
	}
	return node, nil
}
