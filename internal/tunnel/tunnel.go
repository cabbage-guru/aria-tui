package tunnel

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Tunnel represents an active WireGuard tunnel.
type Tunnel struct {
	Name      string
	Interface string // "userspace" for netstack tunnels, or e.g. utun5, wg0
	VPNConfig string
	RPCPort   int
	Aria2Cmd  *exec.Cmd
	Aria2Log  string          // path to aria2c log file for debugging
	Proxy     *ConnectProxy   // HTTP CONNECT proxy that routes through this tunnel
	WG        *UserspaceWG    // userspace WireGuard tunnel (netstack)
	Created   time.Time
	cancel    context.CancelFunc
}

// Manager handles WireGuard tunnel lifecycle.
type Manager struct {
	mu          sync.Mutex
	tunnels     map[string]*Tunnel
	downloadDir string
}

func NewManager(downloadDir string) *Manager {
	return &Manager{
		tunnels:     make(map[string]*Tunnel),
		downloadDir: downloadDir,
	}
}

// SetDownloadDir changes the download directory for new tunnels.
func (m *Manager) SetDownloadDir(dir string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.downloadDir = dir
}

// CheckDependencies verifies that required tools are installed.
func CheckDependencies() error {
	if _, err := exec.LookPath("aria2c"); err != nil {
		switch runtime.GOOS {
		case "darwin":
			return fmt.Errorf("aria2c not found. Install with: brew install aria2")
		case "linux":
			return fmt.Errorf("aria2c not found. Install with: sudo apt install aria2")
		default:
			return fmt.Errorf("aria2c not found")
		}
	}
	return nil
}

// StartTunnel creates a userspace WireGuard tunnel via netstack and starts aria2c
// routed through it via an HTTP CONNECT proxy.
// No kernel interfaces, no routing tables, no sudo needed for WireGuard.
func (m *Manager) StartTunnel(ctx context.Context, name string, wgConfigContents string) (*Tunnel, error) {
	// Find a free port for aria2c RPC
	port, err := freePort()
	if err != nil {
		return nil, fmt.Errorf("finding free port: %w", err)
	}

	tunnelCtx, cancel := context.WithCancel(ctx)

	// Create userspace WireGuard tunnel (netstack - no kernel interface needed)
	wg, err := NewUserspaceWG(wgConfigContents)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("creating userspace WG tunnel: %w", err)
	}

	// Start HTTP CONNECT proxy that dials through the userspace WG tunnel
	proxyLogger := log.New(os.Stderr, "", log.LstdFlags)
	proxy, err := StartConnectProxy(tunnelCtx, name, wg.DialContext, proxyLogger)
	if err != nil {
		wg.Close()
		cancel()
		return nil, fmt.Errorf("starting proxy: %w", err)
	}

	// Start aria2c routed through the proxy
	aria2Cmd, aria2LogPath, err := startAria2c(tunnelCtx, proxy.Port(), port, m.downloadDir)
	if err != nil {
		proxy.Stop()
		wg.Close()
		cancel()
		return nil, fmt.Errorf("starting aria2c: %w", err)
	}

	t := &Tunnel{
		Name:      name,
		Interface: "userspace",
		VPNConfig: name,
		RPCPort:   port,
		Aria2Cmd:  aria2Cmd,
		Aria2Log:  aria2LogPath,
		Proxy:     proxy,
		WG:        wg,
		Created:   time.Now(),
		cancel:    cancel,
	}

	m.mu.Lock()
	m.tunnels[name] = t
	m.mu.Unlock()

	return t, nil
}

// StopTunnel tears down a tunnel.
func (m *Manager) StopTunnel(ctx context.Context, name string) error {
	m.mu.Lock()
	t, ok := m.tunnels[name]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	delete(m.tunnels, name)
	m.mu.Unlock()

	return stopTunnel(t)
}

func stopTunnel(t *Tunnel) error {
	// Cancel context to signal processes
	if t.cancel != nil {
		t.cancel()
	}

	// Kill aria2c
	if t.Aria2Cmd != nil && t.Aria2Cmd.Process != nil {
		t.Aria2Cmd.Process.Kill()
		t.Aria2Cmd.Wait()
	}

	// Stop proxy
	if t.Proxy != nil {
		t.Proxy.Stop()
	}

	// Close userspace WireGuard
	if t.WG != nil {
		t.WG.Close()
	}

	// Clean up aria2c log
	if t.Aria2Log != "" {
		os.Remove(t.Aria2Log)
	}
	return nil
}

