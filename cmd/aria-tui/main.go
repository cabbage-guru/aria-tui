package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
