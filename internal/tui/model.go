package tui

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/atotto/clipboard"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/cabbage-guru/aria-tui/internal/config"
	"github.com/cabbage-guru/aria-tui/internal/download"
	"github.com/cabbage-guru/aria-tui/internal/history"
	"github.com/cabbage-guru/aria-tui/internal/hotkey"
	"github.com/cabbage-guru/aria-tui/internal/ipc"
	"github.com/cabbage-guru/aria-tui/internal/vpn"
)

type tab int

const (
	tabDownloads tab = iota
	tabVPN
	tabHistory
	tabSettings
)

type inputMode int

const (
	inputNone inputMode = iota
	inputAddURL
	inputAddVPN
	inputImportVPN
	inputMaxConcurrent
	inputStaleTimeout
	inputConfirmDelete
)

type tickMsg time.Time

// ipcURLMsg is sent when a URL arrives via the Unix socket from 'aria-tui clip'.
type ipcURLMsg string

type Model struct {
	cfg     *config.Config
	vpnPool *vpn.Pool
	dlMgr   *download.Manager
	hist    *history.Store

	activeTab  tab
	cursor     int
	width      int
	height     int
	inputMode inputMode
	textInput textinput.Model
	textArea  textarea.Model
	message   string
	messageAt time.Time

	// History scroll state
	historyOffset int

	// VPN config add state
	vpnAddName  string
	vpnAddPhase int // 0 = name, 1 = content

	// IPC listener for 'aria-tui clip' commands
	ipcListener *ipc.Listener
}

func NewModel(cfg *config.Config, vpnPool *vpn.Pool, dlMgr *download.Manager, hist *history.Store, ipcLn *ipc.Listener) Model {
	ti := textinput.New()
	ti.CharLimit = 2048

	ta := textarea.New()
	ta.SetWidth(80)
	ta.SetHeight(10)
	ta.CharLimit = 8192

	return Model{
		cfg:         cfg,
		vpnPool:     vpnPool,
		dlMgr:       dlMgr,
		hist:        hist,
		textInput:   ti,
		textArea:    ta,
		width:       120,
		height:      40,
		ipcListener: ipcLn,
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(tickCmd(), m.waitForIPC())
}

// waitForIPC returns a Cmd that blocks until a URL arrives on the IPC channel.
func (m Model) waitForIPC() tea.Cmd {
	if m.ipcListener == nil {
		return nil
	}
	ch := m.ipcListener.URLs()
	return func() tea.Msg {
		url, ok := <-ch
		if !ok {
			return nil
		}
		return ipcURLMsg(url)
	}
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tickMsg:
		// Clear old messages
		if !m.messageAt.IsZero() && time.Since(m.messageAt) > 5*time.Second {
			m.message = ""
		}
		return m, tickCmd()

	case ipcURLMsg:
		u := string(msg)
		m.dlMgr.Add(u)
		m.activeTab = tabDownloads
		m.setMessage(fmt.Sprintf("Queued (remote): %s", u))
		return m, m.waitForIPC()

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	if m.inputMode != inputNone {
		var cmd tea.Cmd
		if m.inputMode == inputAddVPN && m.vpnAddPhase == 1 {
			m.textArea, cmd = m.textArea.Update(msg)
		} else {
			m.textInput, cmd = m.textInput.Update(msg)
		}
		return m, cmd
	}

	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Handle input mode first
	if m.inputMode != inputNone {
		return m.handleInputKey(msg)
	}

	switch msg.String() {
	case "q", "ctrl+c":
		m.dlMgr.Stop()
		return m, tea.Quit

	case "ctrl+v":
		// Paste URL from clipboard and queue download
		text, err := clipboard.ReadAll()
		if err != nil {
			m.setMessage("Error reading clipboard")
			return m, nil
		}
		text = strings.TrimSpace(text)
		if text == "" {
			m.setMessage("Clipboard is empty")
			return m, nil
		}
		u, err := url.Parse(text)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			m.setMessage("Clipboard does not contain a valid URL")
			return m, nil
		}
		m.dlMgr.Add(text)
		m.activeTab = tabDownloads
		m.setMessage(fmt.Sprintf("Queued from clipboard: %s", text))
		return m, nil

	case "tab", "right", "l":
		m.activeTab = (m.activeTab + 1) % 4
		m.cursor = 0
		m.historyOffset = 0
		return m, nil

	case "shift+tab", "left", "h":
		m.activeTab = (m.activeTab - 1 + 4) % 4
		m.cursor = 0
		m.historyOffset = 0
		return m, nil

	case "up", "k":
		if m.activeTab == tabHistory {
			return m.handleHistoryKey(msg)
		}
		if m.cursor > 0 {
			m.cursor--
		}
		return m, nil

	case "down", "j":
		if m.activeTab == tabHistory {
			return m.handleHistoryKey(msg)
		}
		m.cursor++
		return m, nil

	case "1":
		m.activeTab = tabDownloads
		m.cursor = 0
		m.historyOffset = 0
		return m, nil
	case "2":
		m.activeTab = tabVPN
		m.cursor = 0
		m.historyOffset = 0
		return m, nil
	case "3":
		m.activeTab = tabHistory
		m.cursor = 0
		m.historyOffset = 0
		return m, nil
	case "4":
		m.activeTab = tabSettings
		m.cursor = 0
		m.historyOffset = 0
		return m, nil
	}

	// Tab-specific keys
	switch m.activeTab {
	case tabDownloads:
		return m.handleDownloadKey(msg)
	case tabVPN:
		return m.handleVPNKey(msg)
	case tabHistory:
		return m.handleHistoryKey(msg)
	case tabSettings:
		return m.handleSettingsKey(msg)
	}

	return m, nil
}

