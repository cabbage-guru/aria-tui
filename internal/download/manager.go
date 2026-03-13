package download

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cabbage-guru/aria-tui/internal/config"
	"github.com/cabbage-guru/aria-tui/internal/history"
	"github.com/cabbage-guru/aria-tui/internal/tunnel"
	"github.com/cabbage-guru/aria-tui/internal/vpn"
)

// Status represents the state of a download.
type Status int

const (
	StatusQueued Status = iota
	StatusStarting
	StatusDownloading
	StatusComplete
	StatusError
	StatusStale
	StatusCancelled
)

const cooldownDuration = 5 * time.Minute

func (s Status) String() string {
	switch s {
	case StatusQueued:
		return "Queued"
	case StatusStarting:
		return "Starting"
	case StatusDownloading:
		return "Downloading"
	case StatusComplete:
		return "Complete"
	case StatusError:
		return "Error"
	case StatusStale:
		return "Stale"
	case StatusCancelled:
		return "Cancelled"
	default:
		return "Unknown"
	}
}

// Download represents a single download task.
type Download struct {
	ID            string
	URL           string
	Filename      string
	Status        Status
	Error         string
	VPNConfig     string
	Interface     string // WireGuard interface name
	GID           string
	RPCPort       int
	TotalSize     int64
	CompletedSize int64
	Speed         int64
	StartedAt     time.Time
	CompletedAt   time.Time
	LastProgress  time.Time // last time we saw bytes increase
	LastBytes     int64     // bytes at last progress check
	StaleNotified bool
	RateLimited   bool // true if this download hit a 429
	Retries       int  // number of times this download was re-queued due to tunnel failures
}

func (d *Download) Progress() float64 {
	if d.TotalSize == 0 {
		return 0
	}
	return float64(d.CompletedSize) / float64(d.TotalSize) * 100
}

func (d *Download) SpeedStr() string {
	return formatBytes(d.Speed) + "/s"
}

func (d *Download) TotalStr() string {
	return formatBytes(d.TotalSize)
}

func (d *Download) CompletedStr() string {
	return formatBytes(d.CompletedSize)
}

// StaleDuration returns how long this download has been without progress.
func (d *Download) StaleDuration() time.Duration {
	if d.LastProgress.IsZero() {
		return 0
	}
	return time.Since(d.LastProgress)
}

func formatBytes(b int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)
	switch {
	case b >= GB:
		return fmt.Sprintf("%.2f GB", float64(b)/float64(GB))
	case b >= MB:
		return fmt.Sprintf("%.2f MB", float64(b)/float64(MB))
	case b >= KB:
		return fmt.Sprintf("%.2f KB", float64(b)/float64(KB))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// Manager coordinates downloads across WireGuard tunnels.
type Manager struct {
	mu              sync.RWMutex
	downloads       []*Download
	queue           []string // IDs of queued downloads
	rpcClients      map[int]*Aria2Client // rpcPort -> cached client (reuses http.Client connection pool)
	lastTeardown    time.Time // when the most recent tunnel was torn down
	peakBeforeFree  bool      // true if we were at MaxConcurrent when the last slot freed
	cfg             *config.Config
	vpnPool         *vpn.Pool
	tunnelMgr       *tunnel.Manager
	hist            *history.Store
	nextID          int
	ctx             context.Context
	cancel          context.CancelFunc
}

func NewManager(cfg *config.Config, vpnPool *vpn.Pool, tunnelMgr *tunnel.Manager, hist *history.Store) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		downloads:  make([]*Download, 0),
		queue:      make([]string, 0),
		rpcClients: make(map[int]*Aria2Client),
		cfg:        cfg,
		vpnPool:    vpnPool,
		tunnelMgr:  tunnelMgr,
		hist:       hist,
		ctx:        ctx,
		cancel:     cancel,
	}
}

// getOrCreateClient returns a cached Aria2Client for the given port, creating one if needed.
func (m *Manager) getOrCreateClient(port int) *Aria2Client {
	if c, ok := m.rpcClients[port]; ok {
		return c
	}
	c := NewAria2Client(port, tunnel.RPCSecret(port))
	m.rpcClients[port] = c
	return c
}

