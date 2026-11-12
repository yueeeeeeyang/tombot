package term

import (
	"bytes"
	"errors"
	"sort"
	"sync"
)

var errOutputLimit = errors.New("命令输出达到容量上限")

// capture 在分片边界保留足够前缀，确保密钥跨 stdout/stderr 分片时仍先脱敏再保存。
type capture struct {
	mu       sync.Mutex
	buffer   bytes.Buffer
	pending  []byte
	secrets  [][]byte
	longest  int
	maximum  int
	overflow bool
	stop     func()
}

func newCapture(secrets map[string][]byte, maximum int, stop func()) *capture {
	c := &capture{maximum: maximum, stop: stop, longest: 1}
	for _, value := range secrets {
		if len(value) > 0 {
			c.secrets = append(c.secrets, value)
			c.longest = max(c.longest, len(value))
		}
	}
	sort.Slice(c.secrets, func(i, j int) bool { return len(c.secrets[i]) > len(c.secrets[j]) })
	return c
}
func (c *capture) Write(content []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.overflow {
		return 0, errOutputLimit
	}
	c.pending = append(c.pending, content...)
	if err := c.flush(false); err != nil {
		return 0, err
	}
	return len(content), nil
}
func (c *capture) append(content []byte) error {
	remaining := c.maximum - c.buffer.Len()
	if len(content) > remaining {
		c.buffer.Write(content[:remaining])
		c.overflow = true
		c.stop()
		return errOutputLimit
	}
	c.buffer.Write(content)
	return nil
}
func (c *capture) flush(final bool) error {
	boundary := len(c.pending) - c.longest + 1
	if final {
		boundary = len(c.pending)
	}
	index := 0
	for index < boundary {
		matched := false
		for _, value := range c.secrets {
			if bytes.HasPrefix(c.pending[index:], value) {
				if err := c.append([]byte("***")); err != nil {
					return err
				}
				index += len(value)
				matched = true
				break
			}
		}
		if !matched {
			if err := c.append(c.pending[index : index+1]); err != nil {
				return err
			}
			index++
		}
	}
	copy(c.pending, c.pending[index:])
	clear(c.pending[len(c.pending)-index:])
	c.pending = c.pending[:len(c.pending)-index]
	return nil
}
func (c *capture) finish() ([]byte, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var err error
	if !c.overflow {
		err = c.flush(true)
		if errors.Is(err, errOutputLimit) {
			err = nil
		}
	}
	clear(c.pending)
	c.pending = nil
	c.secrets = nil
	return bytes.Clone(c.buffer.Bytes()), c.overflow, err
}
