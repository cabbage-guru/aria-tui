package ipc

import (
	"bufio"
	"net"
	"os"
	"strings"

	"github.com/cabbage-guru/aria-tui/internal/config"
)

// Listener accepts URL submissions over a Unix domain socket.
type Listener struct {
	ln       net.Listener
	urlChan  chan string
	done     chan struct{}
}

// NewListener creates and starts a Unix socket listener.
// Callers read URLs from the channel returned by URLs().
func NewListener() (*Listener, error) {
	sockPath := config.SocketPath()

	// Remove stale socket file from a previous run.
	os.Remove(sockPath)

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, err
	}
	// Allow other processes by this user to connect.
	os.Chmod(sockPath, 0600)

	l := &Listener{
		ln:      ln,
		urlChan: make(chan string, 64),
		done:    make(chan struct{}),
	}
	go l.accept()
	return l, nil
}

// URLs returns the channel that emits submitted URLs.
func (l *Listener) URLs() <-chan string {
	return l.urlChan
}

// Close shuts down the listener and removes the socket file.
func (l *Listener) Close() {
	l.ln.Close()
	<-l.done
	os.Remove(config.SocketPath())
}

func (l *Listener) accept() {
	defer close(l.done)
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			return // listener closed
		}
		go l.handle(conn)
	}
}

func (l *Listener) handle(conn net.Conn) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			l.urlChan <- line
		}
	}
}
