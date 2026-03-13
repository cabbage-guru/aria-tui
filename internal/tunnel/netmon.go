package tunnel

import (
	"log"
	"net"
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/device"
)

// networkMonitor watches for host network changes (WiFi switch, VPN toggle, etc.)
// and calls BindUpdate() on all active WireGuard devices to rebind their UDP sockets.
type networkMonitor struct {
	mu      sync.Mutex
	devices map[string]*device.Device // tunnel name -> device
	lastIP  string
	stop    chan struct{}
}

func newNetworkMonitor() *networkMonitor {
	return &networkMonitor{
		devices: make(map[string]*device.Device),
		stop:    make(chan struct{}),
	}
}

func (nm *networkMonitor) add(name string, dev *device.Device) {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	nm.devices[name] = dev
}

func (nm *networkMonitor) remove(name string) {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	delete(nm.devices, name)
}

// run polls for network changes every few seconds.
func (nm *networkMonitor) run() {
	nm.lastIP = defaultSourceIP()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-nm.stop:
			return
		case <-ticker.C:
			current := defaultSourceIP()
			if current != "" && current != nm.lastIP {
				log.Printf("[netmon] network change detected: %s -> %s, rebinding tunnels", nm.lastIP, current)
				nm.rebindAll()
				nm.lastIP = current
			}
		}
	}
}

func (nm *networkMonitor) close() {
	close(nm.stop)
}

func (nm *networkMonitor) rebindAll() {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	for name, dev := range nm.devices {
		if err := dev.BindUpdate(); err != nil {
			log.Printf("[netmon] rebind %s failed: %v", name, err)
		}
	}
}

// defaultSourceIP discovers the host's current default route source IP by
// performing a non-connected UDP "dial" to a public address. No packets are
// sent — this just asks the kernel which source IP it would use.
func defaultSourceIP() string {
	conn, err := net.DialTimeout("udp4", "1.1.1.1:80", 2*time.Second)
	if err != nil {
		return ""
	}
	defer conn.Close()
	addr := conn.LocalAddr().(*net.UDPAddr)
	return addr.IP.String()
}