// Add queues a new download.
func (m *Manager) Add(url string) *Download {
	m.mu.Lock()
	m.nextID++
	dl := &Download{
		ID:     fmt.Sprintf("dl-%d", m.nextID),
		URL:    url,
		Status: StatusQueued,
	}
	m.downloads = append(m.downloads, dl)
	m.queue = append(m.queue, dl.ID)
	m.mu.Unlock()

	m.saveQueue()
	return dl
}

// Start begins processing the download queue.
func (m *Manager) Start() {
	go m.processLoop()
	go m.monitorLoop()
}

// Stop gracefully shuts down: records interrupted downloads to history, then tears down tunnels.
func (m *Manager) Stop() {
	m.cancel()

	type interruptedDL struct {
		dl      *Download
		vpnName string
	}
	m.mu.Lock()
	var interrupted []interruptedDL
	for _, dl := range m.downloads {
		switch dl.Status {
		case StatusDownloading, StatusStarting, StatusQueued, StatusStale:
			vpnName := dl.VPNConfig
			dl.Status = StatusCancelled
			dl.Error = "interrupted by shutdown"
			interrupted = append(interrupted, interruptedDL{dl, vpnName})
		}
	}
	m.mu.Unlock()

	for _, item := range interrupted {
		m.recordHistory(item.dl, item.vpnName)
	}

	// Clear persisted queue since all downloads were recorded to history
	os.Remove(config.QueuePath())

	m.tunnelMgr.StopAll(context.Background())
}

// Downloads returns a copy of all downloads.
func (m *Manager) Downloads() []*Download {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]*Download, len(m.downloads))
	for i, d := range m.downloads {
		cp := *d
		result[i] = &cp
	}
	return result
}

// ActiveCount returns the number of currently active downloads.
func (m *Manager) ActiveCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	count := 0
	for _, d := range m.downloads {
		if d.Status == StatusStarting || d.Status == StatusDownloading {
			count++
		}
	}
	return count
}

// Cancel cancels a download.
func (m *Manager) Cancel(id string) {
	m.mu.Lock()
	var vpnName string
	for _, d := range m.downloads {
		if d.ID == id {
			d.Status = StatusCancelled
			vpnName = d.VPNConfig
			d.VPNConfig = ""
			break
		}
	}
	m.mu.Unlock()
	if vpnName != "" {
		go m.cleanupTunnel(vpnName)
	}
	m.saveQueue()
}

// Restart restarts a stale or failed download.
func (m *Manager) Restart(id string) {
	m.mu.Lock()
	var vpnName string
	for _, d := range m.downloads {
		if d.ID == id && (d.Status == StatusStale || d.Status == StatusError) {
			vpnName = d.VPNConfig

			d.Status = StatusQueued
			d.Error = ""
			d.GID = ""
			d.RPCPort = 0
			d.VPNConfig = ""
			d.Interface = ""
			d.Retries = 0
			d.StaleNotified = false
			d.RateLimited = false
			d.LastProgress = time.Time{}
			d.LastBytes = 0
			m.queue = append(m.queue, d.ID)
			break
		}
	}
	m.mu.Unlock()
	if vpnName != "" {
		go m.cleanupTunnel(vpnName)
	}
}

// Remove removes a download from the list.
func (m *Manager) Remove(id string) {
	m.mu.Lock()
	var vpnName string
	for i, d := range m.downloads {
		if d.ID == id {
			vpnName = d.VPNConfig
			d.VPNConfig = ""
			m.downloads = append(m.downloads[:i], m.downloads[i+1:]...)
			break
		}
	}

	// Remove from queue too
	for i, qid := range m.queue {
		if qid == id {
			m.queue = append(m.queue[:i], m.queue[i+1:]...)
			break
		}
	}
	m.mu.Unlock()
	if vpnName != "" {
		go m.cleanupTunnel(vpnName)
	}
	m.saveQueue()
}

// StaleTimeout returns the configured stale timeout duration.
func (m *Manager) StaleTimeout() time.Duration {
	return time.Duration(m.cfg.StaleTimeoutMins) * time.Minute
}

