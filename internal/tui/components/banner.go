package components

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/Dicklesworthstone/ntm/internal/agent"
	"github.com/Dicklesworthstone/ntm/internal/tui/icons"
	"github.com/Dicklesworthstone/ntm/internal/tui/styles"
	"github.com/Dicklesworthstone/ntm/internal/tui/theme"
)

// ASCII art logos for NTM
var (
	// Large banner logo
	LogoLarge = []string{
		"███╗   ██╗████████╗███╗   ███╗",
		"████╗  ██║╚══██╔══╝████╗ ████║",
		"██╔██╗ ██║   ██║   ██╔████╔██║",
		"██║╚██╗██║   ██║   ██║╚██╔╝██║",
		"██║ ╚████║   ██║   ██║ ╚═╝ ██║",
		"╚═╝  ╚═══╝   ╚═╝   ╚═╝     ╚═╝",
	}

	// Medium banner logo
	LogoMedium = []string{
		"╔╗╔╔╦╗╔╦╗",
		"║║║ ║ ║║║",
		"╝╚╝ ╩ ╩ ╩",
	}

	// Small inline logo
	LogoSmall = "⟦NTM⟧"

	// Icon variants
	LogoIcon      = "󰆍" // Terminal icon (Nerd Font)
	LogoIconPlain = "▣" // Plain Unicode fallback
)

func gradientPrimary() []string {
	t := theme.Current()
	return []string{string(t.Blue), string(t.Lavender), string(t.Mauve)}
}

// RenderBanner renders the large logo with gradient
func RenderBanner(animated bool, tick int) string {
	var lines []string

	for _, line := range LogoLarge {
		if animated {
			lines = append(lines, styles.Shimmer(line, tick, gradientPrimary()...))
		} else {
			lines = append(lines, styles.GradientText(line, gradientPrimary()...))
		}
	}

	return strings.Join(lines, "\n")
}

// RenderBannerMedium renders the medium logo with gradient
func RenderBannerMedium(animated bool, tick int) string {
	var lines []string

	for _, line := range LogoMedium {
		if animated {
			lines = append(lines, styles.Shimmer(line, tick, gradientPrimary()...))
		} else {
			lines = append(lines, styles.GradientText(line, gradientPrimary()...))
		}
	}

	return strings.Join(lines, "\n")
}

// RenderSubtitle renders a styled subtitle
func RenderSubtitle(text string) string {
	return lipgloss.NewStyle().
		Foreground(theme.Current().Subtext).
		Italic(true).
		Render(text)
}

// RenderAgentBadge renders a colored badge for an agent type
func RenderAgentBadge(agentType string) string {
	t := theme.Current()
	ic := icons.Current()

	bgColor, icon := renderAgentBadgeStyle(agentType, t, ic)
	label := strings.ToUpper(renderAgentBadgeLabel(agentType))

	return lipgloss.NewStyle().
		Background(bgColor).
		Foreground(t.Crust).
		Bold(true).
		Padding(0, 1).
		Render(strings.TrimSpace(icon + " " + label))
}

// renderAgentBadgeStyle returns the agent badge's background, the type's
// AgentTypeColor, and its icon: the generic robot glyph for types the icon
// set has none for (Grok, Muse, omp, ...), which the badge's label then names.
// Types ntm doesn't know take the user pane's color and icon, as on the
// dashboard.
func renderAgentBadgeStyle(agentType string, t theme.Theme, ic icons.IconSet) (lipgloss.Color, string) {
	paneType := PaneAgentType(agent.AgentType(agentType))
	if paneType == agent.AgentTypeUser {
		return AgentTypeColor(paneType, t), ic.User
	}
	return AgentTypeColor(paneType, t), ic.AgentIcon(string(paneType))
}

func renderAgentBadgeLabel(agentType string) string {
	canonical := agent.AgentType(agentType).Canonical()
	if canonical.IsValid() || canonical == agent.AgentTypeUnknown {
		if label := strings.TrimSpace(canonical.ProfileName()); label != "" {
			return label
		}
	}

	label := strings.TrimSpace(agentType)
	if label == "" {
		return "unknown"
	}
	return label
}
