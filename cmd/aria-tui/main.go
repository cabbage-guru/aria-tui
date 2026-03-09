package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
	// Test URL: small file from a reliable source
	testURL := "https://speed.cloudflare.com/100kB.bin"

	fmt.Println("=== aria-tui end-to-end test ===")
	fmt.Println()

	// Step 1: Check dependencies
	fmt.Print("[1/6] Checking dependencies... ")
	if err := tunnel.CheckDependencies(); err != nil {
		fmt.Printf("FAIL\n  %v\n", err)
		os.Exit(1)
	}
	fmt.Println("OK")

	// Step 2: Load VPN configs
	fmt.Print("[2/6] Loading VPN configs... ")
	pool := vpn.NewPool()
	if err := pool.LoadConfigs(); err != nil {
		fmt.Printf("FAIL\n  %v\n", err)
		os.Exit(1)
	}
	if pool.Available() == 0 {
		fmt.Printf("FAIL\n  No VPN configs available. Import some first:\n")
		fmt.Printf("  aria-tui import <file.conf>\n")
		os.Exit(1)
	}
	fmt.Printf("OK (%d available)\n", pool.Available())

	// Step 3: Acquire a VPN config
	fmt.Print("[3/6] Acquiring VPN config... ")
	wgCfg := pool.Acquire()
	if wgCfg == nil {
		fmt.Println("FAIL\n  No VPN config could be acquired")
		os.Exit(1)
	}
	fmt.Printf("OK (%s)\n", wgCfg.Name)
	defer pool.Release(wgCfg.Name)

	// Step 4: Start tunnel
	fmt.Print("[4/6] Starting WireGuard tunnel + aria2c... ")
	ctx := context.Background()
	tmpDir, _ := os.MkdirTemp("", "aria-tui-test-*")
	defer os.RemoveAll(tmpDir)

	tunnelMgr := tunnel.NewManager(tmpDir)
	defer tunnelMgr.StopAll(ctx)

	tun, err := tunnelMgr.StartTunnel(ctx, wgCfg.Name, wgCfg.Contents)
	if err != nil {
		fmt.Printf("FAIL\n  %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("OK (iface=%s, port=%d)\n", tun.Interface, tun.RPCPort)

	// Step 5: Wait for RPC
	fmt.Print("[5/6] Waiting for aria2c RPC... ")
	client := download.NewAria2Client(tun.RPCPort, tunnel.RPCSecret(tun.RPCPort))
	ready := false
	for i := 0; i < 20; i++ {
		if ver, err := client.GetVersion(); err == nil {
			fmt.Printf("OK (aria2 v%s)\n", ver)
			ready = true
			break
		}
		time.Sleep(time.Second)
	}
	if !ready {
		// Read aria2c log for diagnostics
		errMsg := "aria2c RPC not ready after 20s"
		if tun.Aria2Log != "" {
			if logData, readErr := os.ReadFile(tun.Aria2Log); readErr == nil {
				if out := strings.TrimSpace(string(logData)); out != "" {
					errMsg += "\n  aria2c output: " + out
				}
			}
		}
		fmt.Printf("FAIL\n  %s\n", errMsg)
		os.Exit(1)
	}

	// Step 6: Download test file
	fmt.Printf("[6/6] Downloading test file (%s)... ", testURL)
	gid, err := client.AddURI(testURL)
	if err != nil {
		fmt.Printf("FAIL\n  AddURI error: %v\n", err)
		os.Exit(1)
	}

	// Poll until complete or error (max 60s)
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(time.Second)
		status, err := client.TellStatus(gid)
		if err != nil {
			continue
		}
		switch status.Status {
		case "complete":
			filename := ""
			if len(status.Files) > 0 && status.Files[0].Path != "" {
				filename = filepath.Base(status.Files[0].Path)
			}
			fmt.Printf("OK\n")
			fmt.Println()
			fmt.Println("=== Test passed! ===")
			fmt.Printf("  File:      %s\n", filename)
			fmt.Printf("  Size:      %d bytes\n", status.CompletedLength)
			fmt.Printf("  Interface: %s\n", tun.Interface)
			fmt.Printf("  VPN:       %s\n", wgCfg.Name)
			return
		case "error":
			fmt.Printf("FAIL\n  aria2c error: [%s] %s\n", status.ErrorCode, status.ErrorMessage)
			os.Exit(1)
		}
	}
	fmt.Println("FAIL\n  Download timed out after 60s")
	os.Exit(1)
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