// StopAll tears down all active tunnels.
func (m *Manager) StopAll(ctx context.Context) {
	m.mu.Lock()
	tunnels := make([]*Tunnel, 0, len(m.tunnels))
	for _, t := range m.tunnels {
		tunnels = append(tunnels, t)
	}
	m.tunnels = make(map[string]*Tunnel)
	m.mu.Unlock()

	for _, t := range tunnels {
		stopTunnel(t)
	}
}

// GetTunnel returns a tunnel by name.
func (m *Manager) GetTunnel(name string) *Tunnel {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tunnels[name]
}

// IsRunning checks if a tunnel's aria2c process is still alive.
func (m *Manager) IsRunning(name string) bool {
	m.mu.Lock()
	t, ok := m.tunnels[name]
	m.mu.Unlock()
	if !ok {
		return false
	}

	// Check if aria2c is still running
	if t.Aria2Cmd != nil && t.Aria2Cmd.Process != nil {
		// On Unix, sending signal 0 checks if process exists
		err := t.Aria2Cmd.Process.Signal(os.Signal(nil))
		return err == nil
	}
	return false
}

// freePort finds an available TCP port by binding to :0 and releasing it.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port, nil
}

// startAria2c launches an aria2c process that routes through an HTTP CONNECT proxy.
func startAria2c(ctx context.Context, proxyPort int, rpcPort int, downloadDir string) (*exec.Cmd, string, error) {
	args := []string{
		"--enable-rpc=true",
		"--rpc-listen-all=false",
		fmt.Sprintf("--rpc-listen-port=%d", rpcPort),
		"--rpc-allow-origin-all=true",
		"--disable-ipv6=true",
		fmt.Sprintf("--rpc-secret=%s", rpcSecret(rpcPort)),
		fmt.Sprintf("--dir=%s", downloadDir),
		fmt.Sprintf("--all-proxy=http://127.0.0.1:%d", proxyPort),
		fmt.Sprintf("--file-allocation=%s", fileAllocMethod()),
		"--continue=true",
		"--max-connection-per-server=4",
		"--min-split-size=1M",
		"--split=4",
		"--max-tries=0",
		"--retry-wait=5",
		"--timeout=60",
		"--connect-timeout=30",
		"--max-concurrent-downloads=1",
		"--auto-file-renaming=false",
		"--allow-overwrite=true",
		"--summary-interval=0",
		"--console-log-level=warn",
	}

	// Log aria2c output for debugging
	aria2Log, err := os.CreateTemp("", "aria2c-*.log")
	if err != nil {
		return nil, "", fmt.Errorf("creating aria2c log: %w", err)
	}
	logPath := aria2Log.Name()

	cmd := exec.CommandContext(ctx, "aria2c", args...)
	cmd.Stdout = aria2Log
	cmd.Stderr = aria2Log

	if err := cmd.Start(); err != nil {
		aria2Log.Close()
		os.Remove(logPath)
		return nil, "", fmt.Errorf("failed to start aria2c: %w", err)
	}

	// Wait a bit and verify aria2c is still running
	time.Sleep(1 * time.Second)

	if cmd.Process != nil {
		if err := cmd.Process.Signal(os.Signal(nil)); err != nil {
			// Process already exited
			cmd.Wait()
			logData, _ := os.ReadFile(logPath)
			aria2Log.Close()
			output := strings.TrimSpace(string(logData))
			if output == "" {
				output = "no output"
			}
			return nil, "", fmt.Errorf("aria2c exited immediately\nOutput: %s", output)
		}
	}

	aria2Log.Close()

	return cmd, logPath, nil
}

// RPCSecret returns the RPC secret token for a given port.
// Deterministic so the client can compute it without storing state.
func RPCSecret(port int) string {
	return rpcSecret(port)
}

func rpcSecret(port int) string {
	return fmt.Sprintf("aria-tui-%d", port)
}

func fileAllocMethod() string {
	if runtime.GOOS == "darwin" {
		return "none" // macOS doesn't support fallocate
	}
	return "falloc"
}


