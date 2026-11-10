package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image/jpeg"
	"io"

	"io.yueyang/tom/internal/browser"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/policy"
	"io.yueyang/tom/internal/service"
	"io.yueyang/tom/internal/workspace"
)

type browserInput struct {
	Action  string                 `json:"action"`
	ID      string                 `json:"id,omitempty"`
	URL     string                 `json:"url,omitempty"`
	Ref     string                 `json:"ref,omitempty"`
	Text    string                 `json:"text,omitempty"`
	Clear   *bool                  `json:"clear,omitempty"`
	Key     string                 `json:"key,omitempty"`
	DeltaX  float64                `json:"delta_x,omitempty"`
	DeltaY  float64                `json:"delta_y,omitempty"`
	Version service.BrowserVersion `json:"version"`
}

func browserDefinition() definition {
	text := map[string]any{"type": "string"}
	return definition{description: "Use your persistent browser tabs with a shared login profile. Actions: tabs, open, snapshot, screenshot, click, type, key, scroll, download, close_tab. Open accepts url and optional id to navigate an existing tab. Other actions require id. Take a fresh snapshot for element refs; every interaction invalidates refs. Click takes ref; type takes ref/text and optional clear (default true); key takes an allowed key; scroll takes delta_x/delta_y. Download takes exactly one url or ref, triggers one native browser download using the existing login session, and saves up to 32 MiB to a unique workspace inbox directory. Use download instead of click for download links or forms. New origins require approval; redirects, popups and embedded documents cannot expand a tab's permission. Click/type/key/download always require an individual user decision. Never enter credentials or secrets. Never retry failed input, downloads or blocked POSTs automatically: inspect the page and propose a new call. Screenshots create JPEG workspace artifacts. Vision-enabled models receive the current image; otherwise use a fresh accessibility snapshot. No arbitrary JavaScript is available. Users may take control, invalidating pending actions.", properties: map[string]any{
		"action": map[string]any{"type": "string", "enum": []string{"tabs", "open", "snapshot", "screenshot", "click", "type", "key", "scroll", "download", "close_tab"}},
		"id":     text, "url": text, "ref": text, "text": map[string]any{"type": "string", "maxLength": browser.MaxInputBytes}, "clear": map[string]any{"type": "boolean"}, "key": text,
		"delta_x": map[string]any{"type": "number", "minimum": -4096, "maximum": 4096}, "delta_y": map[string]any{"type": "number", "minimum": -4096, "maximum": 4096},
	}, required: []string{"action"}}
}

func (input browserInput) command() service.BrowserAction {
	action := input.Action
	if action == "open" {
		action = "navigate"
	}
	return service.BrowserAction{BrowserVersion: input.Version, Action: action, URL: input.URL, Ref: input.Ref, Text: input.Text, Clear: input.Clear, Key: input.Key, DeltaX: input.DeltaX, DeltaY: input.DeltaY}
}

func (e *Executor) prepareBrowser(ctx context.Context, bot domain.Bot, raw []byte) (preparedCall, error) {
	var input browserInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return preparedCall{}, domain.NewError(400, "INVALID_TOOL_INPUT", "A browser argument has the wrong type.")
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return preparedCall{}, err
	}
	allowed := map[string]bool{"action": true, "id": true}
	action := policy.Action{Class: "browser.read", Risk: "low", Title: "查看浏览器", Pattern: "tab:" + input.ID, Details: map[string]any{"tool": "browser", "action": input.Action, "tab_id": input.ID}}
	switch input.Action {
	case "tabs":
		allowed = map[string]bool{"action": true}
	case "open":
		allowed["url"] = true
		action.Class = "net.new_domain"
		action.Title = "访问浏览器地址"
	case "snapshot", "screenshot":
	case "click":
		allowed["ref"] = true
		action.ForceApproval = true
		action.Title = "点击网页元素"
	case "type":
		allowed["ref"], allowed["text"], allowed["clear"] = true, true, true
		action.ForceApproval = true
		action.Title = "向网页输入内容"
		action.Details["text"] = input.Text
	case "key":
		allowed["key"] = true
		action.ForceApproval = true
		action.Title = "向网页发送按键"
		action.Details["key"] = input.Key
	case "download":
		allowed["url"], allowed["ref"] = true, true
		action.ForceApproval = true
		action.Title = "下载网页文件到工作区"
		if err := (browser.DownloadInput{URL: input.URL, Ref: input.Ref}).Validate(); err != nil {
			return preparedCall{}, err
		}
	case "scroll":
		allowed["delta_x"], allowed["delta_y"] = true, true
	case "close_tab":
		action.Class = "browser.close"
		action.Risk = "medium"
		action.Title = "关闭浏览器标签"
	default:
		return preparedCall{}, domain.NewError(400, "INVALID_TOOL_INPUT", "Unsupported browser action.")
	}
	for field := range fields {
		if !allowed[field] {
			return preparedCall{}, domain.NewError(400, "INVALID_TOOL_INPUT", "The argument is not valid for this browser action.")
		}
	}
	if input.Action != "tabs" && input.Action != "open" && input.ID == "" {
		return preparedCall{}, domain.NewError(400, "INVALID_TOOL_INPUT", "This browser action requires a tab id.")
	}
	if input.Action != "download" && (input.Action == "open" || action.ForceApproval || input.Action == "scroll") {
		if err := input.command().Validate(); err != nil {
			return preparedCall{}, err
		}
	}
	if action.ForceApproval {
		action.Class = "browser.interact"
		action.Risk = "high"
		action.Details["ref"] = input.Ref
	}
	if input.ID != "" {
		tab, err := e.browser.Get(ctx, input.ID)
		if err != nil {
			return preparedCall{}, err
		}
		if tab.BotID != bot.ID {
			return preparedCall{}, domain.NewError(403, "BROWSER_TAB_FORBIDDEN", "This tab belongs to another bot.")
		}
		readOnly := input.Action == "snapshot" || input.Action == "screenshot"
		if !readOnly && tab.Controller != "bot" {
			return preparedCall{}, domain.NewError(409, "BROWSER_CONTROL_REQUIRED", "The user controls this tab. Wait until control is returned.")
		}
		if !readOnly {
			input.Version = service.BrowserVersion{Revision: tab.Revision, PageRevision: tab.PageRevision}
		}
		action.Details["title"] = tab.Title
		origin, err := browser.Origin(tab.URL)
		if err != nil && tab.URL != "about:blank" {
			return preparedCall{}, err
		}
		action.Details["origin"] = origin
	}
	if input.Action == "open" || input.Action == "download" && input.URL != "" {
		address, err := browser.NormalizeURL(input.URL)
		if err != nil {
			return preparedCall{}, err
		}
		input.URL = address
		origin, err := browser.Origin(input.URL)
		if err != nil {
			return preparedCall{}, err
		}
		action.Pattern = origin
		action.Details["origin"] = origin
		action.Details["url"] = input.URL
		permitted, err := e.browser.CanVisit(ctx, bot.ID, input.ID, input.URL)
		if err != nil {
			return preparedCall{}, err
		}
		if !permitted {
			action.Risk = "high"
			action.Title = "批准浏览器访问新来源"
			if input.Action == "download" {
				action.Title = "批准新来源并下载文件到工作区"
			}
		}
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return preparedCall{}, err
	}
	digest := sha256.Sum256(encoded)
	return preparedCall{action: action, input: input, timeout: browser.OperationTimeout, binding: hex.EncodeToString(digest[:])}, nil
}

