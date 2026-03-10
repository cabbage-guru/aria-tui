package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/atotto/clipboard"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/cabbage-guru/aria-tui/internal/config"
	"github.com/cabbage-guru/aria-tui/internal/download"
	"github.com/cabbage-guru/aria-tui/internal/history"
	"github.com/cabbage-guru/aria-tui/internal/ipc"
	"github.com/cabbage-guru/aria-tui/internal/tui"
	"github.com/cabbage-guru/aria-tui/internal/tunnel"
	"github.com/cabbage-guru/aria-tui/internal/vpn"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "import":
			handleImport()
			return
		case "import-dir":
			handleImportDir()
			return
		case "list-vpn":
			handleListVPN()
			return
		case "clip":
			handleClip()
			return
		case "check":
			handleCheck()
			return
		case "cleanup":
			handleCleanup()
			return
		case "test":
			handleTest()
			return
		case "help", "--help", "-h":
			printHelp()
			return
		}
	}

	runTUI()
}

func printHelp() {
	fmt.Println(`aria-tui - Download files through WireGuard VPN tunnels

Usage:
  aria-tui                     Start the TUI
  aria-tui clip                Paste clipboard URL to running TUI (for global hotkeys)
  aria-tui import <file>...    Import WireGuard .conf files
  aria-tui import-dir <dir>    Import all .conf files from a directory
  aria-tui list-vpn            List all VPN configurations
  aria-tui check               Check that dependencies are installed
  aria-tui cleanup             Kill orphaned aria2c processes and remove stale files
  aria-tui test                Test download through a VPN tunnel

TUI Controls:
  1-4          Switch tabs (Downloads, VPN Pool, History, Settings)
  Tab/Arrow    Navigate tabs and items
  j/k          Move cursor up/down
  a            Add (URL in Downloads tab, VPN config in VPN tab)
  Ctrl+V       Paste URL from clipboard and queue download (any tab)
  i            Import VPN config from file path (VPN tab)
  r            Restart stale/failed download
  c            Cancel active download
  d            Delete selected item
  R            Reload VPN configs from disk (VPN tab)
  m            Change max concurrent downloads (Settings tab)
  s            Change stale timeout (Settings tab)
  q            Quit

Prerequisites:
  brew install wireguard-go wireguard-tools aria2    (macOS)
  sudo apt install wireguard-tools wireguard-go aria2 (Linux)

Config directory: ~/.config/aria-tui/
VPN configs:      ~/.config/aria-tui/wireguard/`)
}

func handleClip() {
	text, err := clipboard.ReadAll()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading clipboard: %v\n", err)
		os.Exit(1)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		fmt.Fprintln(os.Stderr, "Clipboard is empty")
		os.Exit(1)
	}
	u, err := url.Parse(text)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		fmt.Fprintf(os.Stderr, "Clipboard does not contain a valid URL: %s\n", text)
		os.Exit(1)
	}
	if err := ipc.Send(text); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Queued: %s\n", text)
}

func handleCleanup() {
	fmt.Println("Cleaning up orphaned aria-tui resources...")

	// 1. Kill orphaned aria2c processes spawned by aria-tui
	killOrphanedAria2c()

	// 2. Remove stale IPC socket
	sockPath := config.SocketPath()
	if _, err := os.Stat(sockPath); err == nil {
		// Check if a TUI is actually listening
		conn, err := net.DialTimeout("unix", sockPath, 500*time.Millisecond)
		if err != nil {
			// Nobody listening — stale socket
			os.Remove(sockPath)
			fmt.Printf("  Removed stale socket: %s\n", sockPath)
		} else {
			conn.Close()
			fmt.Println("  Socket is active (TUI is running), skipping")
		}
	}

	// 3. Remove orphaned aria2c temp log files
	matches, _ := filepath.Glob(filepath.Join(os.TempDir(), "aria2c-*.log"))
	for _, m := range matches {
		os.Remove(m)
		fmt.Printf("  Removed temp log: %s\n", m)
	}

	// 4. Clear stale queue file
	queuePath := config.QueuePath()
	if data, err := os.ReadFile(queuePath); err == nil && len(data) > 0 {
		os.Remove(queuePath)
		fmt.Printf("  Removed stale queue: %s\n", queuePath)
	}

	fmt.Println("Done.")
}

