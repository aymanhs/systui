package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/aymanhs/jeeves/systemd"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

func (m *Model) handleDetailKey(msg tea.KeyMsg) tea.Cmd {
	if m.activeDetail == nil {
		return nil
	}

	switch msg.String() {
	case "esc", "left", "h":
		m.stopFollowLogs()
		m.tickGeneration++ // invalidate any in-flight live tick
		m.currentView = listView
		// Refresh list when going back
		return m.fetchServicesCmd()

	case "f": // Toggle follow-logs mode
		if m.followLogs {
			m.stopFollowLogs()
			return m.triggerStatus("Stopped following logs", false)
		}
		return m.startFollowLogsCmd(m.activeDetail.Name)

	case "tab":
		if m.detailFocusedSection == focusInfo {
			m.detailFocusedSection = focusLogs
		} else {
			m.detailFocusedSection = focusInfo
		}
		return nil

	case "s": // Start
		return m.requestAction("start", m.activeDetail.Name)

	case "t": // Stop
		return m.requestAction("stop", m.activeDetail.Name)

	case "r": // Restart
		return m.requestAction("restart", m.activeDetail.Name)

	case "e": // Enable
		return m.requestAction("enable", m.activeDetail.Name)

	case "d": // Disable
		return m.requestAction("disable", m.activeDetail.Name)

	case "R": // Refresh details & logs
		return m.fetchDetailsCmd(m.activeDetail.Name)

	case "u": // Open unit file in its own view
		m.currentView = unitFileView
		m.unitFileViewport.GotoTop()
		return nil

	case "w": // Toggle log line wrapping
		m.logWrap = !m.logWrap
		m.refreshLogViewport()
		m.logViewport.GotoBottom()
		return nil

	case "]": // Increase log line limit and re-fetch
		if m.logLineLimitIdx < len(logLineChoices)-1 {
			m.logLineLimitIdx++
			return m.fetchDetailsCmd(m.activeDetail.Name)
		}
		return nil

	case "[": // Decrease log line limit and re-fetch
		if m.logLineLimitIdx > 0 {
			m.logLineLimitIdx--
			return m.fetchDetailsCmd(m.activeDetail.Name)
		}
		return nil

	case "g": // Jump to top of logs
		if m.detailFocusedSection == focusLogs {
			m.logViewport.GotoTop()
			return nil
		}

	case "G": // Jump to bottom of logs
		if m.detailFocusedSection == focusLogs {
			m.logViewport.GotoBottom()
			return nil
		}
	}

	// If Logs panel is focused, send scrolling keys to viewport
	if m.detailFocusedSection == focusLogs {
		var cmd tea.Cmd
		m.logViewport, cmd = m.logViewport.Update(msg)
		return cmd
	}

	return nil
}

func (m Model) renderDetailView() string {
	if m.activeDetail == nil {
		return DocStyle.Render("\n  Loading details...")
	}

	var s strings.Builder

	// 1. Header
	title := TitleStyle.Render("SERVICE DETAILS")
	subtitle := SubTitleStyle.Render(m.activeDetail.Name)
	s.WriteString(lipgloss.JoinHorizontal(lipgloss.Center, title, subtitle) + "\n\n")

	// 2. Responsive Panels Layout. All geometry comes from detailLayout() so the
	//    viewport, headers, and box borders agree on size — otherwise the panels
	//    overflow off the bottom of the screen and scroll the page header off the top.
	infoBoxW, infoBoxH, logBoxW, logBoxH, _, _ := m.detailLayout()

	var content string
	if m.width >= 100 {
		leftBox := m.renderInfoPanel(infoBoxW, infoBoxH)
		rightBox := m.renderLogPanel(logBoxW, logBoxH)
		content = lipgloss.JoinHorizontal(lipgloss.Top, leftBox, rightBox)
	} else {
		leftBox := m.renderInfoPanel(infoBoxW, infoBoxH)
		rightBox := m.renderLogPanel(logBoxW, logBoxH)
		content = lipgloss.JoinVertical(lipgloss.Left, leftBox, rightBox)
	}
	s.WriteString(content + "\n")

	// 3. Status Banner - always exactly 2 lines
	if m.statusMsg != "" {
		if m.statusIsErr {
			s.WriteString(ErrorBanner.Render("⚠ "+m.statusMsg) + "\n")
		} else {
			s.WriteString(SuccessBanner.Render("✔ "+m.statusMsg) + "\n")
		}
	} else {
		s.WriteString("\n\n")
	}

	// 4. Footer
	s.WriteString(m.renderDetailFooter())

	return DocStyle.Render(s.String())
}

