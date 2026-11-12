package egressproxy

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
)

const maximumHeaderBytes = 32 << 10

// headerConnection 保留逐跳头名称，不保留头值。net/http 在响应含 close 时会先删除整个 Connection，
// 因而必须在解析前取出其中声明的其他逐跳字段，防止把这些字段当作端到端响应转发。
type headerConnection struct {
	net.Conn
	buffer     []byte
	readBytes  int
	done       bool
	hopHeaders []string
}

func (connection *headerConnection) Read(output []byte) (int, error) {
	count, err := connection.Conn.Read(output)
	if connection.done || count == 0 {
		return count, err
	}
	for _, value := range output[:count] {
		connection.readBytes++
		if connection.readBytes > maximumHeaderBytes {
			return 0, errors.New("出网响应头超过限额")
		}
		connection.buffer = append(connection.buffer, value)
		if !bytes.HasSuffix(connection.buffer, []byte("\r\n\r\n")) {
			continue
		}
		reader := bufio.NewReader(bytes.NewReader(connection.buffer))
		line, lineErr := reader.ReadString('\n')
		fields := strings.Fields(line)
		if lineErr != nil || len(fields) < 2 {
			return 0, errors.New("出网响应状态无效")
		}
		status, statusErr := strconv.Atoi(fields[1])
		header, headerErr := textproto.NewReader(reader).ReadMIMEHeader()
		clear(connection.buffer)
		connection.buffer = connection.buffer[:0]
		if statusErr != nil || status < 100 || status > 999 || headerErr != nil {
			return 0, errors.New("出网响应头无效")
		}
		if status < 200 && status != http.StatusSwitchingProtocols {
			continue
		}
		for _, value := range header.Values("Connection") {
			for _, name := range strings.Split(value, ",") {
				connection.hopHeaders = append(connection.hopHeaders, textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(name)))
			}
		}
		connection.done = true
		break
	}
	return count, err
}