func killOrphanedAria2c() {
	// Find aria2c processes with aria-tui RPC secret in their arguments
	out, err := exec.Command("pgrep", "-f", "aria2c.*rpc-secret=aria-tui").Output()
	if err != nil {
		// No matching processes
		return
	}

	pids := strings.Fields(strings.TrimSpace(string(out)))
	for _, pidStr := range pids {
		var pid int
		if _, err := fmt.Sscanf(pidStr, "%d", &pid); err != nil {
			continue
		}
		proc, err := os.FindProcess(pid)
		if err != nil {
			continue
		}
		if err := proc.Kill(); err != nil {
			fmt.Printf("  Failed to kill aria2c PID %d: %v\n", pid, err)
		} else {
			fmt.Printf("  Killed orphaned aria2c PID %d\n", pid)
		}
	}
}

func handleCheck() {
	fmt.Println("Checking dependencies...")
	if err := tunnel.CheckDependencies(); err != nil {
		fmt.Fprintf(os.Stderr, "\n%v\n", err)
		os.Exit(1)
	}
	fmt.Println("All dependencies found!")
}

func handleTest() {
	testURL := "https://hil-speed.hetzner.com/100MB.bin"
	ipCheckURL := "https://checkip.amazonaws.com"

	fmt.Println("=== aria-tui end-to-end test (2 tunnels) ===")
	fmt.Println()

	// Step 1: Check dependencies
	fmt.Print("[1/7] Checking dependencies... ")
	if err := tunnel.CheckDependencies(); err != nil {
		fmt.Printf("FAIL\n  %v\n", err)
		os.Exit(1)
	}
	fmt.Println("OK")

	// Step 2: Load VPN configs
	fmt.Print("[2/7] Loading VPN configs... ")
	pool := vpn.NewPool()
	if err := pool.LoadConfigs(); err != nil {
		fmt.Printf("FAIL\n  %v\n", err)
		os.Exit(1)
	}
	if pool.Available() < 2 {
		fmt.Printf("FAIL\n  Need at least 2 VPN configs, have %d. Import more:\n", pool.Available())
		fmt.Printf("  aria-tui import <file.conf>\n")
		os.Exit(1)
	}
	fmt.Printf("OK (%d available)\n", pool.Available())

	// Step 3: Acquire two VPN configs
	fmt.Print("[3/7] Acquiring 2 VPN configs... ")
	wgCfg1 := pool.Acquire()
	wgCfg2 := pool.Acquire()
	if wgCfg1 == nil || wgCfg2 == nil {
		fmt.Println("FAIL\n  Could not acquire 2 VPN configs")
		os.Exit(1)
	}
	fmt.Printf("OK (%s, %s)\n", wgCfg1.Name, wgCfg2.Name)
	defer pool.Release(wgCfg1.Name)
	defer pool.Release(wgCfg2.Name)

	// Step 4: Start both tunnels
	ctx := context.Background()
	tmpDir1, _ := os.MkdirTemp("", "aria-tui-test1-*")
	tmpDir2, _ := os.MkdirTemp("", "aria-tui-test2-*")
	defer os.RemoveAll(tmpDir1)
	defer os.RemoveAll(tmpDir2)

	tunnelMgr := tunnel.NewManager(tmpDir1)
	defer tunnelMgr.StopAll(ctx)

	fmt.Print("[4/7] Starting tunnel 1... ")
	tun1, err := tunnelMgr.StartTunnel(ctx, wgCfg1.Name, wgCfg1.Contents)
	if err != nil {
		fmt.Printf("FAIL\n  %v\n", err)
		os.Exit(1)
	}
	proxyPort1 := 0
	if tun1.Proxy != nil {
		proxyPort1 = tun1.Proxy.Port()
	}
	fmt.Printf("OK (rpc=%d, proxy=%d)\n", tun1.RPCPort, proxyPort1)

	// Switch download dir for tunnel 2 so IP check file doesn't collide
	tunnelMgr.SetDownloadDir(tmpDir2)

	fmt.Print("      Starting tunnel 2... ")
	tun2, err := tunnelMgr.StartTunnel(ctx, wgCfg2.Name, wgCfg2.Contents)
	if err != nil {
		fmt.Printf("FAIL\n  %v\n", err)
		os.Exit(1)
	}
	proxyPort2 := 0
	if tun2.Proxy != nil {
		proxyPort2 = tun2.Proxy.Port()
	}
	fmt.Printf("OK (rpc=%d, proxy=%d)\n", tun2.RPCPort, proxyPort2)

	// Step 5: Wait for both RPCs
	fmt.Print("[5/7] Waiting for aria2c RPC (tunnel 1)... ")
	client1 := download.NewAria2Client(tun1.RPCPort, tunnel.RPCSecret(tun1.RPCPort))
	if !waitForRPC(client1, tun1) {
		os.Exit(1)
	}

	fmt.Print("      Waiting for aria2c RPC (tunnel 2)... ")
	client2 := download.NewAria2Client(tun2.RPCPort, tunnel.RPCSecret(tun2.RPCPort))
	if !waitForRPC(client2, tun2) {
		os.Exit(1)
	}

	// Step 6: Check external IPs (host + both tunnels)
	fmt.Print("[6/8] Checking host external IP... ")
	hostIP, err := getHostExternalIP(ipCheckURL)
	if err != nil {
		fmt.Printf("FAIL\n  %v\n", err)
		fmt.Println("  (continuing anyway)")
		hostIP = "unknown"
	} else {
		fmt.Printf("%s\n", hostIP)
	}

	// Test 1: Direct proxy test (IP_BOUND_IF + scoped routes)
	fmt.Print("[7/12] Direct proxy IP check (tunnel 1)... ")
	directIP1, directErr1 := getIPViaProxy(tun1.Proxy.Port(), ipCheckURL)
	if directErr1 != nil {
		fmt.Printf("FAIL: %v\n", directErr1)
	} else {
		fmt.Printf("%s\n", directIP1)
	}

	fmt.Print("       Direct proxy IP check (tunnel 2)... ")
	directIP2, directErr2 := getIPViaProxy(tun2.Proxy.Port(), ipCheckURL)
	if directErr2 != nil {
		fmt.Printf("FAIL: %v\n", directErr2)
	} else {
		fmt.Printf("%s\n", directIP2)
	}

	fmt.Print("[8/10] Checking external IP via aria2c (tunnel 1)... ")
	ip1, err := getExternalIP(client1, ipCheckURL, tmpDir1)
	if err != nil {
		fmt.Printf("FAIL\n  %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("%s\n", ip1)

	fmt.Print("       Checking external IP via aria2c (tunnel 2)... ")
	ip2, err := getExternalIP(client2, ipCheckURL, tmpDir2)
	if err != nil {
		fmt.Printf("FAIL\n  %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("%s\n", ip2)

	fmt.Println()

	// Print diagnostics
	fmt.Println("--- IP comparison ---")
	fmt.Printf("  Host:     %s\n", hostIP)
	fmt.Printf("  Tunnel 1: %s\n", ip1)
	fmt.Printf("  Tunnel 2: %s\n", ip2)

	allSame := ip1 == hostIP && ip2 == hostIP
	tunnelsSame := ip1 == ip2

	if allSame {
		fmt.Println()
		fmt.Println("  PROBLEM: All IPs are the same!")
		fmt.Println("  Tunnels are NOT routing through VPN at all.")
	} else if tunnelsSame {
		fmt.Println()
		fmt.Printf("  WARNING: Both tunnels share the same IP (%s)\n", ip1)
		if ip1 != hostIP {
			fmt.Println("  They differ from host, so VPN is working but both use the same exit.")
		}
	} else {
		fmt.Println("  Tunnels have different IPs - isolation confirmed!")
	}

	// Print proxy stats
	fmt.Println()
	fmt.Println("--- Proxy diagnostics ---")
	if tun1.Proxy != nil {
		total, ok, fail := tun1.Proxy.Stats()
		fmt.Printf("  Proxy 1 (port %d): conns=%d dial_ok=%d dial_fail=%d\n", tun1.Proxy.Port(), total, ok, fail)
	}
	if tun2.Proxy != nil {
		total, ok, fail := tun2.Proxy.Stats()
		fmt.Printf("  Proxy 2 (port %d): conns=%d dial_ok=%d dial_fail=%d\n", tun2.Proxy.Port(), total, ok, fail)
	}

	// Step 8: Download test file through both tunnels
	fmt.Printf("\n[9/10] Downloading test file through tunnel 1... ")
	if err := testDownload(client1, testURL); err != nil {
		fmt.Printf("FAIL\n  %v\n", err)
		os.Exit(1)
	}
	fmt.Println("OK")

	fmt.Printf("      Downloading test file through tunnel 2... ")
	if err := testDownload(client2, testURL); err != nil {
		fmt.Printf("FAIL\n  %v\n", err)
		os.Exit(1)
	}
	fmt.Println("OK")

	// Post-download proxy stats
	fmt.Println()
	fmt.Println("--- Proxy stats after downloads ---")
	if tun1.Proxy != nil {
		total, ok, fail := tun1.Proxy.Stats()
		fmt.Printf("  Proxy 1 (port %d): conns=%d dial_ok=%d dial_fail=%d\n", tun1.Proxy.Port(), total, ok, fail)
	}
	if tun2.Proxy != nil {
		total, ok, fail := tun2.Proxy.Stats()
		fmt.Printf("  Proxy 2 (port %d): conns=%d dial_ok=%d dial_fail=%d\n", tun2.Proxy.Port(), total, ok, fail)
	}

	fmt.Println()
	fmt.Println("=== Test passed! ===")
	fmt.Printf("  Host:     %s\n", hostIP)
	fmt.Printf("  Tunnel 1: %s (vpn=%s)\n", ip1, wgCfg1.Name)
	fmt.Printf("  Tunnel 2: %s (vpn=%s)\n", ip2, wgCfg2.Name)
	if ip1 != ip2 {
		fmt.Println("  IPs differ: traffic is correctly routed through separate VPNs")
	}
}

// getIPViaProxy fetches external IP through the HTTP CONNECT proxy directly
// (bypassing aria2c) to verify the proxy and interface binding work.
func getIPViaProxy(proxyPort int, checkURL string) (string, error) {
	proxyURL, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", proxyPort))
	client := &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
	}
	resp, err := client.Get(checkURL)
	if err != nil {
		return "", fmt.Errorf("proxy request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}

// getHostExternalIP fetches the host's external IP directly (no tunnel).
func getHostExternalIP(checkURL string) (string, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(checkURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}


// waitForRPC polls aria2c RPC until ready, returns false on failure.
func waitForRPC(client *download.Aria2Client, tun *tunnel.Tunnel) bool {
	for i := 0; i < 20; i++ {
		if ver, err := client.GetVersion(); err == nil {
			fmt.Printf("OK (aria2 v%s)\n", ver)
			return true
		}
		time.Sleep(time.Second)
	}
	errMsg := "aria2c RPC not ready after 20s"
	if tun.Aria2Log != "" {
		if logData, readErr := os.ReadFile(tun.Aria2Log); readErr == nil {
			if out := strings.TrimSpace(string(logData)); out != "" {
				errMsg += "\n  aria2c output: " + out
			}
		}
	}
	fmt.Printf("FAIL\n  %s\n", errMsg)
	return false
}

// getExternalIP uses aria2c to download from checkip URL and reads the result.
func getExternalIP(client *download.Aria2Client, checkURL, dir string) (string, error) {
	gid, err := client.AddURI(checkURL)
	if err != nil {
		return "", fmt.Errorf("AddURI: %v", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		status, err := client.TellStatus(gid)
		if err != nil {
			continue
		}
		switch status.Status {
		case "complete":
			if len(status.Files) > 0 && status.Files[0].Path != "" {
				data, err := os.ReadFile(status.Files[0].Path)
				if err != nil {
					return "", fmt.Errorf("reading IP file: %v", err)
				}
				os.Remove(status.Files[0].Path)
				return strings.TrimSpace(string(data)), nil
			}
			return "", fmt.Errorf("no file path in completed download")
		case "error":
			return "", fmt.Errorf("aria2c error [%s]: %s", status.ErrorCode, status.ErrorMessage)
		}
	}
	return "", fmt.Errorf("timed out after 30s")
}

// testDownload downloads a file via aria2c and waits for completion.
func testDownload(client *download.Aria2Client, url string) error {
	gid, err := client.AddURI(url)
	if err != nil {
		return fmt.Errorf("AddURI: %v", err)
	}

	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		status, err := client.TellStatus(gid)
		if err != nil {
			continue
		}
		switch status.Status {
		case "complete":
			return nil
		case "error":
			return fmt.Errorf("aria2c error [%s]: %s", status.ErrorCode, status.ErrorMessage)
		}
	}
	return fmt.Errorf("timed out after 120s")
}

func handleImport() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "Usage: aria-tui import <file>...")
		os.Exit(1)
	}

	pool := vpn.NewPool()
	pool.LoadConfigs()

	for _, path := range os.Args[2:] {
		if err := pool.ImportConfig(path); err != nil {
			fmt.Fprintf(os.Stderr, "Error importing %s: %v\n", path, err)
		} else {
			fmt.Printf("Imported: %s\n", filepath.Base(path))
		}
	}

	fmt.Printf("\nTotal VPN configs: %d\n", pool.Total())
	fmt.Printf("Config directory: %s\n", config.WireGuardDir())
}

func handleImportDir() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "Usage: aria-tui import-dir <directory>")
		os.Exit(1)
	}

	dir := os.Args[2]
	entries, err := os.ReadDir(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading directory: %v\n", err)
		os.Exit(1)
	}

	pool := vpn.NewPool()
	pool.LoadConfigs()

	count := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".conf") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if err := pool.ImportConfig(path); err != nil {
			fmt.Fprintf(os.Stderr, "Error importing %s: %v\n", entry.Name(), err)
		} else {
			fmt.Printf("Imported: %s\n", entry.Name())
			count++
		}
	}

	fmt.Printf("\nImported %d configs. Total: %d\n", count, pool.Total())
	fmt.Printf("Config directory: %s\n", config.WireGuardDir())
}

