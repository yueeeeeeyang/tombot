package computeragent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"slices"

	"io.yueyang/tom/internal/computer"
)

// desktopAccess 只保存所属会话的末张画面与末次输入。撤销不等待业务锁，所有传输跟随撤销取消。
type desktopAccess struct {
	session computer.ExecutionSession
	dial    func(context.Context) (net.Conn, error)
	life    context.Context
	stop    context.CancelFunc
	gate    chan struct{}
	image   computer.DesktopImage
	png     []byte
	input   computer.DesktopInput
	receipt computer.DesktopInputReceipt
}

func newDesktopAccess(session computer.ExecutionSession, dial func(context.Context) (net.Conn, error)) *desktopAccess {
	life, stop := context.WithCancel(context.Background())
	return &desktopAccess{session: session, dial: dial, life: life, stop: stop, gate: make(chan struct{}, 1)}
}

func (access *desktopAccess) enter(ctx context.Context) (context.Context, func(), error) {
	ctx, cancel := context.WithTimeout(ctx, desktopIOTimeout)
	stop := context.AfterFunc(access.life, cancel)
	leave := func() { stop(); cancel() }
	select {
	case access.gate <- struct{}{}:
	case <-ctx.Done():
		leave()
		return nil, nil, ctx.Err()
	}
	if access.life.Err() != nil || ctx.Err() != nil {
		<-access.gate
		leave()
		return nil, nil, errors.New("桌面画面操作授权已结束")
	}
	return ctx, func() { <-access.gate; leave() }, nil
}

func (access *desktopAccess) capture(ctx context.Context, request computer.DesktopCapture) (computer.DesktopImage, error) {
	if request.Validate() != nil || request.Session != access.session {
		return computer.DesktopImage{}, errors.New("桌面截图不属于当前会话")
	}
	ctx, leave, err := access.enter(ctx)
	if err != nil {
		return computer.DesktopImage{}, err
	}
	defer leave()
	if request.Sequence == access.image.Sequence {
		if len(access.png) == 0 {
			return computer.DesktopImage{}, errors.New("该次截图失败，须使用更大的操作序号")
		}
		return access.image, nil
	}
	if request.Sequence < access.image.Sequence {
		return computer.DesktopImage{}, errors.New("桌面截图操作序号已经过期")
	}
	access.image, access.png = computer.DesktopImage{DesktopCapture: request}, nil
	connection, err := access.dial(ctx)
	if err != nil {
		return computer.DesktopImage{}, err
	}
	encoded, captureError := captureDesktopFrame(ctx, connection)
	if err = errors.Join(captureError, connection.Close(), ctx.Err(), access.life.Err()); err != nil {
		return computer.DesktopImage{}, errors.New("桌面截图未完成")
	}
	digest := sha256.Sum256(encoded)
	access.png = encoded
	access.image.Width, access.image.Height = computer.DesktopWidth, computer.DesktopHeight
	access.image.MediaType, access.image.Size, access.image.SHA256 = "image/png", len(encoded), hex.EncodeToString(digest[:])
	return access.image, nil
}

func (access *desktopAccess) read(ctx context.Context, request computer.DesktopImageRead) (computer.DesktopImageChunk, error) {
	if request.Validate() != nil || request.Session != access.session {
		return computer.DesktopImageChunk{}, errors.New("桌面截图读取不属于当前会话")
	}
	_, leave, err := access.enter(ctx)
	if err != nil {
		return computer.DesktopImageChunk{}, err
	}
	defer leave()
	if request.DesktopCapture != access.image.DesktopCapture || len(access.png) == 0 || request.Offset > len(access.png) {
		return computer.DesktopImageChunk{}, errors.New("桌面截图不存在、已过期或读取越界")
	}
	end := min(request.Offset+request.MaximumBytes, len(access.png))
	return computer.DesktopImageChunk{Image: access.image, Offset: request.Offset, Bytes: slices.Clone(access.png[request.Offset:end])}, nil
}

func (access *desktopAccess) send(ctx context.Context, request computer.DesktopInput) (computer.DesktopInputReceipt, error) {
	if request.Validate() != nil || request.Session != access.session {
		return computer.DesktopInputReceipt{}, errors.New("桌面输入不属于当前会话")
	}
	ctx, leave, err := access.enter(ctx)
	if err != nil {
		return computer.DesktopInputReceipt{}, err
	}
	defer leave()
	if request.Sequence == access.input.Sequence {
		if !slices.Equal(request.Events, access.input.Events) {
			return computer.DesktopInputReceipt{}, errors.New("同一桌面输入序号内容不一致")
		}
		return access.receipt, nil
	}
	if request.Sequence < access.input.Sequence || access.receipt.State == "uncertain" {
		return computer.DesktopInputReceipt{}, errors.New("桌面输入操作序号已经过期或前次结果未确认")
	}
	// 先记录尝试，再连接或写入；重发未知动作不会重复产生外部副作用。
	access.input = request
	access.input.Events = slices.Clone(request.Events)
	access.receipt = computer.DesktopInputReceipt{Session: request.Session, Sequence: request.Sequence, State: "uncertain"}
	connection, err := access.dial(ctx)
	if err != nil {
		return access.receipt, nil
	}
	writeError := sendDesktopInput(ctx, connection, request)
	if errors.Join(writeError, connection.Close(), ctx.Err(), access.life.Err()) == nil {
		access.receipt.State = "sent"
	}
	return access.receipt, nil
}

func (access *desktopAccess) close(ctx context.Context) error {
	access.stop()
	select {
	case access.gate <- struct{}{}:
		access.png = nil
		access.input.Events = nil
		<-access.gate
		return nil
	case <-ctx.Done():
		return errors.New("桌面画面操作尚未退出，不能清理缓存")
	}
}
