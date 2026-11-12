// adminpreview 仅供管理前端浏览器验收使用，通过正式静态处理器提供构建产物，不提供业务 API。
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	adminweb "io.yueyang/tom/web/admin"
)

func main() {
	assets := flag.String("assets", "", "管理前端构建目录")
	address := flag.String("address", "127.0.0.1:4177", "仅限本机的测试监听地址")
	flag.Parse()
	if err := run(*assets, *address); err != nil {
		slog.Error("管理前端测试服务失败", "error", err)
		os.Exit(1)
	}
}

func run(assets, address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil || host != "127.0.0.1" || assets == "" {
		return errors.New("管理前端测试服务参数无效")
	}
	handler, err := adminweb.New(os.DirFS(assets))
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		bounded, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := server.Shutdown(bounded)
		if err != nil {
			return errors.Join(err, server.Close())
		}
		serveErr := <-done
		if !errors.Is(serveErr, http.ErrServerClosed) {
			return serveErr
		}
		return nil
	}
}