func (m Model) handleInputKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Textarea mode for VPN config content (phase 1)
	if m.inputMode == inputAddVPN && m.vpnAddPhase == 1 {
		switch msg.String() {
		case "esc":
			m.inputMode = inputNone
			m.textArea.Blur()
			m.vpnAddPhase = 0
			return m, nil
		case "ctrl+s":
			return m.submitInput()
		}
		var cmd tea.Cmd
		m.textArea, cmd = m.textArea.Update(msg)
		return m, cmd
	}

	// Single-line text input mode
	switch msg.String() {
	case "esc":
		m.inputMode = inputNone
		m.textInput.Blur()
		m.vpnAddPhase = 0
		return m, nil

	case "enter":
		return m.submitInput()
	}

	var cmd tea.Cmd
	m.textInput, cmd = m.textInput.Update(msg)
	return m, cmd
}

func (m Model) handleDownloadKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	downloads := m.dlMgr.Downloads()

	switch msg.String() {
	case "a":
		// Add URL
		m.inputMode = inputAddURL
		m.textInput.Placeholder = "Enter download URL..."
		m.textInput.SetValue("")
		m.textInput.Focus()
		return m, m.textInput.Cursor.BlinkCmd()

	case "r":
		// Restart selected
		if m.cursor < len(downloads) {
			dl := downloads[m.cursor]
			if dl.Status == download.StatusStale || dl.Status == download.StatusError {
				m.dlMgr.Restart(dl.ID)
				m.setMessage("Restarting download...")
			}
		}
		return m, nil

	case "c":
		// Cancel selected
		if m.cursor < len(downloads) {
			dl := downloads[m.cursor]
			if dl.Status == download.StatusDownloading || dl.Status == download.StatusStarting || dl.Status == download.StatusQueued {
				m.dlMgr.Cancel(dl.ID)
				m.setMessage("Download cancelled")
			}
		}
		return m, nil

	case "d", "delete":
		// Remove selected
		if m.cursor < len(downloads) {
			dl := downloads[m.cursor]
			m.dlMgr.Remove(dl.ID)
			if m.cursor > 0 {
				m.cursor--
			}
			m.setMessage("Download removed")
		}
		return m, nil
	}

	// Clamp cursor
	if m.cursor >= len(downloads) && len(downloads) > 0 {
		m.cursor = len(downloads) - 1
	}

	return m, nil
}