// PeerGraceRemaining returns how long until the peer teardown grace period expires.
// Returns 0 if no grace period is active (i.e., safe to start new tunnels immediately).
func (m *Manager) PeerGraceRemaining() time.Duration {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.peakBeforeFree {
		return 0
	}
	remaining := peerTeardownGrace - time.Since(m.lastTeardown)
	if remaining < 0 {
		return 0
	}
	return remaining
}

func (m *Manager) processLoop() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.processQueue()
		}
	}
}

func (m *Manager) processQueue() {
	m.mu.Lock()
	active := 0
	for _, d := range m.downloads {
		if d.Status == StatusStarting || d.Status == StatusDownloading {
			active++
		}
	}

	if active >= m.cfg.MaxConcurrent || len(m.queue) == 0 {
		m.mu.Unlock()
		return
	}

	// If we just freed a slot from a full pool, wait for the VPN server to drop
	// the old peer before opening a new tunnel on the same provider account.
	if m.peakBeforeFree && time.Since(m.lastTeardown) < peerTeardownGrace {
		m.mu.Unlock()
		return
	}

	// Get next queued download
	dlID := m.queue[0]
	m.queue = m.queue[1:]

	var dl *Download
	for _, d := range m.downloads {
		if d.ID == dlID && d.Status == StatusQueued {
			dl = d
			break
		}
	}
	if dl == nil {
		m.mu.Unlock()
		return
	}

	dl.Status = StatusStarting
	dl.StartedAt = time.Now()
	m.mu.Unlock()

	// Acquire a VPN config
	wgCfg := m.vpnPool.Acquire()
	if wgCfg == nil {
		// No VPN available right now — put it back in the queue
		m.mu.Lock()
		dl.Status = StatusQueued
		m.queue = append(m.queue, dl.ID)
		m.mu.Unlock()
		return
	}

	// Start WireGuard tunnel + aria2c
	tun, err := m.tunnelMgr.StartTunnel(m.ctx, wgCfg.Name, wgCfg.Contents)
	if err != nil {
		m.vpnPool.Release(wgCfg.Name)
		m.mu.Lock()
		dl.Retries++
		if dl.Retries >= 5 {
			dl.Status = StatusError
			dl.Error = fmt.Sprintf("Tunnel start failed after %d retries: %v", dl.Retries, err)
			m.mu.Unlock()
			m.recordHistory(dl, wgCfg.Name)
		} else {
			dl.Status = StatusQueued
			dl.Error = fmt.Sprintf("Tunnel start failed (retry %d/5): %v", dl.Retries, err)
			m.queue = append(m.queue, dl.ID)
			m.mu.Unlock()
		}
		return
	}

	m.mu.Lock()
	dl.VPNConfig = wgCfg.Name
	dl.Interface = tun.Interface
	dl.RPCPort = tun.RPCPort
	m.mu.Unlock()

	// Wait for aria2c RPC to become ready
	client := NewAria2Client(tun.RPCPort, tunnel.RPCSecret(tun.RPCPort))
	ready := false
	for i := 0; i < 15; i++ {
		select {
		case <-m.ctx.Done():
			return
		default:
		}
		if _, err := client.GetVersion(); err == nil {
			ready = true
			break
		}
		time.Sleep(time.Second)
	}

	if !ready {
		// Read aria2c log for diagnostics
		errMsg := "aria2c RPC not ready after 15s"
		if tun.Aria2Log != "" {
			if logData, err := os.ReadFile(tun.Aria2Log); err == nil {
				if out := strings.TrimSpace(string(logData)); out != "" {
					errMsg += "\naria2c output: " + out
				}
			}
		}
		m.vpnPool.Release(wgCfg.Name)
		m.tunnelMgr.StopTunnel(m.ctx, wgCfg.Name)
		m.mu.Lock()
		dl.Retries++
		if dl.Retries >= 5 {
			dl.Status = StatusError
			dl.Error = fmt.Sprintf("%s (failed after %d retries)", errMsg, dl.Retries)
			m.mu.Unlock()
			m.recordHistory(dl, wgCfg.Name)
		} else {
			dl.Status = StatusQueued
			dl.Error = fmt.Sprintf("%s (retry %d/5)", errMsg, dl.Retries)
			m.queue = append(m.queue, dl.ID)
			m.mu.Unlock()
		}
		return
	}

	// Add the download
	gid, err := client.AddURI(dl.URL)
	if err != nil {
		m.vpnPool.Release(wgCfg.Name)
		m.tunnelMgr.StopTunnel(m.ctx, wgCfg.Name)
		m.mu.Lock()
		dl.Status = StatusError
		dl.Error = fmt.Sprintf("Failed to add URL: %v", err)
		m.mu.Unlock()
		m.recordHistory(dl, wgCfg.Name)
		return
	}

	m.mu.Lock()
	dl.GID = gid
	dl.Status = StatusDownloading
	dl.LastProgress = time.Now()
	dl.Retries = 0
	dl.Error = ""
	m.mu.Unlock()
}

