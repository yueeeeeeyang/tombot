// Package desktopstream 转发标准 RFB 字节流，并在受信端执行观看与输入权限。
package desktopstream

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

const WriteTimeout = 5 * time.Second

// Relay 每个观看连接只有一个实例；权限切换与写入共用锁，撤权先释放按键再返回。
type Relay struct {
	mu                     sync.Mutex
	client, server         net.Conn
	control, ready, closed bool
	keys                   map[uint64][]byte
	pointer                []byte
	done                   chan struct{}
}

func New(client, server net.Conn) *Relay {
	return &Relay{client: client, server: server, keys: map[uint64][]byte{}, done: make(chan struct{})}
}

func (relay *Relay) Run() error {
	results := make(chan error, 2)
	go func() { _, err := io.Copy(relay.client, relay.server); results <- err }()
	go func() { results <- relay.forward() }()
	first := <-results
	closeError := relay.Close()
	second := <-results
	var failure error
	for _, err := range []error{first, second, closeError} {
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
			failure = errors.Join(failure, err)
		}
	}
	close(relay.done)
	return failure
}

func (relay *Relay) Done() <-chan struct{} { return relay.done }

func (relay *Relay) SetControl(enabled bool) error {
	relay.mu.Lock()
	defer relay.mu.Unlock()
	if relay.closed {
		return net.ErrClosed
	}
	if !enabled {
		if err := relay.release(); err != nil {
			return err
		}
	}
	relay.control = enabled
	return nil
}

func (relay *Relay) Close() error {
	relay.mu.Lock()
	defer relay.mu.Unlock()
	if relay.closed {
		return nil
	}
	relay.closed = true
	relay.control = false
	return errors.Join(relay.release(), relay.client.Close(), relay.server.Close())
}

func (relay *Relay) write(encoded []byte) error {
	if err := relay.server.SetWriteDeadline(time.Now().Add(WriteTimeout)); err != nil {
		return err
	}
	for len(encoded) > 0 {
		count, err := relay.server.Write(encoded)
		if err != nil {
			return err
		}
		if count == 0 {
			return io.ErrShortWrite
		}
		encoded = encoded[count:]
	}
	return nil
}

func (relay *Relay) release() error {
	if !relay.ready {
		return nil
	}
	var failure error
	for key, packet := range relay.keys {
		packet[1] = 0
		if packet[0] == 255 {
			packet[2], packet[3] = 0, 0
		}
		if err := relay.write(packet); err != nil {
			return err
		}
		delete(relay.keys, key)
	}
	if relay.pointer != nil {
		relay.pointer[1] = 0
		if len(relay.pointer) == 7 {
			relay.pointer[1] = 128
			relay.pointer[6] = 0
		}
		failure = errors.Join(failure, relay.write(relay.pointer))
		relay.pointer = nil
	}
	return failure
}

func (relay *Relay) forward() error {
	// 逐步转发握手，不能等完整握手后再写，否则服务端等待安全选择会死锁。
	for _, expected := range [][]byte{[]byte("RFB 003.008\n"), {1}, {1}} {
		encoded := make([]byte, len(expected))
		if _, err := io.ReadFull(relay.client, encoded); err != nil {
			return err
		}
		if !bytes.Equal(encoded, expected) {
			return errors.New("桌面 RFB 握手无效")
		}
		relay.mu.Lock()
		err := relay.write(encoded)
		relay.mu.Unlock()
		if err != nil {
			return err
		}
	}
	relay.mu.Lock()
	relay.ready = true
	relay.mu.Unlock()
	for {
		packet, input, discard, err := readPacket(relay.client)
		if err != nil {
			return err
		}
		relay.mu.Lock()
		if relay.closed {
			relay.mu.Unlock()
			return net.ErrClosed
		}
		if discard || input && !relay.control {
			relay.mu.Unlock()
			continue
		}
		if input {
			switch packet[0] {
			case 4, 255:
				key := uint64(binary.BigEndian.Uint32(packet[4:8]))
				if packet[0] == 255 {
					key = 1<<32 | uint64(binary.BigEndian.Uint32(packet[8:12]))
				}
				down := packet[1] != 0
				if packet[0] == 255 {
					down = binary.BigEndian.Uint16(packet[2:4]) != 0
				}
				if down {
					if _, exists := relay.keys[key]; !exists && len(relay.keys) >= 64 {
						relay.mu.Unlock()
						return errors.New("桌面按键数量超过限制")
					}
					relay.keys[key] = append([]byte(nil), packet...)
				} else {
					delete(relay.keys, key)
				}
			case 5:
				relay.pointer = append([]byte(nil), packet...)
			}
		}
		err = relay.write(packet)
		relay.mu.Unlock()
		if err != nil {
			return err
		}
	}
}

// readPacket 仅接受 noVNC 使用的 RFB 消息；不允许扩展电源控制、文件传输、改分辨率或剪贴板。
func readPacket(reader io.Reader) ([]byte, bool, bool, error) {
	packet := make([]byte, 1, 1024)
	if _, err := io.ReadFull(reader, packet); err != nil {
		return nil, false, false, err
	}
	read := func(size int) error {
		start := len(packet)
		packet = append(packet, make([]byte, size)...)
		_, err := io.ReadFull(reader, packet[start:])
		return err
	}
	size := 0
	input, discard := false, false
	switch packet[0] {
	case 0:
		size = 19
	case 2:
		if err := read(3); err != nil {
			return nil, false, false, err
		}
		count := int(binary.BigEndian.Uint16(packet[2:4]))
		if count > 128 {
			return nil, false, false, errors.New("桌面编码列表超过限制")
		}
		size = count * 4
	case 3:
		size = 9
	case 4:
		size = 7
		input = true
	case 5:
		if err := read(5); err != nil {
			return nil, false, false, err
		}
		if binary.BigEndian.Uint16(packet[2:4]) >= 1280 || binary.BigEndian.Uint16(packet[4:6]) >= 800 {
			return nil, false, false, errors.New("桌面坐标超出范围")
		}
		if packet[1]&128 != 0 {
			size = 1
		}
		input = true
	case 6:
		if err := read(7); err != nil {
			return nil, false, false, err
		}
		length := int64(int32(binary.BigEndian.Uint32(packet[4:8])))
		if length < 0 {
			length = -length
		}
		if length > 64<<10 {
			return nil, false, false, errors.New("桌面剪贴板超过限制")
		}
		size = int(length)
		discard = true
	case 150:
		size = 9
	case 248:
		if err := read(8); err != nil {
			return nil, false, false, err
		}
		size = int(packet[8])
		if size > 64 {
			return nil, false, false, errors.New("桌面同步消息超过限制")
		}
	case 255:
		size = 11
		input = true
	default:
		return nil, false, false, errors.New("桌面 RFB 消息不受支持")
	}
	if err := read(size); err != nil {
		return nil, false, false, err
	}
	if packet[0] == 255 && packet[1] != 0 {
		return nil, false, false, errors.New("桌面键盘扩展无效")
	}
	return packet, input, discard, nil
}
