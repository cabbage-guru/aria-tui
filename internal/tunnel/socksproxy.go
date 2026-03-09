package tunnel

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

// ConnectProxy is an HTTP CONNECT proxy that routes all connections through
// a specific network interface using OS-level socket binding
// (IP_BOUND_IF on macOS, SO_BINDTODEVICE on Linux).
type ConnectProxy struct {
	listener    net.Listener
	iface       string
	port        int
	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	connCount   atomic.Int64
	dialSuccess atomic.Int64
	dialFail    atomic.Int64
	logger      *log.Logger
}

// StartConnectProxy starts an HTTP CONNECT proxy on a free port that routes all
// connections through the given network interface.
func StartConnectProxy(parentCtx context.Context, iface string, logger *log.Logger) (*ConnectProxy, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	port := listener.Addr().(*net.TCPAddr).Port
	ctx, cancel := context.WithCancel(parentCtx)

	if logger == nil {
		logger = log.Default()
	}

	p := &ConnectProxy{
		listener: listener,
		iface:    iface,
		port:     port,
		ctx:      ctx,
		cancel:   cancel,
		logger:   logger,
	}

	p.wg.Add(1)
	go p.serve()

	logger.Printf("[proxy:%s:%d] started HTTP CONNECT proxy", iface, port)
	return p, nil
}

// Port returns the port the proxy is listening on.
func (p *ConnectProxy) Port() int {
	return p.port
}

// Stats returns proxy connection statistics.
func (p *ConnectProxy) Stats() (total, success, fail int64) {
	return p.connCount.Load(), p.dialSuccess.Load(), p.dialFail.Load()
}

// Stop shuts down the proxy.
func (p *ConnectProxy) Stop() {
	p.cancel()
	p.listener.Close()
	p.wg.Wait()
	total, success, fail := p.Stats()
	p.logger.Printf("[proxy:%s:%d] stopped (conns: %d, dial_ok: %d, dial_fail: %d)",
		p.iface, p.port, total, success, fail)
}

func (p *ConnectProxy) serve() {
	defer p.wg.Done()
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			select {
			case <-p.ctx.Done():
				return
			default:
				continue
			}
		}
		go p.handleConn(conn)
	}
}

func (p *ConnectProxy) handleConn(clientConn net.Conn) {
	defer clientConn.Close()
	p.connCount.Add(1)

	// Read the HTTP request
	br := bufio.NewReader(clientConn)
	req, err := http.ReadRequest(br)
	if err != nil {
		p.logger.Printf("[proxy:%s] failed to read request: %v", p.iface, err)
		return
	}

	if req.Method != http.MethodConnect {
		p.logger.Printf("[proxy:%s] non-CONNECT method: %s %s", p.iface, req.Method, req.URL)
		clientConn.Write([]byte("HTTP/1.1 405 Method Not Allowed\r\n\r\n"))
		return
	}

	target := req.Host
	if !strings.Contains(target, ":") {
		target = target + ":443"
	}

	p.logger.Printf("[proxy:%s] CONNECT %s", p.iface, target)

	// Connect through the bound interface
	dialer := &net.Dialer{
		Control: bindToInterfaceControl(p.iface),
	}

	remoteConn, err := dialer.DialContext(p.ctx, "tcp", target)
	if err != nil {
		p.dialFail.Add(1)
		p.logger.Printf("[proxy:%s] dial %s failed: %v", p.iface, target, err)
		clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer remoteConn.Close()
	p.dialSuccess.Add(1)

	localAddr := remoteConn.LocalAddr()
	p.logger.Printf("[proxy:%s] connected %s -> %s (local: %s)", p.iface, target, remoteConn.RemoteAddr(), localAddr)

	// Tell client the tunnel is established
	clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	// Bidirectional copy
	done := make(chan struct{})
	go func() {
		io.Copy(remoteConn, clientConn)
		if tc, ok := remoteConn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
		close(done)
	}()
	io.Copy(clientConn, remoteConn)
	<-done
}
