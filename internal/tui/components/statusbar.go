// Package components provides shared TUI building blocks.
package components

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/Dicklesworthstone/ntm/internal/agent"
	"github.com/Dicklesworthstone/ntm/internal/tui/styles"
	"github.com/Dicklesworthstone/ntm/internal/tui/theme"
)

// StatusBarOptions configures status bar rendering.
type StatusBarOptions struct {
	Width   int
	Session string
	// AgentCounts holds pane counts by canonical agent type, with user shell
	// panes under agent.AgentTypeUser.
	AgentCounts     map[agent.AgentType]int
	FocusedPanel    string
	LayoutTier      string // "Narrow", "Split", "Wide", "Ultra", "Mega"
	Paused          bool
	CurrentVelocity float64
	VelocityHistory []float64
}

// AgentCount is one agent type's pane count and the short label the
// dashboard's count badges show for it.
type AgentCount struct {
	Type  agent.AgentType
	Label string
	Count int
}

// AgentCountsInOrder returns the non-zero counts in counts, agent types in
// agent.AgentTypes() order and then user panes, so every type ntm can launch
// gets its own badge and types without panes add nothing.
func AgentCountsInOrder(counts map[agent.AgentType]int) []AgentCount {
	var out []AgentCount
	for _, agentType := range append(agent.AgentTypes(), agent.AgentTypeUser) {
		if n := counts[agentType]; n > 0 {
			out = append(out, AgentCount{Type: agentType, Label: AgentCountLabel(agentType), Count: n})
		}
	}
	return out
}

// AgentCountLabel returns the short badge label for a canonical agent type:
// its short form in upper case (CC, COD, PI, KIMI, OC), or the three-letter
// code `ntm list` uses when the short form is a whole word.
func AgentCountLabel(agentType agent.AgentType) string {
	switch agentType {
	case agent.AgentTypeGrok:
		return "GRK"
	case agent.AgentTypeCursor:
		return "CUR"
	case agent.AgentTypeWindsurf:
		return "WND"
	case agent.AgentTypeAider:
		return "AID"
	case agent.AgentTypeOllama:
		return "OLL"
	case agent.AgentTypeUser:
		return "USR"
	default:
		return strings.ToUpper(string(agentType))
	}
}

// PaneAgentType returns agentType's canonical form, with types ntm doesn't
// know as user panes, which is how the dashboard counts and draws them.
func PaneAgentType(agentType agent.AgentType) agent.AgentType {
	canonical := agentType.Canonical()
	if !canonical.IsValid() {
		return agent.AgentTypeUser
	}
	return canonical
}

// AgentTypeColor returns the color that marks an agent type wherever the TUI
// draws one: the semantic palette's agent color, which the dashboard's count
// badges, pane cards, rows and borders, the context panel and the command
// palette's agent badge all share.
func AgentTypeColor(agentType agent.AgentType, t theme.Theme) lipgloss.Color {
	return t.Semantic().AgentColor(string(PaneAgentType(agentType)))
}

// RenderStatusBar renders a three-section status bar: left | center | right.
// Uses the full terminal width with a Surface0 background and Surface1 top border.
func RenderStatusBar(opts StatusBarOptions) string {
	t := theme.Current()
	if opts.Width <= 0 {
		return ""
	}

	// Top border — subtle line using Surface1
	border := lipgloss.NewStyle().
		Foreground(t.Surface1).
		Width(opts.Width).
		Render(strings.Repeat("─", opts.Width))

	// Base style for all sections
	base := lipgloss.NewStyle().
		Background(t.Surface0)

	// ── Left section: session name + agent count badges ──────────
	left := renderStatusLeft(t, base, opts)

	// ── Right section: layout tier + tick indicator ──────────────
	right := renderStatusRight(t, base, opts)

	// Calculate widths for three-column layout
	leftW := lipgloss.Width(left)
	rightW := lipgloss.Width(right)
	centerW := opts.Width - leftW - rightW
	if centerW < 0 {
		centerW = 0
	}

	// ── Center section: focused panel name + aggregate trend ─────
	center := renderStatusCenter(t, base, opts, centerW)

	// Pad center to fill remaining space, keeping it visually centered
	centeredSection := base.Width(centerW).Align(lipgloss.Center).Render(
		lipgloss.PlaceHorizontal(centerW, lipgloss.Center, center,
			lipgloss.WithWhitespaceBackground(t.Surface0)),
	)

	bar := lipgloss.JoinHorizontal(lipgloss.Top, left, centeredSection, right)
	if lipgloss.Width(bar) > opts.Width {
		bar = styles.Truncate(bar, opts.Width)
	}

	return border + "\n" + bar
}

