package tunnel

import (
	"fmt"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// bindToInterfaceControl returns a Control function for net.Dialer that sets
// IP_BOUND_IF on macOS to force traffic through a specific interface.
func bindToInterfaceControl(iface string) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		ifi, err := net.InterfaceByName(iface)
		if err != nil {
			return fmt.Errorf("interface %s: %w", iface, err)
		}
		var sockErr error
		err = c.Control(func(fd uintptr) {
			// IP_BOUND_IF binds the socket to the interface index
			sockErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_BOUND_IF, ifi.Index)
		})
		if err != nil {
			return err
		}
		return sockErr
	}
}
