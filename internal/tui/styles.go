package tui

import "github.com/charmbracelet/lipgloss"

var (
	// Colors
	primaryColor   = lipgloss.Color("#7C3AED")
	secondaryColor = lipgloss.Color("#06B6D4")
	successColor   = lipgloss.Color("#10B981")
	warningColor   = lipgloss.Color("#F59E0B")
	errorColor     = lipgloss.Color("#EF4444")
	staleColor     = lipgloss.Color("#F97316")
	mutedColor     = lipgloss.Color("#6B7280")
	bgColor        = lipgloss.Color("#1F2937")
	headerBg       = lipgloss.Color("#111827")

	// Styles
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(primaryColor).
			Padding(0, 1)

	headerStyle = lipgloss.NewStyle().
			Background(headerBg).
			Foreground(lipgloss.Color("#F9FAFB")).
			Bold(true).
			Padding(0, 2).
			Width(80)

	tabStyle = lipgloss.NewStyle().
			Padding(0, 2)

	activeTabStyle = lipgloss.NewStyle().
			Padding(0, 2).
			Bold(true).
			Foreground(primaryColor).
			Underline(true)

	statusQueuedStyle = lipgloss.NewStyle().
				Foreground(mutedColor)

	statusStartingStyle = lipgloss.NewStyle().
				Foreground(secondaryColor)

	statusActiveStyle = lipgloss.NewStyle().
				Foreground(successColor)

	statusCompleteStyle = lipgloss.NewStyle().
				Foreground(successColor).
				Bold(true)

	statusErrorStyle = lipgloss.NewStyle().
				Foreground(errorColor).
				Bold(true)

	statusStaleStyle = lipgloss.NewStyle().
				Foreground(staleColor).
				Bold(true)

	statusCancelledStyle = lipgloss.NewStyle().
				Foreground(mutedColor)

	progressBarStyle = lipgloss.NewStyle().
				Foreground(secondaryColor)

	selectedStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("#374151")).
			Foreground(lipgloss.Color("#F9FAFB"))

	helpStyle = lipgloss.NewStyle().
			Foreground(mutedColor).
			Padding(1, 0)

	vpnInUseStyle = lipgloss.NewStyle().
			Foreground(successColor)

	vpnAvailableStyle = lipgloss.NewStyle().
				Foreground(mutedColor)

	vpnDisabledStyle = lipgloss.NewStyle().
				Foreground(errorColor)

	vpnCooldownStyle = lipgloss.NewStyle().
				Foreground(warningColor)

	boxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(primaryColor).
			Padding(0, 1)

	inputStyle = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder()).
			BorderForeground(secondaryColor).
			Padding(0, 1)

	errorMsgStyle = lipgloss.NewStyle().
			Foreground(errorColor).
			Bold(true)

	successMsgStyle = lipgloss.NewStyle().
			Foreground(successColor).
			Bold(true)
)
