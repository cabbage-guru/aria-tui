package tunnel

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Tunnel represents an active WireGuard tunnel with an interface.
type Tunnel struct {
	Name      string
	Interface string // e.g. utun5, wg0
	VPNConfig string
	RPCPort   int
	Aria2Cmd  *exec.Cmd
	WGCmd     *exec.Cmd // wireguard-go process
	Created   time.Time
	cancel    context.CancelFunc
}

// Manager handles WireGuard tunnel lifecycle without Docker.
type Manager struct {
	mu          sync.Mutex
	tunnels     map[string]*Tunnel
	nextPort    int
	downloadDir string
}

func NewManager(downloadDir string) *Manager {
	return &Manager{
		tunnels:     make(map[string]*Tunnel),
		nextPort:    6800,
		downloadDir: downloadDir,
	}
}

// CheckDependencies verifies that required tools are installed.
func CheckDependencies() error {
	required := []string{"wg", "aria2c"}

	// wireguard-go is needed on macOS; on Linux kernel WireGuard may suffice
	// but we'll use wireguard-go for portability
	required = append(required, "wireguard-go")

	var missing []string
	for _, tool := range required {
		if _, err := exec.LookPath(tool); err != nil {
			missing = append(missing, tool)
		}
	}

	if len(missing) > 0 {
		hint := installHint(missing)
		return fmt.Errorf("missing required tools: %s\n%s", strings.Join(missing, ", "), hint)
	}
	return nil
}

func installHint(missing []string) string {
	switch runtime.GOOS {
	case "darwin":
		return "Install with: brew install wireguard-go wireguard-tools aria2"
	case "linux":
		return "Install with: sudo apt install wireguard-tools wireguard-go aria2\n" +
			"  Or: sudo pacman -S wireguard-tools aria2"
	default:
		return "Please install: wireguard-go, wireguard-tools, aria2"
	}
}

// StartTunnel creates a WireGuard interface, configures it, and starts aria2c bound to it.
func (m *Manager) StartTunnel(ctx context.Context, name string, wgConfigContents string) (*Tunnel, error) {
	m.mu.Lock()
	port := m.nextPort
	m.nextPort++
	m.mu.Unlock()

	tunnelCtx, cancel := context.WithCancel(ctx)

	// Parse the WireGuard config to extract Address
	address := parseAddress(wgConfigContents)
	if address == "" {
		cancel()
		return nil, fmt.Errorf("could not parse Address from WireGuard config")
	}

	// Generate interface name
	ifaceName := m.generateIfaceName(name)

	// Write config to a temp file (wg setconf needs a file)
	// Strip [Interface] Address and DNS lines since wg setconf doesn't understand them
	strippedConfig := stripInterfaceExtras(wgConfigContents)
	confFile, err := writeTempConfig(name, strippedConfig)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("writing temp config: %w", err)
	}

	// Start wireguard-go
	wgCmd, actualIface, err := startWireGuardGo(tunnelCtx, ifaceName)
	if err != nil {
		cancel()
		os.Remove(confFile)
		return nil, fmt.Errorf("starting wireguard-go: %w", err)
	}

	// Configure the interface with wg setconf
	if err := configureInterface(tunnelCtx, actualIface, confFile, address); err != nil {
		if wgCmd.Process != nil {
			wgCmd.Process.Kill()
		}
		cancel()
		removeInterface(actualIface)
		os.Remove(confFile)
		return nil, fmt.Errorf("configuring interface: %w", err)
	}
	os.Remove(confFile) // no longer needed

	// Start aria2c bound to this interface
	aria2Cmd, err := startAria2c(tunnelCtx, actualIface, port, m.downloadDir)
	if err != nil {
		if wgCmd.Process != nil {
			wgCmd.Process.Kill()
		}
		cancel()
		removeInterface(actualIface)
		return nil, fmt.Errorf("starting aria2c: %w", err)
	}

	t := &Tunnel{
		Name:      name,
		Interface: actualIface,
		VPNConfig: name,
		RPCPort:   port,
		Aria2Cmd:  aria2Cmd,
		WGCmd:     wgCmd,
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

	// Kill wireguard-go
	if t.WGCmd != nil && t.WGCmd.Process != nil {
		t.WGCmd.Process.Kill()
		t.WGCmd.Wait()
	}

	// Remove the interface
	removeInterface(t.Interface)
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

func (m *Manager) generateIfaceName(name string) string {
	if runtime.GOOS == "darwin" {
		// macOS uses utun interfaces; wireguard-go will auto-assign
		return "utun"
	}
	// Linux: use wg-aria-<name> (max 15 chars for interface name)
	iface := fmt.Sprintf("wga-%s", sanitize(name))
	if len(iface) > 15 {
		iface = iface[:15]
	}
	return iface
}

func sanitize(s string) string {
	result := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + 32 // lowercase
		}
		return '-'
	}, s)
	return strings.Trim(result, "-")
}

