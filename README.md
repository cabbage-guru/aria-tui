# aria-tui

A terminal user interface for downloading files through WireGuard VPN connections. Each download is routed through a separate WireGuard tunnel using userspace `wireguard-go`, with `aria2c` bound to each interface. No Docker or virtualization required.

## Features

- **VPN-routed downloads**: Each download gets its own WireGuard tunnel interface with aria2c bound to it
- **Connection pool**: Manage hundreds of WireGuard configs, with configurable concurrent connection limit (default: 5)
- **Live progress**: Real-time download speed, progress bars, and status updates
- **Stale detection**: Downloads with no progress for 5+ minutes (configurable) are flagged as stale with option to restart
- **Download stability**: aria2c handles retries, resume, and multi-connection downloads automatically
- **History tracking**: Complete history of downloaded, failed, and cancelled files
- **Cross-platform**: Works on macOS and Linux — no Docker, no VMs, no kernel modules
- **Easy VPN management**: Import configs from files/directories, manage through TUI or CLI

## Prerequisites

**macOS:**
```bash
brew install wireguard-go wireguard-tools aria2
```

**Linux (Debian/Ubuntu):**
```bash
sudo apt install wireguard-tools wireguard-go aria2
```

**Linux (Arch):**
```bash
sudo pacman -S wireguard-tools aria2
# wireguard-go from AUR if needed
```

Verify with:
```bash
aria-tui check
```

## Quick Start

```bash
# Build
make build

# Import your WireGuard configs
./aria-tui import-dir /path/to/your/wireguard/configs/

# Run (requires sudo for WireGuard interface creation)
sudo ./aria-tui
```

> **Note:** Creating WireGuard interfaces requires root/sudo. On macOS, `wireguard-go` needs permission to create utun devices.

## VPN Configuration

### Importing configs

```bash
# Import individual files
./aria-tui import config1.conf config2.conf

# Import all .conf files from a directory
./aria-tui import-dir /path/to/configs/

# Or use make
make import-vpn DIR=/path/to/configs/
```

### Manual config management

Configs are stored in `~/.config/aria-tui/wireguard/`. Drop `.conf` files there directly and press `R` in the VPN tab to reload.

### Config format

Standard WireGuard configuration format:

```ini
[Interface]
PrivateKey = <your-private-key>
Address = 10.0.0.2/24
DNS = 1.1.1.1

[Peer]
PublicKey = <server-public-key>
Endpoint = vpn-server.example.com:51820
AllowedIPs = 0.0.0.0/0
PersistentKeepalive = 25
```

## TUI Controls

| Key | Action |
|-----|--------|
| `1-4` | Switch tabs (Downloads, VPN Pool, History, Settings) |
| `Tab`/`Shift+Tab` | Next/previous tab |
| `j`/`k` or `Up`/`Down` | Navigate items |
| `a` | Add URL (Downloads) or VPN config (VPN Pool) |
| `i` | Import VPN config from file path |
| `r` | Restart stale/failed download |
| `c` | Cancel active download |
| `d` | Delete selected item |
| `R` | Reload VPN configs from disk |
| `m` | Change max concurrent downloads (Settings) |
| `s` | Change stale timeout (Settings) |
| `C` | Clear history (History tab) |
| `q` | Quit |

## Architecture

```
┌──────────────────────────────────────────┐
│              aria-tui (TUI)              │
│  ┌──────────┐ ┌──────────┐ ┌─────────┐  │
│  │ Download  │ │ VPN Pool │ │ History │  │
│  │ Manager   │ │ Manager  │ │ Store   │  │
│  └─────┬─────┘ └─────┬────┘ └─────────┘  │
│        │              │                   │
│  ┌─────▼──────────────▼───────────────┐  │
│  │         Tunnel Manager             │  │
│  └──┬──────────┬──────────┬───────────┘  │
└─────┼──────────┼──────────┼──────────────┘
      │          │          │
 ┌────▼───┐ ┌───▼────┐ ┌───▼────┐
 │ utun0  │ │ utun1  │ │ utun2  │   wireguard-go
 │ wg cfg1│ │ wg cfg2│ │ wg cfg3│   interfaces
 │ aria2c │ │ aria2c │ │ aria2c │   bound processes
 └────────┘ └────────┘ └────────┘
```

Each download:
1. Acquires a WireGuard config from the pool
2. `wireguard-go` creates a userspace tunnel interface (utun on macOS, wg on Linux)
3. `wg setconf` applies the WireGuard configuration
4. `aria2c --interface=<iface>` binds all download traffic to that tunnel
5. Traffic is encrypted and routed through the VPN endpoint
6. Interface is torn down when download completes or is cancelled

## Settings

Configuration is stored in `~/.config/aria-tui/config.json`:

| Setting | Default | Description |
|---------|---------|-------------|
| `max_concurrent` | 5 | Maximum simultaneous VPN+download connections |
| `stale_timeout_mins` | 5 | Minutes of no progress before marking download as stale |
| `download_dir` | `~/Downloads/aria-tui` | Where files are saved |

## How it works without Docker

Instead of running containers, aria-tui uses:

- **`wireguard-go`** — Userspace WireGuard implementation. Creates tunnel interfaces without needing kernel modules. Works in VMs, containers, and anywhere you can run a binary.
- **`wg setconf`** — Standard WireGuard tools to configure the tunnel with your VPN provider's settings.
- **`aria2c --interface=<iface>`** — Binds the download process to the specific WireGuard interface, ensuring all traffic goes through the VPN.

This means it works on:
- Bare metal Linux/macOS
- UTM virtual machines
- Any Linux VM (no nested virtualization needed)
- Cloud instances
