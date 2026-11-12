//go:build !linux

package sharedbrowser

import (
	"context"
	"io.yueyang/tom/internal/computer"
	"net"
)

func dialLocalBrowser(context.Context, computer.ExecutionSession) (net.Conn, error) {
	return nil, ErrUnavailable
}
