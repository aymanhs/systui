package tui

import (
	"fmt"
	"strings"

	"github.com/aymanhs/jeeves/systemd"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

func (m *Model) handleListKey(msg tea.KeyMsg) tea.Cmd {
	if len(m.filteredServices) == 0 && msg.String() != "R" && msg.String() != "/" && msg.String() != "a" {
		return nil
	}

	var selected *systemd.ServiceInfo
	if len(m.filteredServices) > 0 {
		selected = &m.filteredServices[m.selectedIndex]
	}

	switch msg.String() {
	case "up", "k":
		if m.selectedIndex > 0 {
			m.selectedIndex--
		} else {
			m.selectedIndex = len(m.filteredServices) - 1 // wrap around
		}

	case "down", "j":
		if m.selectedIndex < len(m.filteredServices)-1 {
			m.selectedIndex++
		} else {
			m.selectedIndex = 0 // wrap around
		}

	case "enter", "right", "l":
		if selected != nil {
			m.currentView = detailView
			m.detailFocusedSection = focusInfo
			m.activeDetail = selected // temporary details while fetching
			m.logs = "Loading logs..."
			m.unitFile = "Loading unit file..."
			m.logRawLineCount = 0
			m.recalculateViewportSize() // also pushes content through refreshLogViewport
			return m.fetchDetailsCmd(selected.Name)
		}

	case "s": // Start
		if selected != nil {
			return m.requestAction("start", selected.Name)
		}

	case "t": // Stop
		if selected != nil {
			return m.requestAction("stop", selected.Name)
		}

	case "r": // Restart
		if selected != nil {
			return m.requestAction("restart", selected.Name)
		}

	case "e": // Enable
		if selected != nil {
			return m.requestAction("enable", selected.Name)
		}

	case "d": // Disable
		if selected != nil {
			return m.requestAction("disable", selected.Name)
		}

	case "/": // Filter
		m.filtering = true
		m.searchInput.Focus()
		m.searchInput.SetValue(m.filterQuery)
		return textinput.Blink

	case "a": // Toggle view mode: all → running → failed → all
		m.showMode = (m.showMode + 1) % showModeCount
		m.filterServices()

	case "S": // Cycle sort mode
		m.sortMode = (m.sortMode + 1) % sortModeCount
		m.filterServices()
		return m.triggerStatus(fmt.Sprintf("Sorting by %s", m.sortMode.String()), false)

	case "R": // Refresh
		return m.fetchServicesCmd()
	}

	return nil
}

func (m Model) renderListView() string {
	var s strings.Builder

	// 1. Header
	title := TitleStyle.Render("SYSTEMD SERVICES")
	modeStr := fmt.Sprintf("Bus: %s Mode", m.client.Mode().String())
	if m.loading {
		modeStr += " [LOADING...]"
	}
	subtitle := SubTitleStyle.Render(modeStr)
	s.WriteString(lipgloss.JoinHorizontal(lipgloss.Center, title, subtitle) + "\n\n")

	// 2. Search / Filter bar
	if m.filtering {
		// While typing: framed input box + an explicit hint so the user can see
		// that keypresses are being captured by the filter, not the list.
		label := SearchActiveLabel.Render(" FILTER ")
		input := SearchActiveBox.Render(m.searchInput.View())
		hint := HelpDescStyle.Render("  Enter / Esc to apply")
		s.WriteString(label + " " + input + hint + "\n\n")
	} else if m.filterQuery != "" {
		filterText := SearchPromptStyle.Render("Filter: ") + SearchInputStyle.Render(m.filterQuery)
		infoText := HelpDescStyle.Render(" (press / to edit, esc to clear)")
		s.WriteString(filterText + infoText + "\n\n")
	} else {
		s.WriteString(HelpDescStyle.Render("Press / to search/filter services, [a] to toggle view...") + "\n\n")
	}

	// 3. Mode + sort indicator
	var modeLabel string
	switch m.showMode {
	case showRunning:
		modeLabel = ActiveBadge.Render("View: Running")
	case showFailed:
		modeLabel = FailedBadge.Render("View: Failed")
	default:
		modeLabel = HelpDescStyle.Render("View: All")
	}
	sortLabel := HelpDescStyle.Render(fmt.Sprintf("  •  Sort: %s  •  %d shown",
		m.sortMode.String(), len(m.filteredServices)))
	s.WriteString(modeLabel + sortLabel + "\n\n")

	// 4. Services Table
	if len(m.filteredServices) == 0 {
		switch {
		case m.initialLoad:
			s.WriteString("\n  " + HelpDescStyle.Render("Loading services...") + "\n")
		case m.loading:
			s.WriteString("\n  " + HelpDescStyle.Render("Refreshing...") + "\n")
		default:
			s.WriteString("\n  No services found matching filters.\n")
		}
	} else {
		// Table Columns configuration
		colStatusW := 12
		colNameW := 35
		colSubW := 12
		colEnableW := 12
		colDescW := m.width - colStatusW - colNameW - colSubW - colEnableW - 6
		if colDescW < 15 {
			colDescW = 15 // min fallback
		}

		// Table Header
		headerRow := fmt.Sprintf("  %s %s %s %s %s",
			padRight("STATUS", colStatusW),
			padRight("SERVICE NAME", colNameW),
			padRight("SUB STATE", colSubW),
			padRight("ENABLE STATE", colEnableW),
			padRight("DESCRIPTION", colDescW),
		)
		s.WriteString(TableHeaderStyle.Render(headerRow) + "\n")
		s.WriteString(lipgloss.NewStyle().Foreground(ColorDim).Render(strings.Repeat("-", m.width)) + "\n")

		// Calculate scrolling viewport for list. Must match listMaxRows() in
		// model.go so the scroll-offset clamp and the renderer agree.
		maxRows := m.listMaxRows()

		start := m.scrollOffset
		end := start + maxRows
		if end > len(m.filteredServices) {
			end = len(m.filteredServices)
		}

		// Render rows
		renderedRows := 0
		for i := start; i < end; i++ {
			svc := m.filteredServices[i]
			renderedRows++

			// Format columns
			statusIndicator := m.formatActiveState(svc.ActiveState)
			nameStr := svc.Name
			subStateStr := svc.SubState
			enableStateStr := m.formatEnableState(svc.UnitFileState)
			descStr := svc.Description

			rowText := fmt.Sprintf("%s %s %s %s %s",
				padRight(statusIndicator, colStatusW),
				padRight(nameStr, colNameW),
				padRight(subStateStr, colSubW),
				padRight(enableStateStr, colEnableW),
				padRight(descStr, colDescW),
			)

			if i == m.selectedIndex {
				// Selected Row
				s.WriteString(SelectedRowStyle.Render("➜ "+rowText) + "\n")
			} else {
				// Regular Row
				s.WriteString(RowStyle.Render("  "+rowText) + "\n")
			}
		}

		// Pad with empty rows to keep status and help pinned to bottom
		for i := renderedRows; i < maxRows; i++ {
			s.WriteString("\n")
		}
	}

	// 5. Status banner (errors or success) - always exactly 2 lines
	if m.statusMsg != "" {
		if m.statusIsErr {
			s.WriteString("\n" + ErrorBanner.Render("⚠ "+m.statusMsg))
		} else {
			s.WriteString("\n" + SuccessBanner.Render("✔ "+m.statusMsg))
		}
	} else {
		s.WriteString("\n\n")
	}

	// 6. Help Footer
	s.WriteString(m.renderListFooter())

	return DocStyle.Render(s.String())
}

func (m Model) formatActiveState(state string) string {
	// Single colored word — no leading glyph. The color of the word IS the indicator;
	// the bullet looked like a blob next to every row and added visual noise.
	switch state {
	case "active":
		return ActiveBadge.Render("active")
	case "failed":
		return FailedBadge.Render("failed")
	case "activating", "deactivating", "reloading":
		return WarningBadge.Render(state)
	case "inactive":
		return InactiveBadge.Render("inactive")
	default:
		return InactiveBadge.Render(state)
	}
}

func (m Model) formatEnableState(state string) string {
	switch state {
	case "enabled":
		return EnabledBadge.Render("enabled")
	case "disabled":
		return DisabledBadge.Render("disabled")
	case "static":
		return StaticBadge.Render("static")
	case "masked":
		return WarningBadge.Render("masked")
	case "alias":
		return StaticBadge.Render("alias")
	case "generated":
		return StaticBadge.Render("generated")
	case "":
		return InactiveBadge.Render("-")
	default:
		return InactiveBadge.Render(state)
	}
}

func (m Model) renderListFooter() string {
	return "\n" + RenderFooter([]string{
		"↑/↓/k/j", "Navigate",
		"Enter/→", "Details",
		"s/t/r", "Start/Stop/Restart",
		"e/d", "Enable/Disable",
		"/", "Filter",
		"a", "View",
		"S", "Sort",
		"R", "Refresh",
		"q", "Quit",
	}, m.width)
}

// padRight returns s padded with spaces (or truncated with "…") to fit width
// terminal cells. Unicode- and ANSI-safe: never slices in the middle of a
// multi-byte rune or an escape sequence.
func padRight(s string, width int) string {
	visualWidth := lipgloss.Width(s)
	if visualWidth > width {
		if width <= 1 {
			return ansi.Truncate(s, width, "")
		}
		return ansi.Truncate(s, width, "…")
	}
	return s + strings.Repeat(" ", width-visualWidth)
}