func (m Model) handleVPNKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	configs := m.vpnPool.List()

	switch msg.String() {
	case "a":
		// Add VPN config (name phase)
		m.inputMode = inputAddVPN
		m.vpnAddPhase = 0
		m.textInput.Placeholder = "Enter config name (e.g., us-east-1)..."
		m.textInput.SetValue("")
		m.textInput.Focus()
		return m, m.textInput.Cursor.BlinkCmd()

	case "i":
		// Import VPN config from file
		m.inputMode = inputImportVPN
		m.textInput.Placeholder = "Enter path to .conf file..."
		m.textInput.SetValue("")
		m.textInput.Focus()
		return m, m.textInput.Cursor.BlinkCmd()

	case "d", "delete":
		// Delete selected config
		if m.cursor < len(configs) {
			cfg := configs[m.cursor]
			if cfg.InUse {
				m.setMessage("Cannot delete config in use")
			} else {
				if err := m.vpnPool.RemoveConfig(cfg.Name); err != nil {
					m.setMessage(fmt.Sprintf("Error: %v", err))
				} else {
					m.setMessage(fmt.Sprintf("Removed %s", cfg.Name))
					if m.cursor > 0 {
						m.cursor--
					}
				}
			}
		}
		return m, nil

	case "e":
		// Toggle enable/disable
		if m.cursor < len(configs) {
			cfg := configs[m.cursor]
			if cfg.InUse {
				m.setMessage("Cannot disable config while in use")
			} else if cfg.Disabled {
				m.vpnPool.Enable(cfg.Name)
				m.setMessage(fmt.Sprintf("Enabled %s", cfg.Name))
			} else {
				m.vpnPool.Disable(cfg.Name)
				m.setMessage(fmt.Sprintf("Disabled %s", cfg.Name))
			}
		}
		return m, nil

	case "R":
		// Reload configs
		if err := m.vpnPool.LoadConfigs(); err != nil {
			m.setMessage(fmt.Sprintf("Error reloading: %v", err))
		} else {
			m.setMessage(fmt.Sprintf("Loaded %d configs", m.vpnPool.Total()))
		}
		return m, nil
	}

	if m.cursor >= len(configs) && len(configs) > 0 {
		m.cursor = len(configs) - 1
	}

	return m, nil
}

func (m Model) handleHistoryKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	entries := m.hist.All()
	total := len(entries)

	const windowSize = 10
	const scrollThreshold = 7 // start scrolling when cursor reaches this position

	switch msg.String() {
	case "C":
		m.hist.Clear()
		m.cursor = 0
		m.historyOffset = 0
		m.setMessage("History cleared")
		return m, nil

	case "r":
		absIdx := m.historyOffset + m.cursor
		if absIdx < total {
			e := entries[absIdx]
			m.dlMgr.Add(e.URL)
			m.setMessage(fmt.Sprintf("Re-queued: %s", e.URL))
		}
		return m, nil

	case "down", "j":
		absIdx := m.historyOffset + m.cursor
		if absIdx < total-1 {
			if m.cursor >= scrollThreshold {
				// Scroll the window forward
				m.historyOffset++
			} else {
				m.cursor++
			}
		}
		return m, nil

	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		} else if m.historyOffset > 0 {
			m.historyOffset--
		}
		return m, nil
	}

	// Clamp
	if total > 0 {
		maxOffset := total - windowSize
		if maxOffset < 0 {
			maxOffset = 0
		}
		if m.historyOffset > maxOffset {
			m.historyOffset = maxOffset
		}
		visible := total - m.historyOffset
		if visible > windowSize {
			visible = windowSize
		}
		if m.cursor >= visible {
			m.cursor = visible - 1
		}
	} else {
		m.cursor = 0
		m.historyOffset = 0
	}

	return m, nil
}

