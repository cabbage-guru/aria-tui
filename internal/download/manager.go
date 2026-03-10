package download

import (
	"context"
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
	mu        sync.RWMutex
	downloads []*Download
	queue     []string // IDs of queued downloads
	cfg       *config.Config
	vpnPool   *vpn.Pool
	tunnelMgr *tunnel.Manager
	hist      *history.Store
	nextID    int
	ctx       context.Context
	cancel    context.CancelFunc
}

func NewManager(cfg *config.Config, vpnPool *vpn.Pool, tunnelMgr *tunnel.Manager, hist *history.Store) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		downloads: make([]*Download, 0),
		queue:     make([]string, 0),
		cfg:       cfg,
		vpnPool:   vpnPool,
		tunnelMgr: tunnelMgr,
		hist:      hist,
		ctx:       ctx,
		cancel:    cancel,
	}
}

// Add queues a new download.
func (m *Manager) Add(url string) *Download {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.nextID++
	dl := &Download{
		ID:     fmt.Sprintf("dl-%d", m.nextID),
		URL:    url,
		Status: StatusQueued,
	}
	m.downloads = append(m.downloads, dl)
	m.queue = append(m.queue, dl.ID)

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

	m.mu.Lock()
	interrupted := make([]*Download, 0)
	for _, dl := range m.downloads {
		switch dl.Status {
		case StatusDownloading, StatusStarting, StatusQueued, StatusStale:
			dl.Status = StatusCancelled
			dl.Error = "interrupted by shutdown"
			interrupted = append(interrupted, dl)
		}
	}
	m.mu.Unlock()

	for _, dl := range interrupted {
		m.recordHistory(dl)
	}

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
	defer m.mu.Unlock()

	for _, d := range m.downloads {
		if d.ID == id {
			d.Status = StatusCancelled
			go m.cleanupDownload(d)
			break
		}
	}
}

// Restart restarts a stale or failed download.
func (m *Manager) Restart(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, d := range m.downloads {
		if d.ID == id && (d.Status == StatusStale || d.Status == StatusError) {
			// Clean up old tunnel
			go m.cleanupDownload(d)

			d.Status = StatusQueued
			d.Error = ""
			d.GID = ""
			d.RPCPort = 0
			d.VPNConfig = ""
			d.Interface = ""
			d.StaleNotified = false
			d.RateLimited = false
			d.LastProgress = time.Time{}
			d.LastBytes = 0
			m.queue = append(m.queue, d.ID)
			break
		}
	}
}

// Remove removes a download from the list.
func (m *Manager) Remove(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i, d := range m.downloads {
		if d.ID == id {
			if d.Status == StatusStarting || d.Status == StatusDownloading {
				go m.cleanupDownload(d)
			}
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
}

// StaleTimeout returns the configured stale timeout duration.
func (m *Manager) StaleTimeout() time.Duration {
	return time.Duration(m.cfg.StaleTimeoutMins) * time.Minute
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
		dl.Status = StatusError
		dl.Error = fmt.Sprintf("Tunnel start failed: %v", err)
		m.mu.Unlock()
		m.recordHistory(dl)
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
		dl.Status = StatusError
		dl.Error = errMsg
		m.mu.Unlock()
		m.recordHistory(dl)
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
		m.recordHistory(dl)
		return
	}

	m.mu.Lock()
	dl.GID = gid
	dl.Status = StatusDownloading
	dl.LastProgress = time.Now()
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
	m.mu.RLock()
	active := make([]*Download, 0)
	for _, d := range m.downloads {
		if d.Status == StatusDownloading && d.GID != "" && d.RPCPort != 0 {
			active = append(active, d)
		}
	}
	m.mu.RUnlock()

	for _, dl := range active {
		client := NewAria2Client(dl.RPCPort, tunnel.RPCSecret(dl.RPCPort))
		status, err := client.TellStatus(dl.GID)
		if err != nil {
			continue
		}

		m.mu.Lock()
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
			go m.cleanupDownload(dl)
			m.mu.Unlock()
			m.recordHistory(dl)
			continue

		case "error":
			// Detect 429 rate limiting: cooldown the VPN and retry on a different one
			if isRateLimited(status.ErrorCode, status.ErrorMessage) {
				dl.RateLimited = true
				rateLimitedVPN := dl.VPNConfig
				m.vpnPool.SetCooldown(rateLimitedVPN, cooldownDuration)
				go m.cleanupDownload(dl)

				// Re-queue for retry on a different VPN
				dl.Status = StatusQueued
				dl.Error = fmt.Sprintf("Rate limited (429) on %s - retrying on different VPN", rateLimitedVPN)
				dl.GID = ""
				dl.RPCPort = 0
				dl.VPNConfig = ""
				dl.Interface = ""
				dl.StaleNotified = false
				dl.LastProgress = time.Time{}
				dl.LastBytes = 0
				m.queue = append(m.queue, dl.ID)
				m.mu.Unlock()
				continue
			}

			dl.Status = StatusError
			dl.Error = fmt.Sprintf("aria2 error %s: %s", status.ErrorCode, status.ErrorMessage)
			go m.cleanupDownload(dl)
			m.mu.Unlock()
			m.recordHistory(dl)
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

func (m *Manager) cleanupDownload(dl *Download) {
	if dl.VPNConfig != "" {
		m.tunnelMgr.StopTunnel(context.Background(), dl.VPNConfig)
		m.vpnPool.Release(dl.VPNConfig)
	}
}

func (m *Manager) recordHistory(dl *Download) {
	entry := history.Entry{
		URL:         dl.URL,
		Filename:    dl.Filename,
		VPNConfig:   dl.VPNConfig,
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
}
