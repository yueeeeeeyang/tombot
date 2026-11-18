package desktopstream

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

func TestViewingControlAndRelease(t *testing.T) {
	viewer, client := net.Pipe()
	server, vnc := net.Pipe()
	relay := New(client, server)
	go relay.Run()
	t.Cleanup(func() {
		if err := relay.Close(); err != nil {
			t.Error(err)
		}
		viewer.Close()
		vnc.Close()
		select {
		case <-relay.Done():
		case <-time.After(time.Second):
			t.Error("转发未停止")
		}
	})
	packets := make(chan []byte, 32)
	go func() {
		handshake := make([]byte, 14)
		if _, err := io.ReadFull(vnc, handshake); err != nil {
			return
		}
		for {
			p, _, _, err := readPacket(vnc)
			if err != nil {
				return
			}
			packets <- p
		}
	}()
	write := func(packet []byte) {
		t.Helper()
		if err := viewer.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		for _, b := range packet {
			if _, err := viewer.Write([]byte{b}); err != nil {
				t.Fatal(err)
			}
		}
	}
	next := func(expected []byte) {
		t.Helper()
		select {
		case actual := <-packets:
			if !bytes.Equal(actual, expected) {
				t.Fatalf("RFB 字节不符: %v / %v", actual, expected)
			}
		case <-time.After(time.Second):
			t.Fatal("未转发预期的 RFB 消息")
		}
	}
	write(append([]byte("RFB 003.008\n"), 1, 1))
	key := []byte{4, 1, 0, 0, 0, 0, 0, 97}
	pointer := []byte{5, 1, 0, 30, 0, 40}
	refresh := []byte{3, 1, 0, 0, 0, 0, 5, 0, 3, 32}
	write(key)
	write(pointer)
	write(refresh)
	next(refresh)
	if err := relay.SetControl(true); err != nil {
		t.Fatal(err)
	}
	write(key)
	next(key)
	write(pointer)
	next(pointer)
	if err := relay.SetControl(false); err != nil {
		t.Fatal(err)
	}
	releasedKey := bytes.Clone(key)
	releasedKey[1] = 0
	next(releasedKey)
	releasedPointer := bytes.Clone(pointer)
	releasedPointer[1] = 0
	next(releasedPointer)
	write(key)
	write(refresh)
	next(refresh)
	if err := relay.SetControl(true); err != nil {
		t.Fatal(err)
	}
	extended := []byte{255, 0, 0, 1, 0, 0, 0, 98, 0, 0, 0, 48}
	write(extended)
	next(extended)
	if err := relay.SetControl(false); err != nil {
		t.Fatal(err)
	}
	extended[3] = 0
	next(extended)
}
func TestRFBPacketLimits(t *testing.T) {
	cases := [][]byte{{250}, {251}, {99}, {2, 0, 0, 129}, {5, 0, 5, 0, 0, 0}, {5, 0, 0, 0, 3, 32}, {248, 0, 0, 0, 0, 0, 0, 0, 65}, {255, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}}
	for _, packet := range cases {
		if _, _, _, err := readPacket(bytes.NewReader(packet)); err == nil {
			t.Fatalf("接受危险或超限消息: %v", packet)
		}
	}
	for _, length := range []int32{65537, -65537} {
		packet := make([]byte, 8)
		packet[0] = 6
		binary.BigEndian.PutUint32(packet[4:], uint32(length))
		if _, _, _, err := readPacket(bytes.NewReader(packet)); err == nil {
			t.Fatal("接受超限剪贴板")
		}
	}
	packet := []byte{6, 0, 0, 0, 0, 0, 0, 3, 'a', 'b', 'c'}
	if _, input, discard, err := readPacket(bytes.NewReader(packet)); err != nil || input || !discard {
		t.Fatal("剪贴板未丢弃", err)
	}
	for _, packet := range [][]byte{{4}, {0, 0}, {2, 0, 0, 1}, {6, 0, 0, 0, 0, 0, 0, 5, 'x'}} {
		if _, _, _, err := readPacket(bytes.NewReader(packet)); err == nil {
			t.Fatal("截断消息未拒绝")
		}
	}
}

func TestRelayReportsHandshakeFailure(t *testing.T) {
	viewer, client := net.Pipe()
	server, vnc := net.Pipe()
	defer viewer.Close()
	defer vnc.Close()
	relay := New(client, server)
	done := make(chan error, 1)
	go func() { done <- relay.Run() }()
	if _, err := viewer.Write([]byte("RFB 003.003\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("非法握手未报告失败")
		}
	case <-time.After(time.Second):
		t.Fatal("非法握手后没有回收转发")
	}
}

func TestPipeCancellationAndHalfClose(t *testing.T) {
	for _, cancelFirst := range []bool{true, false} {
		left, a := net.Pipe()
		b, right := net.Pipe()
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- Pipe(ctx, a, b) }()
		if cancelFirst {
			cancel()
		} else {
			left.Close()
		}
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("半关闭或取消没有收尾")
		}
		cancel()
		left.Close()
		right.Close()
	}
}
