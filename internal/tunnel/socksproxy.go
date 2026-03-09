package tunnel

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
)

// SocksProxy is a minimal SOCKS5 proxy that routes all connections through
// a specific network interface. This is more reliable than aria2c's --interface
// flag, which only uses bind() (not SO_BINDTODEVICE/IP_BOUND_IF).
type SocksProxy struct {
	listener  net.Listener
	iface     string
	port      int
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// StartSocksProxy starts a SOCKS5 proxy on a free port that routes all
// connections through the given network interface.
func StartSocksProxy(parentCtx context.Context, iface string) (*SocksProxy, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	port := listener.Addr().(*net.TCPAddr).Port
	ctx, cancel := context.WithCancel(parentCtx)

	p := &SocksProxy{
		listener: listener,
		iface:    iface,
		port:     port,
		ctx:      ctx,
		cancel:   cancel,
	}

	p.wg.Add(1)
	go p.serve()

	return p, nil
}

// Port returns the port the proxy is listening on.
func (p *SocksProxy) Port() int {
	return p.port
}

// Stop shuts down the proxy.
func (p *SocksProxy) Stop() {
	p.cancel()
	p.listener.Close()
	p.wg.Wait()
}

func (p *SocksProxy) serve() {
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

func (p *SocksProxy) handleConn(clientConn net.Conn) {
	defer clientConn.Close()

	// SOCKS5 handshake: client sends version + auth methods
	buf := make([]byte, 258)
	if _, err := io.ReadFull(clientConn, buf[:2]); err != nil {
		return
	}
	if buf[0] != 0x05 { // SOCKS5
		return
	}
	nMethods := int(buf[1])
	if _, err := io.ReadFull(clientConn, buf[:nMethods]); err != nil {
		return
	}

	// Reply: no auth required
	clientConn.Write([]byte{0x05, 0x00})

	// Read connect request
	if _, err := io.ReadFull(clientConn, buf[:4]); err != nil {
		return
	}
	if buf[0] != 0x05 || buf[1] != 0x01 { // CONNECT
		clientConn.Write([]byte{0x05, 0x07, 0x00, 0x01, 0, 0, 0, 0, 0, 0}) // command not supported
		return
	}

	var destAddr string
	switch buf[3] {
	case 0x01: // IPv4
		if _, err := io.ReadFull(clientConn, buf[:4]); err != nil {
			return
		}
		destAddr = net.IP(buf[:4]).String()
	case 0x03: // Domain
		if _, err := io.ReadFull(clientConn, buf[:1]); err != nil {
			return
		}
		domainLen := int(buf[0])
		if _, err := io.ReadFull(clientConn, buf[:domainLen]); err != nil {
			return
		}
		destAddr = string(buf[:domainLen])
	case 0x04: // IPv6
		if _, err := io.ReadFull(clientConn, buf[:16]); err != nil {
			return
		}
		destAddr = net.IP(buf[:16]).String()
	default:
		clientConn.Write([]byte{0x05, 0x08, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}

	// Read port
	if _, err := io.ReadFull(clientConn, buf[:2]); err != nil {
		return
	}
	destPort := binary.BigEndian.Uint16(buf[:2])
	target := fmt.Sprintf("%s:%d", destAddr, destPort)

	// Connect through the bound interface
	dialer := &net.Dialer{
		Control: bindToInterfaceControl(p.iface),
	}

	remoteConn, err := dialer.DialContext(p.ctx, "tcp", target)
	if err != nil {
		// Connection refused or network unreachable
		clientConn.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer remoteConn.Close()

	// Send success reply
	localAddr := remoteConn.LocalAddr().(*net.TCPAddr)
	reply := make([]byte, 10)
	reply[0] = 0x05 // version
	reply[1] = 0x00 // success
	reply[2] = 0x00 // reserved
	reply[3] = 0x01 // IPv4
	copy(reply[4:8], localAddr.IP.To4())
	binary.BigEndian.PutUint16(reply[8:10], uint16(localAddr.Port))
	if _, err := clientConn.Write(reply); err != nil {
		return
	}

	// Bidirectional copy
	done := make(chan struct{})
	go func() {
		io.Copy(remoteConn, clientConn)
		remoteConn.(*net.TCPConn).CloseWrite()
		close(done)
	}()
	io.Copy(clientConn, remoteConn)
	<-done
}