func (m Model) handleSettingsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "m":
		m.inputMode = inputMaxConcurrent
		m.textInput.Placeholder = fmt.Sprintf("Max concurrent downloads (current: %d)...", m.cfg.MaxConcurrent)
		m.textInput.SetValue("")
		m.textInput.Focus()
		return m, m.textInput.Cursor.BlinkCmd()

	case "s":
		m.inputMode = inputStaleTimeout
		m.textInput.Placeholder = fmt.Sprintf("Stale timeout minutes (current: %d)...", m.cfg.StaleTimeoutMins)
		m.textInput.SetValue("")
		m.textInput.Focus()
		return m, m.textInput.Cursor.BlinkCmd()

	case "g":
		if hotkey.IsInstalled() {
			if err := hotkey.Uninstall(); err != nil {
				m.setMessage(fmt.Sprintf("Error removing hotkey: %v", err))
			} else {
				m.setMessage("Global hotkey Quick Action removed")
			}
		} else {
			if err := hotkey.Install(); err != nil {
				m.setMessage(fmt.Sprintf("Error: %v", err))
			} else {
				m.setMessage(hotkey.Instructions())
			}
		}
		return m, nil
	}
	return m, nil
}

func (m Model) submitInput() (tea.Model, tea.Cmd) {
	value := strings.TrimSpace(m.textInput.Value())
	m.textInput.Blur()

	switch m.inputMode {
	case inputAddURL:
		if value != "" {
			m.dlMgr.Add(value)
			m.setMessage("Download queued")
		}
		m.inputMode = inputNone

	case inputAddVPN:
		if m.vpnAddPhase == 0 {
			// Got the name, now switch to textarea for content
			if value == "" {
				m.inputMode = inputNone
				return m, nil
			}
			m.vpnAddName = value
			m.vpnAddPhase = 1
			m.textArea.SetValue("")
			m.textArea.Placeholder = "Paste WireGuard config here..."
			m.textArea.Focus()
			return m, m.textArea.Cursor.BlinkCmd()
		} else {
			// Got the content from textarea
			content := strings.TrimSpace(m.textArea.Value())
			m.textArea.Blur()
			if content != "" {
				if err := m.vpnPool.AddConfig(m.vpnAddName, content); err != nil {
					m.setMessage(fmt.Sprintf("Error: %v", err))
				} else {
					m.setMessage(fmt.Sprintf("Added %q (disabled - press 'e' to enable)", m.vpnAddName))
				}
			}
			m.vpnAddPhase = 0
			m.inputMode = inputNone
		}

	case inputImportVPN:
		if value != "" {
			if err := m.vpnPool.ImportConfig(value); err != nil {
				m.setMessage(fmt.Sprintf("Error importing: %v", err))
			} else {
				m.setMessage("Config imported (disabled - press 'e' to enable)")
			}
		}
		m.inputMode = inputNone

	case inputMaxConcurrent:
		if value != "" {
			var n int
			if _, err := fmt.Sscanf(value, "%d", &n); err == nil && n > 0 && n <= 50 {
				m.cfg.MaxConcurrent = n
				m.cfg.Save()
				m.setMessage(fmt.Sprintf("Max concurrent set to %d", n))
			} else {
				m.setMessage("Invalid number (1-50)")
			}
		}
		m.inputMode = inputNone

	case inputStaleTimeout:
		if value != "" {
			var n int
			if _, err := fmt.Sscanf(value, "%d", &n); err == nil && n > 0 {
				m.cfg.StaleTimeoutMins = n
				m.cfg.Save()
				m.setMessage(fmt.Sprintf("Stale timeout set to %d minutes", n))
			} else {
				m.setMessage("Invalid number")
			}
		}
		m.inputMode = inputNone
	}

	return m, nil
}

func (m *Model) setMessage(msg string) {
	m.message = msg
	m.messageAt = time.Now()
}