func (m *Manager) monitorLoop() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.updateStatuses()
		}
	}
}

// isRateLimited checks if an aria2c error indicates a 429 Too Many Requests.
func isRateLimited(errorCode, errorMessage string) bool {
	if strings.Contains(errorMessage, "429") {
		return true
	}
	if strings.Contains(strings.ToLower(errorMessage), "too many requests") {
		return true
	}
	if strings.Contains(strings.ToLower(errorMessage), "rate limit") {
		return true
	}
	return false
}

func (m *Manager) updateStatuses() {
	// Snapshot active downloads info under RLock — only read stable identifiers.
	type activeInfo struct {
		dl      *Download
		gid     string
		rpcPort int
	}

	m.mu.RLock()
	var active []activeInfo
	for _, d := range m.downloads {
		if d.Status == StatusDownloading && d.GID != "" && d.RPCPort != 0 {
			active = append(active, activeInfo{dl: d, gid: d.GID, rpcPort: d.RPCPort})
		}
	}
	m.mu.RUnlock()

	for _, info := range active {
		client := m.getOrCreateClient(info.rpcPort)
		status, err := client.TellStatus(info.gid)
		if err != nil {
			continue
		}

		m.mu.Lock()
		dl := info.dl

		// Recheck: download may have been cancelled/removed while we were polling.
		if dl.Status != StatusDownloading {
			m.mu.Unlock()
			continue
		}

		dl.TotalSize = status.TotalLength
		dl.CompletedSize = status.CompletedLength
		dl.Speed = status.DownloadSpeed

		if len(status.Files) > 0 && status.Files[0].Path != "" {
			dl.Filename = filepath.Base(status.Files[0].Path)
		}

		switch status.Status {
		case "complete":
			dl.Status = StatusComplete
			dl.CompletedAt = time.Now()
			vpnName := dl.VPNConfig
			dl.VPNConfig = ""
			m.mu.Unlock()
			go m.cleanupTunnel(vpnName)
			m.recordHistory(dl, vpnName)
			continue

		case "error":
			// Detect 429 rate limiting: cooldown the VPN and retry on a different one
			if isRateLimited(status.ErrorCode, status.ErrorMessage) {
				dl.RateLimited = true
				vpnName := dl.VPNConfig
				m.vpnPool.SetCooldown(vpnName, cooldownDuration)

				// Re-queue for retry on a different VPN
				dl.Status = StatusQueued
				dl.Error = fmt.Sprintf("Rate limited (429) on %s - retrying on different VPN", vpnName)
				dl.GID = ""
				dl.RPCPort = 0
				dl.VPNConfig = ""
				dl.Interface = ""
				dl.StaleNotified = false
				dl.LastProgress = time.Time{}
				dl.LastBytes = 0
				m.queue = append(m.queue, dl.ID)
				m.mu.Unlock()
				go m.cleanupTunnel(vpnName)
				continue
			}

			dl.Status = StatusError
			dl.Error = fmt.Sprintf("aria2 error %s: %s", status.ErrorCode, status.ErrorMessage)
			vpnName := dl.VPNConfig
			dl.VPNConfig = ""
			m.mu.Unlock()
			go m.cleanupTunnel(vpnName)
			m.recordHistory(dl, vpnName)
			continue

		case "active":
			// Check for stale (no progress for stale_timeout_mins)
			if dl.CompletedSize > dl.LastBytes {
				dl.LastProgress = time.Now()
				dl.LastBytes = dl.CompletedSize
			}

			staleDuration := time.Duration(m.cfg.StaleTimeoutMins) * time.Minute
			if time.Since(dl.LastProgress) > staleDuration && !dl.StaleNotified {
				dl.Status = StatusStale
				dl.StaleNotified = true
			}
		}
		m.mu.Unlock()
	}
}

