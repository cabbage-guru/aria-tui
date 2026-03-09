package tunnel

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/netip"
	"strings"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

// UserspaceWG is a WireGuard tunnel running entirely in userspace via netstack.
// No kernel interfaces, no routing tables, no sudo needed for networking.
// Encrypted UDP packets naturally route through the host's network stack
// (including any parent VPN).
type UserspaceWG struct {
	device *device.Device
	tnet   *netstack.Net
}

// NewUserspaceWG creates a userspace WireGuard tunnel from a standard .conf file.
func NewUserspaceWG(config string) (*UserspaceWG, error) {
	// Parse config
	localAddr, err := parseNetAddr(config)
	if err != nil {
		return nil, fmt.Errorf("parse address: %w", err)
	}

	dnsAddrs := parseDNSAddrs(config)
	if len(dnsAddrs) == 0 {
		dnsAddrs = []netip.Addr{netip.MustParseAddr("1.1.1.1")}
	}

	log.Printf("[wg-userspace] creating netstack TUN (addr=%s, dns=%v)", localAddr, dnsAddrs)

	// Create userspace TUN backed by netstack (gvisor TCP/IP stack)
	tun, tnet, err := netstack.CreateNetTUN(
		[]netip.Addr{localAddr},
		dnsAddrs,
		1420, // MTU
	)
	if err != nil {
		return nil, fmt.Errorf("create netstack tun: %w", err)
	}

	log.Printf("[wg-userspace] creating WireGuard device")

	// Create WireGuard device
	dev := device.NewDevice(tun, conn.NewDefaultBind(), device.NewLogger(device.LogLevelVerbose, "wg: "))

	// Convert .conf to IPC format and apply
	ipcConf, err := confToIPC(config)
	if err != nil {
		dev.Close()
		return nil, fmt.Errorf("convert config to IPC: %w", err)
	}

	log.Printf("[wg-userspace] applying IPC config")

	if err := dev.IpcSet(ipcConf); err != nil {
		dev.Close()
		return nil, fmt.Errorf("ipc set: %w", err)
	}

	log.Printf("[wg-userspace] bringing device up")

	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, fmt.Errorf("device up: %w", err)
	}

	log.Printf("[wg-userspace] device up, tunnel ready")

	return &UserspaceWG{
		device: dev,
		tnet:   tnet,
	}, nil
}

// DialContext creates a TCP connection through the WireGuard tunnel.
func (u *UserspaceWG) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return u.tnet.DialContext(ctx, network, address)
}

// Close shuts down the WireGuard device.
func (u *UserspaceWG) Close() {
	if u.device != nil {
		u.device.Close()
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

// parseNetAddr extracts the Address field as a netip.Addr.
func parseNetAddr(config string) (netip.Addr, error) {
	addrStr := parseAddress(config)
	if addrStr == "" {
		return netip.Addr{}, fmt.Errorf("no Address found in config")
	}
	// Strip CIDR suffix if present
	if idx := strings.Index(addrStr, "/"); idx >= 0 {
		addrStr = addrStr[:idx]
	}
	return netip.ParseAddr(addrStr)
}

// parseDNSAddrs extracts DNS addresses from the config.
func parseDNSAddrs(config string) []netip.Addr {
	var addrs []netip.Addr
	for _, line := range strings.Split(config, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(line), "dns") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				for _, d := range strings.Split(parts[1], ",") {
					d = strings.TrimSpace(d)
					if addr, err := netip.ParseAddr(d); err == nil {
						addrs = append(addrs, addr)
					}
				}
			}
		}
	}
	return addrs
}

// confToIPC converts a standard WireGuard .conf file to the IPC format
// expected by device.IpcSet().
func confToIPC(config string) (string, error) {
	var lines []string

	for _, line := range strings.Split(config, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		lower := strings.ToLower(line)

		if lower == "[interface]" || lower == "[peer]" {
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(strings.ToLower(parts[0]))
		value := strings.TrimSpace(parts[1])

		switch key {
		case "privatekey":
			hexKey, err := base64ToHex(value)
			if err != nil {
				return "", fmt.Errorf("invalid PrivateKey: %w", err)
			}
			lines = append(lines, "private_key="+hexKey)

		case "listenport":
			lines = append(lines, "listen_port="+value)

		case "publickey":
			hexKey, err := base64ToHex(value)
			if err != nil {
				return "", fmt.Errorf("invalid PublicKey: %w", err)
			}
			lines = append(lines, "public_key="+hexKey)

		case "presharedkey":
			hexKey, err := base64ToHex(value)
			if err != nil {
				return "", fmt.Errorf("invalid PresharedKey: %w", err)
			}
			lines = append(lines, "preshared_key="+hexKey)

		case "endpoint":
			lines = append(lines, "endpoint="+value)

		case "allowedips":
			for _, cidr := range strings.Split(value, ",") {
				cidr = strings.TrimSpace(cidr)
				if cidr != "" {
					lines = append(lines, "allowed_ip="+cidr)
				}
			}

		case "persistentkeepalive":
			lines = append(lines, "persistent_keepalive_interval="+value)

		// Skip Interface-only fields
		case "address", "dns", "mtu", "table", "preup", "postup", "predown", "postdown", "saveconfig":
			continue
		}
	}

	return strings.Join(lines, "\n") + "\n", nil
}

// base64ToHex converts a base64-encoded WireGuard key to hex.
func base64ToHex(b64 string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}