func (m Model) renderInfoPanel(width, height int) string {
	var sb strings.Builder

	// Shared header layout: title left, meta right, single blank row below. The
	// log panel uses the exact same shape so rows align across the divider.
	innerW := width - 4
	if innerW < 10 {
		innerW = 10
	}
	title := lipgloss.NewStyle().Bold(true).Foreground(ColorSecondary).
		Render(" [ Service Properties ] ")
	meta := LogMetaStyle.Render(fmt.Sprintf("bus: %s ", m.client.Mode().String()))
	gap := innerW - lipgloss.Width(title) - lipgloss.Width(meta)
	if gap < 1 {
		gap = 1
	}
	sb.WriteString(title + strings.Repeat(" ", gap) + meta + "\n\n")

	// Helper to render key-value details. We pre-wrap the value to the column width
	// and indent continuation lines under the value column so a long Description
	// doesn't wrap flush-left under the key, leaving a ragged edge.
	//
	//   Description    : a long description that
	//                    keeps going on the next
	//                    line, aligned under the v.
	const keyColW = 12 // narrower than the old 16-wide key column
	const sep = " : "
	keyPad := keyColW + len(sep)
	valW := innerW - keyPad
	if valW < 10 {
		valW = 10
	}
	indent := strings.Repeat(" ", keyPad)

	keyColStyle := lipgloss.NewStyle().Foreground(ColorSecondary).Bold(true).Width(keyColW)

	renderDetailRow := func(key, val string) string {
		// Wrap by cell width. The value may already be styled (e.g. ActiveBadge),
		// so use ansi-aware wrap to preserve sequences across line breaks.
		wrapped := wrapDetailValue(val, valW)
		lines := strings.Split(wrapped, "\n")
		for i, ln := range lines {
			if i == 0 {
				lines[i] = keyColStyle.Render(key) + sep + DetailValStyle.Render(ln)
			} else {
				lines[i] = indent + DetailValStyle.Render(ln)
			}
		}
		return strings.Join(lines, "\n")
	}

	sb.WriteString(renderDetailRow("Description", m.activeDetail.Description) + "\n")
	sb.WriteString(renderDetailRow("Load State", m.activeDetail.LoadState) + "\n")
	sb.WriteString(renderDetailRow("Active State", m.formatActiveState(m.activeDetail.ActiveState)) + "\n")
	sb.WriteString(renderDetailRow("Sub State", m.activeDetail.SubState) + "\n")
	sb.WriteString(renderDetailRow("Enable State", m.formatEnableState(m.activeDetail.UnitFileState)) + "\n")

	// Active Since (Uptime) / Inactive Since (Downtime)
	if m.activeDetail.ActiveState == "active" {
		sb.WriteString(renderDetailRow("Active Since", formatTimestamp(m.activeDetail.ActiveEnterTimestamp)) + "\n")
	} else if m.activeDetail.ActiveState == "failed" || m.activeDetail.ActiveState == "inactive" {
		sb.WriteString(renderDetailRow("Inactive Since", formatTimestamp(m.activeDetail.ActiveExitTimestamp)) + "\n")
	}

	pidStr := "N/A"
	if m.activeDetail.MainPID > 0 {
		pidStr = fmt.Sprintf("%d", m.activeDetail.MainPID)
	}
	sb.WriteString(renderDetailRow("Main PID", pidStr) + "\n")

	// Tasks
	tasksStr := "N/A"
	if !systemd.IsUnset(m.activeDetail.TasksCurrent) {
		maxTasks := "Unlimited"
		if !systemd.IsUnset(m.activeDetail.TasksMax) {
			maxTasks = fmt.Sprintf("%d", m.activeDetail.TasksMax)
		}
		tasksStr = fmt.Sprintf("%d / %s", m.activeDetail.TasksCurrent, maxTasks)
	}
	sb.WriteString(renderDetailRow("Tasks/Threads", tasksStr) + "\n")

	// Memory
	memStr := formatMemory(m.activeDetail.MemoryCurrent)
	if !systemd.IsUnset(m.activeDetail.MemoryLimit) {
		memStr += " / " + formatMemory(m.activeDetail.MemoryLimit)
	} else if !systemd.IsUnset(m.activeDetail.MemoryCurrent) {
		memStr += " / Unlimited"
	}
	sb.WriteString(renderDetailRow("Memory Current", memStr) + "\n")
	sb.WriteString(renderDetailRow("CPU Usage", formatCPU(m.activeDetail.CPUUsageNSec)) + "\n")

	// Traffic details
	ipTraffic := "N/A"
	if !systemd.IsUnset(m.activeDetail.IPTrafficRxBytes) || !systemd.IsUnset(m.activeDetail.IPTrafficTxBytes) {
		ipTraffic = fmt.Sprintf("Rx: %s, Tx: %s",
			formatMemory(m.activeDetail.IPTrafficRxBytes),
			formatMemory(m.activeDetail.IPTrafficTxBytes))
	}
	sb.WriteString(renderDetailRow("IP Traffic", ipTraffic) + "\n")

	ioTraffic := "N/A"
	if !systemd.IsUnset(m.activeDetail.IOReadBytes) || !systemd.IsUnset(m.activeDetail.IOWriteBytes) {
		ioTraffic = fmt.Sprintf("Read: %s, Write: %s",
			formatMemory(m.activeDetail.IOReadBytes),
			formatMemory(m.activeDetail.IOWriteBytes))
	}
	sb.WriteString(renderDetailRow("I/O Traffic", ioTraffic) + "\n")

	// Exit Code
	if m.activeDetail.ActiveState == "failed" || m.activeDetail.ActiveState == "inactive" {
		if m.activeDetail.ExecMainStatus != 0 {
			sb.WriteString(renderDetailRow("Exit Status", fmt.Sprintf("status=%d code=%d", m.activeDetail.ExecMainStatus, m.activeDetail.ExecMainCode)) + "\n")
		}
	}

	// Apply box styling based on focus. width/height are OUTER dims (border-inclusive);
	// lipgloss .Width()/.Height() are INNER (content) dims so we subtract the border.
	style := BoxStyle
	if m.detailFocusedSection == focusInfo {
		style = FocusBoxStyle
	}
	innerH := height - 2 // top + bottom border
	if innerH < 1 {
		innerH = 1
	}
	// lipgloss .Height() only pads up — it never truncates. Clamp ourselves so an info
	// panel taller than the budget can't push the rest of the page off the screen.
	body := clampLines(sb.String(), innerH)
	return style.Width(width - 2).Height(innerH).Render(body)
}