// peerTeardownGrace is how long we wait after tearing down a tunnel that was at the
// max-concurrent limit before starting a new one. This gives the VPN server time to
// expire the old WireGuard peer session (handshake timeout is typically 2-5 min, but
// most providers notice within ~90s). We only apply this delay when we were actually
// at the concurrency ceiling — otherwise there's no risk of exceeding it.
const peerTeardownGrace = 90 * time.Second

// cleanupTunnel tears down the tunnel and releases the VPN config back to the pool.
// The vpnName must be captured by the caller before clearing dl.VPNConfig to avoid races.
func (m *Manager) cleanupTunnel(vpnName string) {
	if vpnName == "" {
		return
	}
	// Evict cached RPC client for the tunnel's port before tearing it down.
	tun := m.tunnelMgr.GetTunnel(vpnName)
	if tun != nil {
		m.mu.Lock()
		delete(m.rpcClients, tun.RPCPort)
		m.mu.Unlock()
	}
	m.tunnelMgr.StopTunnel(context.Background(), vpnName)
	m.vpnPool.Release(vpnName)

	// Record teardown time so processQueue can enforce the grace period.
	m.mu.Lock()
	active := 0
	for _, d := range m.downloads {
		if d.Status == StatusStarting || d.Status == StatusDownloading {
			active++
		}
	}
	// We were "at peak" if, before this teardown freed a slot, we were at max.
	// active is already decremented (status was changed before cleanupTunnel is called),
	// so active+1 was the count before the slot freed.
	m.peakBeforeFree = (active + 1) >= m.cfg.MaxConcurrent
	m.lastTeardown = time.Now()
	m.mu.Unlock()
}

// saveQueue persists all queued/active URLs to disk so they survive restarts.
func (m *Manager) saveQueue() {
	m.mu.RLock()
	urls := make([]string, 0)
	for _, d := range m.downloads {
		switch d.Status {
		case StatusQueued, StatusStarting, StatusDownloading, StatusStale:
			urls = append(urls, d.URL)
		}
	}
	m.mu.RUnlock()

	data, err := json.Marshal(urls)
	if err != nil {
		return
	}
	os.WriteFile(config.QueuePath(), data, 0600)
}

// LoadQueue re-adds URLs that were persisted from a previous session.
func (m *Manager) LoadQueue() int {
	data, err := os.ReadFile(config.QueuePath())
	if err != nil {
		return 0
	}
	var urls []string
	if err := json.Unmarshal(data, &urls); err != nil {
		return 0
	}
	for _, u := range urls {
		m.Add(u)
	}
	// Clear the file now that they're loaded (Add will re-persist them)
	return len(urls)
}

// recordHistory writes a download result to history. vpnName must be captured
// by the caller before clearing dl.VPNConfig to avoid recording an empty value.
func (m *Manager) recordHistory(dl *Download, vpnName string) {
	entry := history.Entry{
		URL:         dl.URL,
		Filename:    dl.Filename,
		VPNConfig:   vpnName,
		StartedAt:   dl.StartedAt,
		CompletedAt: dl.CompletedAt,
		TotalSize:   dl.TotalSize,
	}

	switch dl.Status {
	case StatusComplete:
		entry.Status = "complete"
	case StatusError:
		entry.Status = "error"
		entry.Error = dl.Error
	case StatusCancelled:
		entry.Status = "cancelled"
	default:
		entry.Status = "failed"
		entry.Error = dl.Error
	}

	m.hist.Add(entry)
	m.saveQueue()
}
