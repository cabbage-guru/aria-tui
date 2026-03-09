package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/cabbage-guru/aria-tui/internal/config"
	"github.com/cabbage-guru/aria-tui/internal/download"
	"github.com/cabbage-guru/aria-tui/internal/history"
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
		case "check":
			handleCheck()
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
  aria-tui import <file>...    Import WireGuard .conf files
  aria-tui import-dir <dir>    Import all .conf files from a directory
  aria-tui list-vpn            List all VPN configurations
  aria-tui check               Check that dependencies are installed
  aria-tui test                Test download through a VPN tunnel

TUI Controls:
  1-4          Switch tabs (Downloads, VPN Pool, History, Settings)
  Tab/Arrow    Navigate tabs and items
  j/k          Move cursor up/down
  a            Add (URL in Downloads tab, VPN config in VPN tab)
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
	fmt.Printf("OK (iface=%s, rpc=%d, proxy=%d)\n", tun1.Interface, tun1.RPCPort, proxyPort1)

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
	fmt.Printf("OK (iface=%s, rpc=%d, proxy=%d)\n", tun2.Interface, tun2.RPCPort, proxyPort2)

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

	// Test 2: Global routes test (wg-quick approach) on tunnel 1 only
	// This tests whether the tunnel can carry traffic at all
	fmt.Print("[8/12] Testing tunnel 1 with global routes (wg-quick style)... ")
	globalRouteIP := testGlobalRoutes(tun1.Interface, ipCheckURL)
	fmt.Printf("%s\n", globalRouteIP)

	// Test 3: curl --interface test
	fmt.Print("[9/12] Testing curl --interface utun10... ")
	curlIP := testCurlInterface(tun1.Interface, ipCheckURL)
	fmt.Printf("%s\n", curlIP)

	fmt.Print("[10/12] Checking external IP via aria2c (tunnel 1)... ")
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
		fmt.Printf("  Proxy 1 (%s:%d): conns=%d dial_ok=%d dial_fail=%d\n", tun1.Interface, tun1.Proxy.Port(), total, ok, fail)
	}
	if tun2.Proxy != nil {
		total, ok, fail := tun2.Proxy.Stats()
		fmt.Printf("  Proxy 2 (%s:%d): conns=%d dial_ok=%d dial_fail=%d\n", tun2.Interface, tun2.Proxy.Port(), total, ok, fail)
	}

	// Print routing diagnostics
	fmt.Println()
	fmt.Println("--- Routing diagnostics ---")
	printDiagnostics(tun1.Interface, tun2.Interface)

	// Step 8: Download test file through both tunnels
	fmt.Printf("\n[11/12] Downloading test file through tunnel 1... ")
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
		fmt.Printf("  Proxy 1 (%s:%d): conns=%d dial_ok=%d dial_fail=%d\n", tun1.Interface, tun1.Proxy.Port(), total, ok, fail)
	}
	if tun2.Proxy != nil {
		total, ok, fail := tun2.Proxy.Stats()
		fmt.Printf("  Proxy 2 (%s:%d): conns=%d dial_ok=%d dial_fail=%d\n", tun2.Interface, tun2.Proxy.Port(), total, ok, fail)
	}

	fmt.Println()
	fmt.Println("=== Test passed! ===")
	fmt.Printf("  Host:     %s\n", hostIP)
	fmt.Printf("  Tunnel 1: %s (iface=%s, vpn=%s)\n", ip1, tun1.Interface, wgCfg1.Name)
	fmt.Printf("  Tunnel 2: %s (iface=%s, vpn=%s)\n", ip2, tun2.Interface, wgCfg2.Name)
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