// View renders the TUI.
func (m Model) View() string {
	var b strings.Builder

	// Header
	b.WriteString(m.renderHeader())
	b.WriteString("\n")

	// Tabs
	b.WriteString(m.renderTabs())
	b.WriteString("\n\n")

	// Content
	switch m.activeTab {
	case tabDownloads:
		b.WriteString(m.renderDownloads())
	case tabVPN:
		b.WriteString(m.renderVPN())
	case tabHistory:
		b.WriteString(m.renderHistory())
	case tabSettings:
		b.WriteString(m.renderSettings())
	}

	// Input area
	if m.inputMode != inputNone {
		b.WriteString("\n")
		if m.inputMode == inputAddVPN && m.vpnAddPhase == 1 {
			b.WriteString(fmt.Sprintf("  Config name: %s\n", m.vpnAddName))
			b.WriteString(inputStyle.Render(m.textArea.View()))
			b.WriteString("\n")
		} else {
			b.WriteString(inputStyle.Render(m.textInput.View()))
			b.WriteString("\n")
		}
	}

	// Messages
	if m.message != "" {
		b.WriteString("\n")
		if strings.HasPrefix(m.message, "Error") {
			b.WriteString(errorMsgStyle.Render(m.message))
		} else {
			b.WriteString(successMsgStyle.Render(m.message))
		}
	}

	// Footer help
	b.WriteString("\n")
	b.WriteString(m.renderHelp())

	return b.String()
}

func (m Model) renderHeader() string {
	title := titleStyle.Render("ARIA-TUI")
	stats := fmt.Sprintf(" VPN: %d/%d  Active: %d/%d",
		m.vpnPool.InUseCount(), m.vpnPool.Total(),
		m.dlMgr.ActiveCount(), m.cfg.MaxConcurrent)
	return headerStyle.Copy().Width(m.width).Render(title + stats)
}

func (m Model) renderTabs() string {
	tabs := []string{"[1] Downloads", "[2] VPN Pool", "[3] History", "[4] Settings"}
	rendered := make([]string, len(tabs))
	for i, t := range tabs {
		if tab(i) == m.activeTab {
			rendered[i] = activeTabStyle.Render(t)
		} else {
			rendered[i] = tabStyle.Render(t)
		}
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, rendered...)
}

func (m Model) renderDownloads() string {
	var b strings.Builder

	downloads := m.dlMgr.Downloads()
	if len(downloads) == 0 {
		b.WriteString(mutedStyle("  No downloads. Press 'a' to add a URL.\n"))
		return b.String()
	}

	for i, dl := range downloads {
		prefix := "  "
		if i == m.cursor {
			prefix = "> "
		}

		// Status indicator
		var statusStr string
		switch dl.Status {
		case download.StatusQueued:
			statusStr = statusQueuedStyle.Render("[QUEUED]")
		case download.StatusStarting:
			statusStr = statusStartingStyle.Render("[STARTING]")
		case download.StatusDownloading:
			statusStr = statusActiveStyle.Render("[DOWNLOADING]")
		case download.StatusComplete:
			statusStr = statusCompleteStyle.Render("[COMPLETE]")
		case download.StatusError:
			statusStr = statusErrorStyle.Render("[ERROR]")
		case download.StatusStale:
			statusStr = statusStaleStyle.Render("[STALE]")
		case download.StatusCancelled:
			statusStr = statusCancelledStyle.Render("[CANCELLED]")
		}

		// URL (truncated)
		url := dl.URL
		maxURL := m.width - 30
		if maxURL < 40 {
			maxURL = 40
		}
		if len(url) > maxURL {
			url = url[:maxURL-3] + "..."
		}

		displayName := url
		if dl.Filename != "" {
			displayName = fmt.Sprintf("%s  (%s)", dl.Filename, url)
		}

		line := fmt.Sprintf("%s%s %s", prefix, statusStr, displayName)
		if i == m.cursor {
			line = selectedStyle.Render(line)
		}
		b.WriteString(line + "\n")

		// Progress line for active downloads
		if dl.Status == download.StatusDownloading || dl.Status == download.StatusStale {
			bar := renderProgressBar(dl.Progress(), 30)
			eta := ""
			if dl.Speed > 0 && dl.TotalSize > 0 {
				remaining := dl.TotalSize - dl.CompletedSize
				secs := remaining / dl.Speed
				eta = fmt.Sprintf("  ETA %s", formatDuration(time.Duration(secs)*time.Second))
			}
			info := fmt.Sprintf("   %s  %s / %s  %s%s  VPN: %s",
				bar,
				dl.CompletedStr(), dl.TotalStr(),
				dl.SpeedStr(),
				eta,
				dl.VPNConfig)

			// Show time without progress for active downloads approaching stale
			staleDur := dl.StaleDuration()
			staleTimeout := m.dlMgr.StaleTimeout()
			if dl.Status == download.StatusDownloading && staleDur > 30*time.Second {
				remaining := staleTimeout - staleDur
				if remaining > 0 {
					info += fmt.Sprintf("  idle %s/%s", staleDur.Truncate(time.Second), staleTimeout.Truncate(time.Second))
				}
			}

			b.WriteString(progressBarStyle.Render(info) + "\n")
		}

		// Error message
		if dl.Status == download.StatusError && dl.Error != "" {
			b.WriteString(errorMsgStyle.Render(fmt.Sprintf("   Error: %s", dl.Error)) + "\n")
		}

		// Stale warning
		if dl.Status == download.StatusStale {
			staleDur := dl.StaleDuration().Truncate(time.Second)
			b.WriteString(statusStaleStyle.Render(fmt.Sprintf("   No progress for %s. Press 'r' to restart.", staleDur)) + "\n")
		}
	}

	return b.String()
}