func (m Model) renderLogPanel(width, height int) string {
	var sb strings.Builder

	// width/height are OUTER (border-inclusive). Inner content width subtracts border
	// (2) + padding (2). Must match detailLayout() so the meta line, header, and
	// viewport all align with the box edges.
	innerW := width - 4
	if innerW < 10 {
		innerW = 10
	}
	innerH := height - 2 // top + bottom border
	if innerH < 3 {
		innerH = 3
	}

	// Shared header layout (matches renderInfoPanel): title left, meta right on
	// the same row, single blank row below, then content. Keeps the two panels'
	// content rows aligned across the divider.
	title := lipgloss.NewStyle().Bold(true).Foreground(ColorSecondary).
		Render(" [ Service Logs (journalctl) ] ")

	wrapState := "off"
	if m.logWrap {
		wrapState = "on"
	}
	followIndicator := ""
	if m.followLogs {
		followIndicator = " • " + ActiveBadge.Render("FOLLOW")
	}
	meta := LogMetaStyle.Render(fmt.Sprintf("%d/%d lines • wrap %s • %3d%% ",
		m.logRawLineCount, m.logLineLimit(), wrapState,
		int(m.logViewport.ScrollPercent()*100))) + followIndicator

	gap := innerW - lipgloss.Width(title) - lipgloss.Width(meta)
	if gap < 1 {
		gap = 1
	}
	sb.WriteString(title + strings.Repeat(" ", gap) + meta + "\n\n")

	// Viewport body
	sb.WriteString(m.logViewport.View())

	// Apply box styling based on focus
	style := BoxStyle
	if m.detailFocusedSection == focusLogs {
		style = FocusBoxStyle
	}

	body := clampLines(sb.String(), innerH)
	return style.Width(width - 2).Height(innerH).Render(body)
}

func (m Model) renderDetailFooter() string {
	keys := []string{
		"Tab", "Switch Panel",
		"Esc/←", "Back",
		"u", "Unit File",
		"s/t/r", "Start/Stop/Restart",
		"e/d", "Enable/Disable",
		"f", "Follow",
		"w", "Wrap",
		"[ / ]", "Lines",
		"R", "Refresh",
	}
	if m.detailFocusedSection == focusLogs {
		keys = append(keys, "↑/↓/PgUp/PgDn", "Scroll", "g/G", "Top/Bottom")
	}
	return RenderFooter(keys, m.width)
}

// wrapDetailValue word-wraps a styled value to width cells, falling back to a hard
// break for tokens longer than the column. ansi.Wrap preserves ANSI sequences across
// line breaks so colored badges (e.g. ActiveBadge) don't bleed past the value column.
func wrapDetailValue(s string, width int) string {
	if width <= 0 {
		return s
	}
	return ansi.Wrap(s, width, "")
}

// clampLines keeps at most n lines of s (no padding — lipgloss .Height() handles padding).
// Used to stop overflowing panel content from pushing borders off the screen.
func clampLines(s string, n int) string {
	if n <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n")
}

func formatMemory(bytes uint64) string {
	if systemd.IsUnset(bytes) {
		return "N/A"
	}
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := uint64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

func formatCPU(nsec uint64) string {
	if systemd.IsUnset(nsec) {
		return "N/A"
	}
	dur := time.Duration(nsec) * time.Nanosecond
	return fmt.Sprintf("%.3fs", dur.Seconds())
}

func formatTimestamp(usec uint64) string {
	if usec == 0 {
		return "N/A"
	}
	t := time.UnixMicro(int64(usec))
	return fmt.Sprintf("%s (%s)", t.Format("2006-01-02 15:04:05"), formatDuration(time.Since(t)))
}

// formatDuration renders d as a humanized "X ago" string with two units of precision.
func formatDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %ds ago", int(d.Minutes()), int(d.Seconds())%60)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm ago", int(d.Hours()), int(d.Minutes())%60)
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd %dh ago", int(d.Hours()/24), int(d.Hours())%24)
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
