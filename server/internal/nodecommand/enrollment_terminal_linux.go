package nodecommand

import "golang.org/x/sys/unix"

func getTerminalMode(fd int) (*unix.Termios, error) { return unix.IoctlGetTermios(fd, unix.TCGETS) }
func setTerminalMode(fd int, mode *unix.Termios) error {
	return unix.IoctlSetTermios(fd, unix.TCSETS, mode)
}
func flushTerminalInput(fd int) error { return unix.IoctlSetInt(fd, unix.TCFLSH, unix.TCIFLUSH) }
