package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// handleUnitFileKey routes keys while the unit-file full-screen view is active.
// Scroll keys go to the viewport so PgUp/PgDn/arrow/home/end all just work.
func (m *Model) handleUnitFileKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc", "left", "h", "u", "backspace":
		m.currentView = detailView
		return nil

	case "g":
		m.unitFileViewport.GotoTop()
		return nil

	case "G":
		m.unitFileViewport.GotoBottom()
		return nil

	case "R":
		// Re-fetch service details (which re-reads the unit file).
		if m.activeDetail != nil {
			return m.fetchDetailsCmd(m.activeDetail.Name)
		}
		return nil
	}

	var cmd tea.Cmd
	m.unitFileViewport, cmd = m.unitFileViewport.Update(msg)
	return cmd
}

// renderUnitFileView is the full-screen unit-file viewer. The body is rendered as
// plain text with NO border, NO padding, and no per-line styling so a mouse drag
// in the terminal selects exactly the file contents and nothing else.
func (m Model) renderUnitFileView() string {
	var s strings.Builder

	name := ""
	path := ""
	if m.activeDetail != nil {
		name = m.activeDetail.Name
		path = m.activeDetail.FragmentPath
	}

	// 1. Header
	title := TitleStyle.Render("UNIT FILE")
	subtitle := SubTitleStyle.Render(name)
	s.WriteString(lipgloss.JoinHorizontal(lipgloss.Center, title, subtitle) + "\n")

	// 2. Path row (dim — informational, not part of the file content)
	if path == "" {
		s.WriteString(LogMetaStyle.Render("(no on-disk unit file — transient or generated unit)") + "\n\n")
	} else {
		s.WriteString(LogMetaStyle.Render(path) + "\n\n")
	}

	// 3. Body
	s.WriteString(m.unitFileViewport.View())

	// 4. Footer
	s.WriteString("\n" + m.renderUnitFileFooter())

	return DocStyle.Render(s.String())
}

func (m Model) renderUnitFileFooter() string {
	return RenderFooter([]string{
		"Esc/u/←", "Back",
		"↑/↓/PgUp/PgDn", "Scroll",
		"g/G", "Top/Bottom",
		"R", "Reload",
		"q", "Quit",
	}, m.width)
}
