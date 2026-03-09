# aria-tui

A terminal user interface for downloading files through WireGuard VPN connections using Docker containers. Each download is routed through a separate WireGuard VPN connection running in its own Docker container with aria2c.

## Features

- **VPN-routed downloads**: Each download runs in its own Docker container with a dedicated WireGuard VPN connection
- **Connection pool**: Manage hundreds of WireGuard configs, with configurable concurrent connection limit (default: 5)
- **Live progress**: Real-time download speed, progress bars, and status updates
- **Stale detection**: Downloads with no progress for 5+ minutes (configurable) are flagged as stale with option to restart
- **Download stability**: aria2c handles retries, resume, and multi-connection downloads automatically
- **History tracking**: Complete history of downloaded, failed, and cancelled files
- **Cross-platform**: Works on Linux and macOS (requires Docker)
- **Easy VPN management**: Import configs from files/directories, manage through TUI or CLI

## Prerequisites

- [Go 1.21+](https://golang.org/dl/) (for building)
- [Docker](https://docs.docker.com/get-docker/) (must be running)

## Quick Start

```bash
# Build the binary
make build

# Build the Docker image (required on first run)
make build-image

# Import your WireGuard configs
./aria-tui import-dir /path/to/your/wireguard/configs/

# Run the TUI
./aria-tui
```

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

Configs are stored in `~/.config/aria-tui/wireguard/`. You can also just drop `.conf` files there directly and press `R` in the VPN tab to reload.

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
┌─────────────────────────────────────────┐
│              aria-tui (TUI)             │
│  ┌──────────┐ ┌──────────┐ ┌────────┐  │
│  │ Download  │ │ VPN Pool │ │History │  │
│  │ Manager   │ │ Manager  │ │ Store  │  │
│  └─────┬─────┘ └─────┬────┘ └────────┘  │
│        │              │                  │
│  ┌─────▼──────────────▼──────────────┐  │
│  │        Docker Manager             │  │
│  └──┬──────────┬──────────┬──────────┘  │
└─────┼──────────┼──────────┼─────────────┘
      │          │          │
┌─────▼────┐┌───▼─────┐┌───▼─────┐
│Container ││Container ││Container │
│ WG VPN 1 ││ WG VPN 2 ││ WG VPN 3 │
│ aria2c   ││ aria2c   ││ aria2c   │
└──────────┘└─────────┘└──────────┘
```

Each Docker container:
1. Starts a WireGuard VPN tunnel
2. Runs aria2c with JSON-RPC enabled
3. Routes all download traffic through the VPN
4. Exposes the aria2c RPC port for status monitoring

## Settings

Configuration is stored in `~/.config/aria-tui/config.json`:

| Setting | Default | Description |
|---------|---------|-------------|
| `max_concurrent` | 5 | Maximum simultaneous VPN+download connections |
| `stale_timeout_mins` | 5 | Minutes of no progress before marking download as stale |
| `download_dir` | `~/Downloads/aria-tui` | Where files are saved |
| `docker_image` | `aria-tui-vpn:latest` | Docker image name |
