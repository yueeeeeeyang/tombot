package desktopstream

import (
	"context"
	"errors"
	"io"
	"net"
	"time"
)

// Pipe 保持 RFB 字节不变；慢接收者超过写入预算会断开，不积压无限画面。
func Pipe(ctx context.Context, left, right net.Conn) error {
	results := make(chan error, 2)
	copyStream := func(destination, source net.Conn) {
		buffer := make([]byte, 32<<10)
		for {
			count, err := source.Read(buffer)
			if count > 0 {
				if failure := destination.SetWriteDeadline(time.Now().Add(WriteTimeout)); failure != nil {
					results <- failure
					return
				}
				written, failure := destination.Write(buffer[:count])
				if failure != nil {
					results <- failure
					return
				}
				if written != count {
					results <- io.ErrShortWrite
					return
				}
			}
			if err != nil {
				results <- err
				return
			}
		}
	}
	go copyStream(left, right)
	go copyStream(right, left)
	var first error
	consumed := false
	select {
	case first = <-results:
		consumed = true
	case <-ctx.Done():
		first = ctx.Err()
	}
	closeError := errors.Join(left.Close(), right.Close())
	if !consumed {
		<-results
	} // 取消路径尚未收取任一复制结果。
	second := <-results
	var failure error
	for _, err := range []error{first, second, closeError} {
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) && !errors.Is(err, context.Canceled) {
			failure = errors.Join(failure, err)
		}
	}
	return failure
}