func handleListVPN() {
	pool := vpn.NewPool()
	pool.LoadConfigs()

	configs := pool.List()
	if len(configs) == 0 {
		fmt.Println("No VPN configs found.")
		fmt.Printf("Add configs to: %s\n", config.WireGuardDir())
		return
	}

	fmt.Printf("VPN Configurations (%d total):\n\n", len(configs))
	for _, cfg := range configs {
		fmt.Printf("  %s\n", cfg.Name)
	}
	fmt.Printf("\nConfig directory: %s\n", config.WireGuardDir())
}

func runTUI() {
	// Load config
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}

	// Check dependencies
	if err := tunnel.CheckDependencies(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	// Ensure download directory exists
	os.MkdirAll(cfg.DownloadDir, 0755)

	// Initialize VPN pool
	vpnPool := vpn.NewPool()
	if err := vpnPool.LoadConfigs(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not load VPN configs: %v\n", err)
	}

	// Initialize tunnel manager
	tunnelMgr := tunnel.NewManager(cfg.DownloadDir)

	// Initialize history
	hist := history.NewStore()

	// Initialize download manager
	dlMgr := download.NewManager(cfg, vpnPool, tunnelMgr, hist)
	if n := dlMgr.LoadQueue(); n > 0 {
		fmt.Fprintf(os.Stderr, "Restored %d queued URL(s) from previous session\n", n)
	}
	dlMgr.Start()

	// Start IPC listener for 'aria-tui clip' commands
	ipcLn, err := ipc.NewListener()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not start IPC listener: %v\n", err)
	}
	if ipcLn != nil {
		defer ipcLn.Close()
	}

	// Create and run TUI
	model := tui.NewModel(cfg, vpnPool, dlMgr, hist, ipcLn)
	p := tea.NewProgram(model, tea.WithAltScreen())

	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error running TUI: %v\n", err)
		os.Exit(1)
	}
}