func (m Model) renderVPN() string {
	var b strings.Builder

	configs := m.vpnPool.List()
	b.WriteString(fmt.Sprintf("  WireGuard Configs: %d total, %d available, %d in use\n",
		m.vpnPool.Total(), m.vpnPool.Available(), m.vpnPool.InUseCount()))
	b.WriteString(fmt.Sprintf("  Config dir: %s\n\n", config.WireGuardDir()))

	if len(configs) == 0 {
		b.WriteString(mutedStyle("  No VPN configs. Press 'a' to add or 'i' to import.\n"))
		return b.String()
	}

	for i, cfg := range configs {
		prefix := "  "
		if i == m.cursor {
			prefix = "> "
		}

		var status string
		if cfg.Disabled {
			status = vpnDisabledStyle.Render("[DISABLED]")
		} else if !cfg.CooldownUntil.IsZero() {
			remaining := time.Until(cfg.CooldownUntil).Truncate(time.Second)
			status = vpnCooldownStyle.Render(fmt.Sprintf("[COOLDOWN %s]", remaining))
		} else if cfg.InUse {
			status = vpnInUseStyle.Render("[IN USE]")
		} else {
			status = vpnAvailableStyle.Render("[AVAILABLE]")
		}

		line := fmt.Sprintf("%s%s %s", prefix, status, cfg.Name)
		if i == m.cursor {
			line = selectedStyle.Render(line)
		}
		b.WriteString(line + "\n")
	}

	return b.String()
}

