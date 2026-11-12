//go:build !linux

package processgroup

import (
	"errors"
	"os"
)

func ValidateControls(Reference, []*os.File) error {
	return errors.New("代理资源组描述符仅支持 Linux")
}
func Populated(encoded []byte) (bool, error) { return populated(encoded) }