// startWireGuardGo launches wireguard-go for the given interface name.
// On macOS, wireguard-go auto-assigns a utun interface.
func startWireGuardGo(ctx context.Context, ifaceName string) (*exec.Cmd, string, error) {
	// Ensure the UAPI socket directory exists — wireguard-go won't create it
	if err := os.MkdirAll("/var/run/wireguard", 0755); err != nil {
		return nil, "", fmt.Errorf("creating /var/run/wireguard: %w (try running with sudo)", err)
	}

	// Write output to a temp log file so we can read it while the process runs
	// (avoids concurrency issues with pipes + Wait)
	logFile, err := os.CreateTemp("", "wireguard-go-*.log")
	if err != nil {
		return nil, "", fmt.Errorf("creating log file: %w", err)
	}
	logPath := logFile.Name()

	cmd := exec.CommandContext(ctx, "wireguard-go", ifaceName)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	// WG_TUN_NAME_FILE: wireguard-go writes the actual interface name here
	tmpFile := filepath.Join(os.TempDir(), fmt.Sprintf("wg-tun-%s-%d", ifaceName, time.Now().UnixNano()))
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("WG_TUN_NAME_FILE=%s", tmpFile),
		"WG_PROCESS_FOREGROUND=1",
		"LOG_LEVEL=debug",
	)

	if err := cmd.Start(); err != nil {
		logFile.Close()
		os.Remove(logPath)
		return nil, "", fmt.Errorf("failed to start wireguard-go: %w", err)
	}

	readLog := func() string {
		data, _ := os.ReadFile(logPath)
		s := strings.TrimSpace(string(data))
		if s == "" {
			return "no output"
		}
		return s
	}

	processExited := func() bool {
		// Signal 0 checks if process is still alive without sending a signal
		if cmd.Process == nil {
			return true
		}
		err := cmd.Process.Signal(syscall.Signal(0))
		return err != nil
	}

	actualIface := ifaceName

	// Wait for wireguard-go to create the interface (up to 10 seconds)
	for i := 0; i < 100; i++ {
		time.Sleep(100 * time.Millisecond)

		// Check if wireguard-go died early
		if processExited() {
			cmd.Wait() // reap the process
			logFile.Close()
			output := readLog()
			os.Remove(logPath)
			os.Remove(tmpFile)
			return nil, "", fmt.Errorf("wireguard-go exited early\nOutput: %s\n\nHint: wireguard-go needs root. Try: sudo ./aria-tui", output)
		}

		// Parse output for interface name like "INFO: (utun3) ..."
		for _, line := range strings.Split(readLog(), "\n") {
			if idx := strings.Index(line, "("); idx >= 0 {
				if end := strings.Index(line[idx:], ")"); end >= 0 {
					parsed := line[idx+1 : idx+end]
					if parsed != "" {
						actualIface = parsed
					}
				}
			}
		}

		// Check WG_TUN_NAME_FILE (most reliable for macOS)
		if data, readErr := os.ReadFile(tmpFile); readErr == nil {
			actual := strings.TrimSpace(string(data))
			if actual != "" {
				actualIface = actual
			}
		}

		// Check if the UAPI socket exists (means wireguard-go is ready)
		socketPath := fmt.Sprintf("/var/run/wireguard/%s.sock", actualIface)
		if _, statErr := os.Stat(socketPath); statErr == nil {
			os.Remove(tmpFile)
			os.Remove(logPath)
			logFile.Close()
			return cmd, actualIface, nil
		}
	}

	os.Remove(tmpFile)

	// Socket never appeared — collect diagnostics
	output := readLog()
	logFile.Close()
	os.Remove(logPath)

	// List what's actually in /var/run/wireguard for debugging
	var sockDir string
	if entries, dirErr := os.ReadDir("/var/run/wireguard"); dirErr == nil {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		if len(names) > 0 {
			sockDir = fmt.Sprintf(" (dir contains: %s)", strings.Join(names, ", "))
		} else {
			sockDir = " (dir is empty)"
		}
	} else {
		sockDir = fmt.Sprintf(" (dir error: %v)", dirErr)
	}

	cmd.Process.Kill()
	cmd.Wait()

	return nil, "", fmt.Errorf("wireguard-go started but UAPI socket never appeared\nExpected: /var/run/wireguard/%s.sock%s\nInterface: %s\nOutput: %s\n\nHint: wireguard-go needs root. Try: sudo ./aria-tui", actualIface, sockDir, actualIface, output)
}

