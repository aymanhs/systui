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

	case "pgup", "ctrl+u":
		// One viewport page up. Clamp at 0; no wrap (page-style navigation is
		// for getting somewhere fast, wrapping would be disorienting).
		page := m.listMaxRows()
		if page < 1 {
			page = 1
		}
		m.selectedIndex -= page
		if m.selectedIndex < 0 {
			m.selectedIndex = 0
		}

	case "pgdown", "ctrl+d":
		page := m.listMaxRows()
		if page < 1 {
			page = 1
		}
		m.selectedIndex += page
		if m.selectedIndex > len(m.filteredServices)-1 {
			m.selectedIndex = len(m.filteredServices) - 1
		}

	case "home", "g":
		m.selectedIndex = 0

	case "end", "G":
		m.selectedIndex = len(m.filteredServices) - 1

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
	contentW := m.width - 2
	if contentW < 40 {
		contentW = 40
	}

	// 1. Header
	title := TitleStyle.Render(strings.ToUpper(m.appName) + " v" + m.appVersion)
	var modeStr string
	switch {
	case m.client != nil:
		modeStr = fmt.Sprintf("Bus: %s Mode", m.client.Mode().String())
	case m.showingCached:
		// Cache landed before the live connection — show the cached bus so
		// the header isn't lying about which units we're displaying.
		modeStr = fmt.Sprintf("Bus: %s Mode (cached)", m.cachedMode.String())
	default:
		modeStr = "Bus: Connecting..."
	}
	if m.showingCached && m.client != nil {
		modeStr += " (cached)"
	}
	if m.loading {
		modeStr += " [LOADING...]"
	}
	subtitle := SubTitleStyle.Render(modeStr)
	header := lipgloss.JoinHorizontal(lipgloss.Top, title, "  ", subtitle)
	s.WriteString(ansi.Truncate(header, contentW, "") + "\n")

	// 2. Search / Filter bar
	if m.filtering {
		// Render active search as plain text so typed input is always visible.
		label := SearchActiveLabel.Render(" FILTER ")
		query := m.searchInput.Value()
		if query == "" {
			query = m.searchInput.Placeholder
		}
		input := SearchInputStyle.Render("/ " + query + "_")
		hint := HelpDescStyle.Render("  Enter / Esc to apply")
		row := lipgloss.JoinHorizontal(lipgloss.Top, label, " ", input, hint)
		s.WriteString(ansi.Truncate(row, contentW, "") + "\n")
	} else if m.filterQuery != "" {
		filterText := SearchPromptStyle.Render("Filter: ") + SearchInputStyle.Render(m.filterQuery)
		infoText := HelpDescStyle.Render(" (press / to edit, esc to clear)")
		s.WriteString(ansi.Truncate(filterText+infoText, contentW, "") + "\n")
	} else {
		s.WriteString(ansi.Truncate(HelpDescStyle.Render("Press / to search/filter services, [a] to toggle view..."), contentW, "") + "\n")
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
	s.WriteString(modeLabel + sortLabel + "\n")

	// 4. Services Table — always render the header + separator so the empty
	// state doesn't reflow the rest of the screen. The empty message goes in
	// the rows area where service rows would otherwise be.
	colStatusW := 12
	colNameW := 35
	colSubW := 12
	colEnableW := 12
	colDescW := contentW - colStatusW - colNameW - colSubW - colEnableW - 7
	if colDescW < 15 {
		colDescW = 15 // min fallback
	}

	// Sorted-column indicator: append " ▼"/" ▲" to the header of whichever
	// column drives the current sort. Memory/PID don't have their own
	// columns, so they fall back to the existing "Sort: memory" subtitle
	// for feedback.
	statusHdr := "STATUS"
	nameHdr := "SERVICE NAME"
	switch m.sortMode {
	case sortByState:
		statusHdr += " ▼"
	case sortByName:
		nameHdr += " ▲" // names sort ascending (A→Z); state surfaces failures first
	}

	// Render each cell with its own style — wrapping a pre-styled row in
	// TableHeaderStyle would inherit-then-reset colors at the inner ESC[0m
	// boundaries (lipgloss styles don't compose cleanly when nested), which
	// is what made the sorted-column color/arrow look broken before. Spaces
	// between cells are intentionally plain so the resets land in the
	// margins, never mid-label. Pad first, then style, so the padded width
	// is what the eye measures and the trailing spaces also pick up the
	// header color/underline.
	headerCell := func(label string, width int, active bool) string {
		style := TableHeaderStyle
		if active {
			style = style.Underline(true)
		}
		return style.Render(padRight(label, width))
	}

	statusCell := headerCell(statusHdr, colStatusW, m.sortMode == sortByState)
	nameCell := headerCell(nameHdr, colNameW, m.sortMode == sortByName)
	subCell := headerCell("SUB STATE", colSubW, false)
	enableCell := headerCell("ENABLE STATE", colEnableW, false)
	descCell := headerCell("DESCRIPTION", colDescW, false)

	headerRow := "   " + statusCell + " " + nameCell + " " + subCell + " " + enableCell + " " + descCell
	s.WriteString(ansi.Truncate(headerRow, contentW, "") + "\n")
	s.WriteString(lipgloss.NewStyle().Foreground(ColorDim).Render(strings.Repeat("-", contentW)) + "\n")

	// Calculate scrolling viewport for list. Must match listMaxRows() in
	// model.go so the scroll-offset clamp and the renderer agree.
	maxRows := m.listMaxRows()
	renderedRows := 0

	if len(m.filteredServices) == 0 {
		var msg string
		switch {
		case m.initialLoad:
			msg = "Loading services..."
		case m.loading:
			msg = "Refreshing..."
		default:
			// Mode-aware empty message — "no failed services" reads very
			// differently from "no matches for your filter".
			switch {
			case m.filterQuery != "":
				msg = fmt.Sprintf("No services match %q.", m.filterQuery)
			case m.showMode == showFailed:
				msg = "No failed services 🎉"
			case m.showMode == showRunning:
				msg = "No running services."
			default:
				msg = "No services found."
			}
		}
		s.WriteString("  " + HelpDescStyle.Render(msg) + "\n")
		renderedRows = 1
	} else {
		start := m.scrollOffset
		end := start + maxRows
		if end > len(m.filteredServices) {
			end = len(m.filteredServices)
		}

		for i := start; i < end; i++ {
			svc := m.filteredServices[i]
			renderedRows++

			// Format columns
			nameStr := svc.Name
			subStateStr := svc.SubState
			descStr := svc.Description

			statusIndicator := m.formatActiveState(svc.ActiveState)
			enableStateStr := m.formatEnableState(svc.UnitFileState)
			if i == m.selectedIndex {
				statusIndicator = m.formatActiveStatePlain(svc.ActiveState)
				enableStateStr = m.formatEnableStatePlain(svc.UnitFileState)
			}

			rowText := fmt.Sprintf("%s %s %s %s %s",
				padRight(statusIndicator, colStatusW),
				padRight(nameStr, colNameW),
				padRight(subStateStr, colSubW),
				padRight(enableStateStr, colEnableW),
				padRight(descStr, colDescW),
			)
			innerRowW := contentW - 3
			if innerRowW < 1 {
				innerRowW = 1
			}
			lineBody := padRight(rowText, innerRowW)

			if i == m.selectedIndex {
				selectedLine := "➜ " + lineBody
				s.WriteString(SelectedRowStyle.Render(selectedLine) + "\n")
			} else {
				// Regular Row
				s.WriteString(RowStyle.Render("  "+lineBody) + "\n")
			}
		}
	}

	// Pad with empty rows to keep status and help pinned to bottom
	for i := renderedRows; i < maxRows; i++ {
		s.WriteString("\n")
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

func (m Model) formatActiveStatePlain(state string) string {
	switch state {
	case "":
		return "-"
	default:
		return state
	}
}

func (m Model) formatEnableStatePlain(state string) string {
	if state == "" {
		return "-"
	}
	return state
}

func (m Model) renderListFooter() string {
	return "\n" + RenderFooter([]string{
		"↑/↓/k/j", "Navigate",
		"PgUp/PgDn", "Page",
		"g/G", "Top/Bottom",
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
