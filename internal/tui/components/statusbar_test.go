package components

import (
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/Dicklesworthstone/ntm/internal/agent"
)

func TestRenderStatusBar_ZeroWidth(t *testing.T) {
	got := RenderStatusBar(StatusBarOptions{Width: 0})
	if got != "" {
		t.Errorf("expected empty string for zero width, got %q", got)
	}
}

func TestRenderStatusBar_EmptySession(t *testing.T) {
	got := RenderStatusBar(StatusBarOptions{
		Width:   80,
		Session: "",
	})
	if got == "" {
		t.Fatal("expected non-empty output for default session")
	}
	// Should contain the dash placeholder for empty session
	if !strings.Contains(got, "—") {
		t.Error("expected dash placeholder for empty session name")
	}
}

func TestRenderStatusBar_ZeroAgentCounts(t *testing.T) {
	counts := map[agent.AgentType]int{agent.AgentTypeUser: 0}
	for _, agentType := range agent.AgentTypes() {
		counts[agentType] = 0
	}
	got := RenderStatusBar(StatusBarOptions{
		Width:       200,
		Session:     "test-session",
		AgentCounts: counts,
	})
	if got == "" {
		t.Fatal("expected non-empty output")
	}
	// Agent badge labels should NOT appear when counts are zero
	for agentType := range counts {
		if label := AgentCountLabel(agentType) + ":"; strings.Contains(got, label) {
			t.Errorf("expected no %s badge when count is zero, got %q", label, got)
		}
	}
}

func TestRenderStatusBar_AgentBadgesPresent(t *testing.T) {
	got := RenderStatusBar(StatusBarOptions{
		Width:   120,
		Session: "dev",
		AgentCounts: map[agent.AgentType]int{
			agent.AgentTypeClaudeCode: 3,
			agent.AgentTypeCodex:      1,
			agent.AgentTypeGemini:     2,
			agent.AgentTypeGrok:       4,
			agent.AgentTypeUser:       1,
		},
	})
	for _, label := range []string{"CC:3", "COD:1", "GMI:2", "GRK:4", "USR:1"} {
		if !strings.Contains(got, label) {
			t.Errorf("expected badge %q in output", label)
		}
	}
}

// Pi, Kimi and Opencode panes used to have no status bar badge at all. Each
// type with panes gets its own badge, and types without panes add none.
func TestRenderStatusBar_NewerAgentTypeBadges(t *testing.T) {
	got := RenderStatusBar(StatusBarOptions{
		Width:   160,
		Session: "dev",
		AgentCounts: map[agent.AgentType]int{
			agent.AgentTypePi:       1,
			agent.AgentTypeKimi:     2,
			agent.AgentTypeOpencode: 3,
			agent.AgentTypeMuse:     4,
			agent.AgentTypeCodex:    0,
		},
	})
	for _, label := range []string{"PI:1", "KIMI:2", "OC:3", "MUSE:4"} {
		if !strings.Contains(got, label) {
			t.Errorf("expected badge %q in output, got %q", label, got)
		}
	}
	for _, label := range []string{"CC:", "COD:", "USR:"} {
		if strings.Contains(got, label) {
			t.Errorf("expected no %s badge for a type without panes, got %q", label, got)
		}
	}
}

// Every agent type ntm can launch gets a label of its own, so no two types
// share a badge the way Antigravity and Aider once shared "A:" in the ticker.
func TestAgentCountLabelsAreDistinct(t *testing.T) {
	seen := map[string]agent.AgentType{}
	for _, agentType := range append(agent.AgentTypes(), agent.AgentTypeUser) {
		label := AgentCountLabel(agentType)
		if label == "" {
			t.Errorf("AgentCountLabel(%q) is empty", agentType)
		}
		if other, ok := seen[label]; ok {
			t.Errorf("AgentCountLabel(%q) = %q, same as %q", agentType, label, other)
		}
		seen[label] = agentType
	}
}

func TestAgentCountsInOrder(t *testing.T) {
	got := AgentCountsInOrder(map[agent.AgentType]int{
		agent.AgentTypeUser:       2,
		agent.AgentTypeOpencode:   1,
		agent.AgentTypeClaudeCode: 3,
		agent.AgentTypePi:         1,
		agent.AgentTypeCodex:      0,
	})
	want := []AgentCount{
		{Type: agent.AgentTypeClaudeCode, Label: "CC", Count: 3},
		{Type: agent.AgentTypePi, Label: "PI", Count: 1},
		{Type: agent.AgentTypeOpencode, Label: "OC", Count: 1},
		{Type: agent.AgentTypeUser, Label: "USR", Count: 2},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AgentCountsInOrder() = %+v, want %+v", got, want)
	}
	if empty := AgentCountsInOrder(nil); len(empty) != 0 {
		t.Errorf("AgentCountsInOrder(nil) = %+v, want none", empty)
	}
}