type browserToolFailure struct {
	Tab     service.BrowserTab `json:"tab"`
	Code    string             `json:"error_code"`
	Message string             `json:"message"`
}

func (failure browserToolFailure) FailureCode() string { return failure.Code }

func browserToolTab(tab service.BrowserTab, err error) (any, error) {
	var public *domain.Error
	if errors.As(err, &public) && tab.ID != "" && public.Code != "BROWSER_STATE_FAILED" {
		return browserToolFailure{Tab: tab, Code: public.Code, Message: public.Message}, nil
	}
	return tab, err
}

func (e *Executor) invokeBrowser(ctx context.Context, run domain.Run, callID string, input browserInput, audit workspace.BeforeWrite) (any, error) {
	actor := "bot:" + run.BotID
	switch input.Action {
	case "tabs":
		return e.browser.List(ctx, run.BotID)
	case "open":
		if input.ID == "" {
			tab, err := e.browser.Open(ctx, service.BrowserOpenInput{URL: input.URL}, &run, actor)
			return browserToolTab(tab, err)
		}
		tab, err := e.browser.Apply(ctx, input.ID, input.command(), &run, actor)
		return browserToolTab(tab, err)
	case "snapshot":
		return e.browser.Snapshot(ctx, input.ID, &run)
	case "screenshot":
		picture, err := e.browser.Screenshot(ctx, input.ID, &run, 60)
		if err != nil {
			return nil, err
		}
		config, err := jpeg.DecodeConfig(bytes.NewReader(picture))
		if err != nil {
			return nil, domain.NewError(502, "BROWSER_SCREEN_INVALID", "The browser returned an invalid JPEG frame.")
		}
		file, err := e.files.Upload(ctx, run.BotID, "browser-"+callID+".jpg", bytes.NewReader(picture), audit)
		if err != nil {
			return nil, err
		}
		if _, err = e.events.Publish(ctx, "artifact.created", run.ConversationID, map[string]string{"run_id": run.ID, "bot_id": run.BotID, "path": file.Path, "kind": "image"}); err != nil {
			return nil, err
		}
		return &domain.ArtifactRef{BotID: run.BotID, AttachmentInput: domain.AttachmentInput{Path: file.Path, SHA256: file.SHA256}, Bytes: file.Bytes, Width: config.Width, Height: config.Height, MediaType: "image/jpeg"}, nil
	case "download":
		result, err := e.browser.Download(ctx, input.ID, service.BrowserDownloadInput{BrowserVersion: input.Version, DownloadInput: browser.DownloadInput{URL: input.URL, Ref: input.Ref}}, &run, actor, func(ctx context.Context, botID, name string, source io.Reader) (workspace.WriteResult, error) {
			file, saveErr := e.files.Upload(ctx, botID, name, source, audit)
			if saveErr != nil {
				return file, saveErr
			}
			_, saveErr = e.events.Publish(ctx, "artifact.created", run.ConversationID, map[string]string{"run_id": run.ID, "bot_id": botID, "path": file.Path, "kind": "file"})
			return file, saveErr
		})
		return result, err
	case "close_tab":
		err := e.browser.CloseTab(ctx, input.ID, input.Version, &run, actor)
		return map[string]bool{"closed": err == nil}, err
	default:
		tab, err := e.browser.Apply(ctx, input.ID, input.command(), &run, actor)
		return browserToolTab(tab, err)
	}
}