// testGlobalRoutes temporarily adds non-scoped global routes (wg-quick approach)
// for a single tunnel to verify the WireGuard tunnel can carry traffic at all.
func testGlobalRoutes(iface string, checkURL string) string {
	// Get current default gateway
	out, err := exec.Command("route", "-n", "get", "default").CombinedOutput()
	if err != nil {
		return fmt.Sprintf("FAIL (route get default: %v)", err)
	}
	var defaultGW string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "gateway:") {
			defaultGW = strings.TrimSpace(strings.TrimPrefix(line, "gateway:"))
		}
	}
	if defaultGW == "" {
		return fmt.Sprintf("FAIL (no default gateway found)\n  route get output:\n%s", string(out))
	}

	// Get WireGuard endpoint for this interface
	wgOut, err := exec.Command("sudo", "wg", "show", iface, "endpoints").CombinedOutput()
	if err != nil {
		return fmt.Sprintf("FAIL (wg show endpoints: %v)", err)
	}
	var endpointIP string
	for _, line := range strings.Split(string(wgOut), "\n") {
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			ep := parts[1]
			if idx := strings.LastIndex(ep, ":"); idx > 0 {
				endpointIP = ep[:idx]
			}
		}
	}

	// Add endpoint exclusion route (so WG encrypted packets still reach the endpoint)
	if endpointIP != "" {
		exec.Command("sudo", "route", "add", "-host", endpointIP, defaultGW).Run()
		defer exec.Command("sudo", "route", "delete", "-host", endpointIP).Run()
	}

	// Add global split-default routes
	exec.Command("sudo", "route", "add", "-net", "0.0.0.0/1", "-interface", iface).Run()
	exec.Command("sudo", "route", "add", "-net", "128.0.0.0/1", "-interface", iface).Run()
	defer exec.Command("sudo", "route", "delete", "-net", "0.0.0.0/1", "-interface", iface).Run()
	defer exec.Command("sudo", "route", "delete", "-net", "128.0.0.0/1", "-interface", iface).Run()

	// Now try to get external IP - ALL traffic should go through the tunnel
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(checkURL)
	if err != nil {
		return fmt.Sprintf("FAIL (http get: %v) [gw=%s, ep=%s]", err, defaultGW, endpointIP)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	ip := strings.TrimSpace(string(body))

	// Also show WG transfer after test
	wgOut2, _ := exec.Command("sudo", "wg", "show", iface, "transfer").CombinedOutput()
	return fmt.Sprintf("%s [gw=%s, ep=%s, wg_transfer=%s]", ip, defaultGW, endpointIP, strings.TrimSpace(string(wgOut2)))
}

// testCurlInterface uses curl --interface to test interface binding.
func testCurlInterface(iface string, checkURL string) string {
	out, err := exec.Command("curl", "-s", "--max-time", "10", "--interface", iface, checkURL).CombinedOutput()
	if err != nil {
		return fmt.Sprintf("FAIL (%v: %s)", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out))
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

// printDiagnostics dumps routing and WireGuard state for debugging.
func printDiagnostics(ifaces ...string) {
	// Show default route
	fmt.Println("  route -n get default:")
	if out, err := exec.Command("route", "-n", "get", "default").CombinedOutput(); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			fmt.Printf("    %s\n", line)
		}
	}

	// Show WireGuard status
	fmt.Println("  wg show:")
	if out, err := exec.Command("sudo", "wg", "show").CombinedOutput(); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			fmt.Printf("    %s\n", line)
		}
	} else {
		fmt.Printf("    (error: %v)\n", err)
	}

	// Show ip rules
	fmt.Println("  ip rule list:")
	if out, err := exec.Command("ip", "rule", "list").CombinedOutput(); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			fmt.Printf("    %s\n", line)
		}
	}

	// Show routing tables for our tunnels (51820+)
	for i, iface := range ifaces {
		tableID := 51820 + i
		fmt.Printf("  ip route show table %d (iface %s):\n", tableID, iface)
		if out, err := exec.Command("ip", "route", "show", "table", fmt.Sprintf("%d", tableID)).CombinedOutput(); err == nil {
			output := strings.TrimSpace(string(out))
			if output == "" {
				fmt.Println("    (empty)")
			} else {
				for _, line := range strings.Split(output, "\n") {
					fmt.Printf("    %s\n", line)
				}
			}
		}
	}

	// Show interface addresses (use ifconfig on macOS, ip on Linux)
	for _, iface := range ifaces {
		if runtime.GOOS == "darwin" {
			fmt.Printf("  ifconfig %s:\n", iface)
			if out, err := exec.Command("ifconfig", iface).CombinedOutput(); err == nil {
				for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
					fmt.Printf("    %s\n", line)
				}
			} else {
				fmt.Printf("    (error: %v)\n", err)
			}
		} else {
			fmt.Printf("  ip addr show %s:\n", iface)
			if out, err := exec.Command("ip", "addr", "show", iface).CombinedOutput(); err == nil {
				for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
					fmt.Printf("    %s\n", line)
				}
			} else {
				fmt.Printf("    (error: %v)\n", err)
			}
		}
	}

	// On macOS, show route table for debugging
	if runtime.GOOS == "darwin" {
		fmt.Println("  netstat -rn (relevant routes):")
		if out, err := exec.Command("netstat", "-rn").CombinedOutput(); err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				for _, iface := range ifaces {
					if strings.Contains(line, iface) {
						fmt.Printf("    %s\n", line)
					}
				}
			}
		}
	}
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
	dlMgr.Start()

	// Create and run TUI
	model := tui.NewModel(cfg, vpnPool, dlMgr, hist)
	p := tea.NewProgram(model, tea.WithAltScreen())

	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error running TUI: %v\n", err)
		os.Exit(1)
	}
}
