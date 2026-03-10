package ipc

import (
	"fmt"
	"net"
	"time"

	"github.com/cabbage-guru/aria-tui/internal/config"
)

// Send writes a URL to the running TUI's Unix socket.
func Send(url string) error {
	conn, err := net.DialTimeout("unix", config.SocketPath(), 2*time.Second)
	if err != nil {
		return fmt.Errorf("cannot connect to aria-tui (is the TUI running?): %w", err)
	}
	defer conn.Close()

	conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, err = fmt.Fprintln(conn, url)
	return err
}