// renderStatusLeft builds the left section: session name + agent badges.
func renderStatusLeft(t theme.Theme, base lipgloss.Style, opts StatusBarOptions) string {
	var parts []string

	// Session name
	session := opts.Session
	if session == "" {
		session = "—"
	}
	sessionStyle := base.
		Foreground(t.Text).
		Bold(true).
		Padding(0, 1)
	parts = append(parts, sessionStyle.Render(session))

	// Agent count badges, only for types with panes
	palette := t.Semantic()
	for _, c := range AgentCountsInOrder(opts.AgentCounts) {
		badge := lipgloss.NewStyle().
			Background(palette.AgentColor(string(c.Type))).
			Foreground(t.Base).
			Bold(true).
			Padding(0, 1).
			Render(fmt.Sprintf("%s:%d", c.Label, c.Count))
		parts = append(parts, badge)
	}

	return lipgloss.JoinHorizontal(lipgloss.Top, parts...)
}

// renderStatusCenter builds the center section: focused panel name + velocity trend.
func renderStatusCenter(t theme.Theme, base lipgloss.Style, opts StatusBarOptions, width int) string {
	if width <= 0 {
		return ""
	}

	focusedPanel := ""
	if opts.FocusedPanel != "" {
		focusedPanel = base.
			Foreground(t.Primary).
			Bold(true).
			Render(opts.FocusedPanel)
	}

	if len(opts.VelocityHistory) == 0 {
		if focusedPanel == "" {
			return ""
		}
		return styles.Truncate(focusedPanel, width)
	}

	currentVelocity := opts.CurrentVelocity
	if currentVelocity <= 0 {
		currentVelocity = opts.VelocityHistory[len(opts.VelocityHistory)-1]
	}

	if focusedPanel == "" {
		return SparklineWithLabel("tpm", opts.VelocityHistory, width, fmt.Sprintf("%.0f", currentVelocity))
	}

	if width < 24 {
		return styles.Truncate(focusedPanel, width)
	}

	separator := base.Foreground(t.Overlay).Render(" · ")
	remainingWidth := width - lipgloss.Width(focusedPanel) - lipgloss.Width(separator)
	if remainingWidth < 12 {
		return styles.Truncate(focusedPanel, width)
	}

	trend := SparklineWithLabel("tpm", opts.VelocityHistory, remainingWidth, fmt.Sprintf("%.0f", currentVelocity))
	return lipgloss.JoinHorizontal(lipgloss.Top, focusedPanel, separator, trend)
}

// renderStatusRight builds the right section: layout tier + tick/paused indicator.
func renderStatusRight(t theme.Theme, base lipgloss.Style, opts StatusBarOptions) string {
	var parts []string

	// Layout tier badge
	tier := opts.LayoutTier
	if tier == "" {
		tier = "—"
	}
	tierColor := layoutTierColor(t, tier)
	tierBadge := lipgloss.NewStyle().
		Background(tierColor).
		Foreground(t.Base).
		Bold(true).
		Padding(0, 1).
		Render(tier)
	parts = append(parts, tierBadge)

	// Tick / paused indicator
	var indicator string
	if opts.Paused {
		indicator = lipgloss.NewStyle().
			Background(t.Warning).
			Foreground(t.Base).
			Bold(true).
			Padding(0, 1).
			Render("⏸")
	} else {
		indicator = base.
			Foreground(t.Success).
			Padding(0, 1).
			Render("●")
	}
	parts = append(parts, indicator)

	return lipgloss.JoinHorizontal(lipgloss.Top, parts...)
}

// layoutTierColor returns a color based on the layout tier.
func layoutTierColor(t theme.Theme, tier string) lipgloss.Color {
	switch tier {
	case "Narrow":
		return t.Overlay
	case "Split":
		return t.Sapphire
	case "Wide":
		return t.Blue
	case "Ultra":
		return t.Mauve
	case "Mega":
		return t.Flamingo
	default:
		return t.Surface2
	}
}