// configureInterface sets up the WireGuard interface with wg and assigns IP.
func configureInterface(ctx context.Context, iface string, confFile string, address string) error {
	// Apply WireGuard configuration, retrying since wireguard-go may still be initializing
	var lastErr error
	for attempt := 0; attempt < 10; attempt++ {
		cmd := exec.CommandContext(ctx, "wg", "setconf", iface, confFile)
		out, err := cmd.CombinedOutput()
		if err == nil {
			lastErr = nil
			break
		}
		lastErr = fmt.Errorf("wg setconf: %s: %w", strings.TrimSpace(string(out)), err)
		time.Sleep(500 * time.Millisecond)
	}
	if lastErr != nil {
		return lastErr
	}

	// Parse address for IP assignment
	ip, ipNet, err := net.ParseCIDR(address)
	if err != nil {
		// Try without CIDR
		ip = net.ParseIP(address)
		if ip == nil {
			return fmt.Errorf("invalid address: %s", address)
		}
		ipNet = &net.IPNet{IP: ip, Mask: net.CIDRMask(32, 32)}
	}

	// Assign IP address to interface
	switch runtime.GOOS {
	case "darwin":
		// macOS: ifconfig utunX inet <ip> <ip> (point-to-point)
		ifCmd := exec.CommandContext(ctx, "ifconfig", iface, "inet", ip.String(), ip.String())
		if out, err := ifCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("ifconfig: %s: %w", strings.TrimSpace(string(out)), err)
		}

		// Set MTU
		mtuCmd := exec.CommandContext(ctx, "ifconfig", iface, "mtu", "1420")
		mtuCmd.Run() // best effort

	case "linux":
		// Linux: ip addr add <cidr> dev <iface>
		ones, _ := ipNet.Mask.Size()
		cidr := fmt.Sprintf("%s/%d", ip.String(), ones)
		addrCmd := exec.CommandContext(ctx, "ip", "addr", "add", cidr, "dev", iface)
		if out, err := addrCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("ip addr add: %s: %w", strings.TrimSpace(string(out)), err)
		}

		// Bring interface up
		upCmd := exec.CommandContext(ctx, "ip", "link", "set", iface, "up")
		if out, err := upCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("ip link set up: %s: %w", strings.TrimSpace(string(out)), err)
		}

		// Set MTU
		mtuCmd := exec.CommandContext(ctx, "ip", "link", "set", iface, "mtu", "1420")
		mtuCmd.Run() // best effort
	}

	return nil
}

// startAria2c launches an aria2c process bound to the given interface.
func startAria2c(ctx context.Context, iface string, port int, downloadDir string) (*exec.Cmd, error) {
	args := []string{
		"--enable-rpc=true",
		"--rpc-listen-all=true",
		fmt.Sprintf("--rpc-listen-port=%d", port),
		"--rpc-allow-origin-all=true",
		fmt.Sprintf("--dir=%s", downloadDir),
		fmt.Sprintf("--interface=%s", iface),
		"--file-allocation=falloc",
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

	cmd := exec.CommandContext(ctx, "aria2c", args...)
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start aria2c: %w", err)
	}

	// Wait a bit for aria2c to start listening
	time.Sleep(500 * time.Millisecond)

	return cmd, nil
}

// removeInterface removes a network interface.
func removeInterface(iface string) {
	switch runtime.GOOS {
	case "darwin":
		// On macOS, killing wireguard-go removes the utun
		// But we can also try to bring it down
		exec.Command("ifconfig", iface, "down").Run()
	case "linux":
		exec.Command("ip", "link", "delete", iface).Run()
	}
}

// parseAddress extracts the Address field from a WireGuard config.
func parseAddress(config string) string {
	for _, line := range strings.Split(config, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(line), "address") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				addr := strings.TrimSpace(parts[1])
				// Handle comma-separated addresses (take first IPv4)
				for _, a := range strings.Split(addr, ",") {
					a = strings.TrimSpace(a)
					if strings.Contains(a, ".") { // IPv4
						return a
					}
				}
				return addr
			}
		}
	}
	return ""
}

// stripInterfaceExtras removes lines that wg setconf doesn't understand
// (Address, DNS, MTU, etc. from [Interface] section).
func stripInterfaceExtras(config string) string {
	var result strings.Builder
	inInterface := false

	for _, line := range strings.Split(config, "\n") {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)

		if trimmed == "[Interface]" {
			inInterface = true
			result.WriteString(line + "\n")
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			inInterface = false
		}

		if inInterface {
			// Skip lines that wg setconf doesn't understand
			if strings.HasPrefix(lower, "address") ||
				strings.HasPrefix(lower, "dns") ||
				strings.HasPrefix(lower, "mtu") ||
				strings.HasPrefix(lower, "preup") ||
				strings.HasPrefix(lower, "postup") ||
				strings.HasPrefix(lower, "predown") ||
				strings.HasPrefix(lower, "postdown") ||
				strings.HasPrefix(lower, "table") ||
				strings.HasPrefix(lower, "saveconfig") {
				continue
			}
		}

		result.WriteString(line + "\n")
	}
	return result.String()
}

// writeTempConfig writes a WireGuard config to a temp file.
func writeTempConfig(name string, content string) (string, error) {
	dir := os.TempDir()
	path := filepath.Join(dir, fmt.Sprintf("aria-tui-wg-%s-%d.conf", sanitize(name), time.Now().UnixNano()))
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		return "", err
	}
	return path, nil
}