func TestRenderStatusBar_AllLayoutTiers(t *testing.T) {
	tiers := []string{"Narrow", "Split", "Wide", "Ultra", "Mega"}
	for _, tier := range tiers {
		t.Run(tier, func(t *testing.T) {
			got := RenderStatusBar(StatusBarOptions{
				Width:      100,
				Session:    "s",
				LayoutTier: tier,
			})
			if !strings.Contains(got, tier) {
				t.Errorf("expected layout tier %q in output", tier)
			}
		})
	}
}

func TestRenderStatusBar_UnknownLayoutTier(t *testing.T) {
	got := RenderStatusBar(StatusBarOptions{
		Width:      100,
		Session:    "s",
		LayoutTier: "Custom",
	})
	if !strings.Contains(got, "Custom") {
		t.Error("expected custom tier label in output")
	}
}

func TestRenderStatusBar_EmptyLayoutTier(t *testing.T) {
	got := RenderStatusBar(StatusBarOptions{
		Width:   80,
		Session: "s",
	})
	// Should show dash placeholder for empty tier
	if got == "" {
		t.Fatal("expected non-empty output")
	}
}

func TestRenderStatusBar_FocusedPanel(t *testing.T) {
	got := RenderStatusBar(StatusBarOptions{
		Width:        120,
		Session:      "s",
		FocusedPanel: "Detail",
	})
	if !strings.Contains(got, "Detail") {
		t.Error("expected focused panel name in output")
	}
}

func TestRenderStatusBar_VelocitySparkline(t *testing.T) {
	got := RenderStatusBar(StatusBarOptions{
		Width:           120,
		Session:         "s",
		FocusedPanel:    "Detail",
		CurrentVelocity: 240,
		VelocityHistory: []float64{80, 120, 160, 200, 240},
	})
	if !strings.Contains(got, "Detail") {
		t.Fatal("expected focused panel name in output")
	}
	if !strings.Contains(got, "tpm") {
		t.Fatal("expected tpm sparkline label in output")
	}
	if !strings.Contains(got, "240") {
		t.Fatal("expected current velocity in output")
	}
}

func TestRenderStatusBar_PausedIndicator(t *testing.T) {
	got := RenderStatusBar(StatusBarOptions{
		Width:   80,
		Session: "s",
		Paused:  true,
	})
	if !strings.Contains(got, "⏸") {
		t.Error("expected paused indicator in output")
	}
}

func TestRenderStatusBar_NarrowWidth(t *testing.T) {
	// Even at very narrow widths the function should not panic
	got := RenderStatusBar(StatusBarOptions{
		Width:   10,
		Session: "long-session-name",
		AgentCounts: map[agent.AgentType]int{
			agent.AgentTypeClaudeCode: 5,
			agent.AgentTypeCodex:      3,
			agent.AgentTypeGemini:     2,
		},
		FocusedPanel: "Detail",
		LayoutTier:   "Ultra",
	})
	if got == "" {
		t.Fatal("expected non-empty output even at narrow width")
	}
	// The visible width of the bar line should not exceed opts.Width
	// (border line is the first line)
	lines := strings.Split(got, "\n")
	if len(lines) < 2 {
		t.Fatal("expected at least two lines (border + bar)")
	}
	borderWidth := lipgloss.Width(lines[0])
	if borderWidth > 10 {
		t.Errorf("border width %d exceeds requested width 10", borderWidth)
	}
	barWidth := lipgloss.Width(lines[1])
	if barWidth > 10 {
		t.Errorf("bar width %d exceeds requested width 10", barWidth)
	}
}

func TestRenderStatusBar_NegativeWidth(t *testing.T) {
	got := RenderStatusBar(StatusBarOptions{Width: -1})
	if got != "" {
		t.Errorf("expected empty string for negative width, got %q", got)
	}
}

func TestSparklineWithLabel_RespectsRequestedWidth(t *testing.T) {
	got := SparklineWithLabel("tpm", []float64{10, 20, 30, 40}, 6, "240")
	if width := lipgloss.Width(got); width > 6 {
		t.Fatalf("SparklineWithLabel width = %d, want <= 6 (output=%q)", width, got)
	}
}
