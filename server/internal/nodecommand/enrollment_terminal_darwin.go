package nodecommand

import "golang.org/x/sys/unix"

func getTerminalMode(fd int) (*unix.Termios, error) { return unix.IoctlGetTermios(fd, unix.TIOCGETA) }
func setTerminalMode(fd int, mode *unix.Termios) error {
	return unix.IoctlSetTermios(fd, unix.TIOCSETA, mode)
}
func flushTerminalInput(fd int) error {
	return unix.IoctlSetPointerInt(fd, unix.TIOCFLUSH, unix.TCIFLUSH)
}
