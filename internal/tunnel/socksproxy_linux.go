package tunnel

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// bindToInterfaceControl returns a Control function for net.Dialer that sets
// SO_BINDTODEVICE on Linux to force traffic through a specific interface.
func bindToInterfaceControl(iface string) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		var sockErr error
		err := c.Control(func(fd uintptr) {
			sockErr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, iface)
		})
		if err != nil {
			return err
		}
		return sockErr
	}
}
