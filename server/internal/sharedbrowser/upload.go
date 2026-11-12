package sharedbrowser

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/chromedp"
)

// UploadBrowser 仅接收本机调用者已经读取的文件内容，不接受 Chrome 可读取的任意路径。
type UploadBrowser interface {
	Upload(context.Context, Address, io.Reader) (Window, error)
}

// Upload 先在用户级窄门禁内读取有界正文，再在所属窗口内准备只读副本并执行一次文件选择。
func (service *Service) Upload(ctx context.Context, address Address, body io.Reader) (Window, error) {
	if body == nil || service.options.Uploads == nil {
		return Window{}, ErrUnavailable
	}
	service.mu.Lock()
	if service.closed || service.failed {
		service.mu.Unlock()
		return Window{}, ErrUnavailable
	}
	service.uploads.Add(1)
	service.mu.Unlock()
	defer service.uploads.Done()
	bounded, cancel := service.bounded(service.root, ctx)
	defer cancel()
	if _, err := service.own(bounded, address); err != nil {
		return Window{}, err
	}
	if err := acquire(bounded, service.uploadGate); err != nil {
		return Window{}, err
	}
	defer func() { <-service.uploadGate }()
	encoded, err := io.ReadAll(io.LimitReader(body, maximumUploadBody+1))
	if err != nil || len(encoded) > maximumUploadBody {
		return Window{}, ErrInvalid
	}
	request, err := decodeUpload(encoded)
	if err != nil {
		return Window{}, err
	}
	digest := sha256.Sum256(encoded)
	command := Command{Sequence: request.Sequence, Kind: "upload", Text: hex.EncodeToString(digest[:])}
	return service.execute(bounded, address, command, func(ctx context.Context, item *entry) (func(context.Context) error, error) {
		tab := chromedp.FromContext(item.ctx).Target
		node, err := fileInput(ctx, tab, request.Selector, len(request.Files))
		if err != nil {
			return nil, err
		}
		id, paths, err := service.options.Uploads.Stage(ctx, request.Files)
		if err != nil {
			return nil, err
		}
		service.mu.Lock()
		ready := item.window.State == "ready" && item.life.Err() == nil
		if ready {
			item.uploadBatches = append(item.uploadBatches, id)
		}
		service.mu.Unlock()
		if !ready {
			return nil, errors.Join(ErrDenied, service.options.Uploads.Release([]string{id}))
		}
		return func(ctx context.Context) error {
			_, err := cdp.Call(ctx, tab, dom.SetFileInputFiles, dom.SetFileInputFilesParams{Files: paths, BackendNodeID: node})
			return err
		}, nil
	})
}

func decodeUpload(encoded []byte) (UploadRequest, error) {
	var request UploadRequest
	if len(encoded) > maximumUploadBody || json.Unmarshal(encoded, &request) != nil {
		return request, ErrInvalid
	}
	canonical, err := json.Marshal(request)
	// 私有客户端统一生成规范 JSON，拒绝重复字段、别名、未知字段和相同内容的多种解释。
	if err != nil || !bytes.Equal(encoded, canonical) {
		return request, ErrInvalid
	}
	return request, request.Validate()
}

// fileInput 只在所属页面查找文件输入框；不接受模型传入的全局节点或 CDP 会话。
func fileInput(ctx context.Context, tab cdp.Session, selector string, count int) (cdp.BackendNodeID, error) {
	document, err := cdp.Call(ctx, tab, dom.GetDocument, dom.GetDocumentParams{})
	if err != nil || document.Root == nil {
		return 0, ErrUnavailable
	}
	selected, err := cdp.Call(ctx, tab, dom.QuerySelector, dom.QuerySelectorParams{NodeID: document.Root.NodeID, Selector: selector})
	if err != nil {
		return 0, ErrUnavailable
	}
	if selected.NodeID == 0 {
		return 0, ErrInvalid
	}
	described, err := cdp.Call(ctx, tab, dom.DescribeNode, dom.DescribeNodeParams{NodeID: selected.NodeID})
	if err != nil || described.Node == nil {
		return 0, ErrUnavailable
	}
	node := described.Node
	if !strings.EqualFold(node.NodeName, "input") || len(node.Attributes)%2 != 0 {
		return 0, ErrInvalid
	}
	isFile, multiple := false, false
	for index := 0; index < len(node.Attributes); index += 2 {
		switch strings.ToLower(node.Attributes[index]) {
		case "type":
			isFile = strings.EqualFold(node.Attributes[index+1], "file")
		case "multiple":
			multiple = true
		}
	}
	if !isFile || count > 1 && !multiple || node.BackendNodeID == 0 {
		return 0, ErrInvalid
	}
	return node.BackendNodeID, nil
}

func (service *Service) releaseUploads(item *entry) error {
	service.mu.Lock()
	batches := append([]string(nil), item.uploadBatches...)
	service.mu.Unlock()
	if len(batches) == 0 {
		return nil
	}
	if service.options.Uploads == nil {
		return ErrUnavailable
	}
	if err := service.options.Uploads.Release(batches); err != nil {
		service.fail()
		return err
	}
	service.mu.Lock()
	item.uploadBatches = nil
	service.mu.Unlock()
	return nil
}