func (m Model) renderHistory() string {
	var b strings.Builder

	entries := m.hist.All()
	if len(entries) == 0 {
		b.WriteString(mutedStyle("  No download history.\n"))
		return b.String()
	}

	completed := m.hist.Completed()
	failed := m.hist.Failed()
	b.WriteString(fmt.Sprintf("  Total: %d  Completed: %d  Failed: %d\n\n",
		len(entries), len(completed), len(failed)))

	windowSize := 10
	end := m.historyOffset + windowSize
	if end > len(entries) {
		end = len(entries)
	}

	for i := m.historyOffset; i < end; i++ {
		e := entries[i]
		visIdx := i - m.historyOffset
		prefix := "  "
		if visIdx == m.cursor {
			prefix = "> "
		}

		var statusStr string
		switch e.Status {
		case "complete":
			statusStr = statusCompleteStyle.Render("[OK]")
		case "error":
			statusStr = statusErrorStyle.Render("[ERR]")
		case "cancelled":
			statusStr = statusCancelledStyle.Render("[CXL]")
		default:
			statusStr = statusQueuedStyle.Render("[???]")
		}

		url := e.URL
		maxURL := m.width - 50
		if maxURL < 30 {
			maxURL = 30
		}
		if len(url) > maxURL {
			url = url[:maxURL-3] + "..."
		}

		displayName := url
		if e.Filename != "" {
			displayName = fmt.Sprintf("%s  (%s)", e.Filename, url)
		}

		timeStr := e.StartedAt.Format("2006-01-02 15:04")
		line := fmt.Sprintf("%s%s %s  %s  VPN: %s", prefix, statusStr, displayName, timeStr, e.VPNConfig)

		if visIdx == m.cursor {
			line = selectedStyle.Render(line)
		}
		b.WriteString(line + "\n")

		if e.Status == "error" && e.Error != "" {
			b.WriteString(errorMsgStyle.Render(fmt.Sprintf("     %s", e.Error)) + "\n")
		}
	}

	remaining := len(entries) - end
	if remaining > 0 {
		b.WriteString(mutedStyle(fmt.Sprintf("\n  ... and %d older entries", remaining)))
	}

	return b.String()
}

func (m Model) renderSettings() string {
	var b strings.Builder

	settings := []struct {
		key   string
		value string
		desc  string
	}{
		{"Max Concurrent (m)", fmt.Sprintf("%d", m.cfg.MaxConcurrent), "Maximum simultaneous VPN+download connections"},
		{"Stale Timeout (s)", fmt.Sprintf("%d minutes", m.cfg.StaleTimeoutMins), "Mark download as stale after no progress"},
		{"Download Dir", m.cfg.DownloadDir, "Where downloaded files are saved"},
	}

	hotkeyStatus := "Not installed"
	hotkeyDesc := "Install macOS Quick Action for 'aria-tui clip' (press g)"
	if hotkey.IsInstalled() {
		hotkeyStatus = "Installed"
		hotkeyDesc = "Remove macOS Quick Action (press g)"
	}
	settings = append(settings, struct {
		key   string
		value string
		desc  string
	}{"Global Hotkey (g)", hotkeyStatus, hotkeyDesc})

	for i, s := range settings {
		prefix := "  "
		if i == m.cursor {
			prefix = "> "
		}

		line := fmt.Sprintf("%s%-25s  %s", prefix, s.key, s.value)
		if i == m.cursor {
			line = selectedStyle.Render(line)
		}
		b.WriteString(line + "\n")
		b.WriteString(mutedStyle(fmt.Sprintf("   %s", s.desc)) + "\n")
	}

	return b.String()
}

func (m Model) renderHelp() string {
	var help string
	switch m.activeTab {
	case tabDownloads:
		help = "a:add  ctrl+v:paste URL  r:restart  c:cancel  d:remove  1-4:tabs  q:quit"
	case tabVPN:
		help = "a:add  i:import  d:delete  e:enable/disable  R:reload  1-4:tabs  q:quit"
	case tabHistory:
		help = "r:retry  C:clear history  1-4:tabs  q:quit"
	case tabSettings:
		help = "m:max concurrent  s:stale timeout  g:global hotkey  1-4:tabs  q:quit"
	}

	if m.inputMode != inputNone {
		if m.inputMode == inputAddVPN && m.vpnAddPhase == 1 {
			help = "ctrl+s:submit  esc:cancel  (paste multi-line config)"
		} else {
			help = "enter:submit  esc:cancel"
		}
	}

	return helpStyle.Render(help)
}

func renderProgressBar(percent float64, width int) string {
	if width < 5 {
		width = 5
	}
	filled := int(percent / 100 * float64(width))
	if filled > width {
		filled = width
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
	return fmt.Sprintf("[%s] %5.1f%%", bar, percent)
}

func formatDuration(d time.Duration) string {
	d = d.Truncate(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

func mutedStyle(s string) string {
	return lipgloss.NewStyle().Foreground(mutedColor).Render(s)
}
