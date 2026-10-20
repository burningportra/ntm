package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/config"
	"github.com/Dicklesworthstone/ntm/internal/models"
	"github.com/Dicklesworthstone/ntm/internal/output"
	"github.com/Dicklesworthstone/ntm/internal/tmux"
	"github.com/Dicklesworthstone/ntm/internal/tui/icons"
	"github.com/Dicklesworthstone/ntm/internal/tui/theme"
)

func TestFormatDuration(t *testing.T) {

	tests := []struct {
		name string
		in   time.Duration
		want string
	}{
		{name: "seconds", in: 45 * time.Second, want: "45s"},
		{name: "minutes", in: 5 * time.Minute, want: "5m"},
		{name: "hours+minutes", in: 2*time.Hour + 15*time.Minute, want: "2h15m"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := formatDuration(tc.in); got != tc.want {
				t.Errorf("formatDuration(%s) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestPaneLabel(t *testing.T) {

	session := "myproject"
	pane := tmux.Pane{Index: 2, Title: "myproject__cc_2"}
	if got := paneLabel(session, pane); got != "cc_2" {
		t.Errorf("paneLabel trimmed prefix = %q, want %q", got, "cc_2")
	}

	pane.Title = "custom-title"
	if got := paneLabel(session, pane); got != "custom-title" {
		t.Errorf("paneLabel custom title = %q, want %q", got, "custom-title")
	}

	pane.Title = "   "
	if got := paneLabel(session, pane); got != "pane 2" {
		t.Errorf("paneLabel empty title = %q, want %q", got, "pane 2")
	}
}

func TestRenderProgressBar(t *testing.T) {

	if got := renderProgressBar(50, 0); got != "" {
		t.Errorf("renderProgressBar width 0 = %q, want empty", got)
	}

	bar := renderProgressBar(-10, 4)
	if bar != "[----]" {
		t.Errorf("renderProgressBar negative percent = %q, want %q", bar, "[----]")
	}

	bar = renderProgressBar(150, 3)
	if bar != "[===]" {
		t.Errorf("renderProgressBar >100 percent = %q, want %q", bar, "[===]")
	}

	bar = renderProgressBar(50, 4)
	if bar != "[==--]" {
		t.Errorf("renderProgressBar 50%% = %q, want %q", bar, "[==--]")
	}
}

func TestModelNameForPane(t *testing.T) {
	oldCfg := cfg
	t.Cleanup(func() { cfg = oldCfg })

	cfg = nil

	defaults := config.DefaultModels()
	if got := modelNameForPane(tmux.Pane{Type: tmux.AgentClaude}); got != defaults.DefaultClaude {
		t.Errorf("default claude model = %q, want %q", got, defaults.DefaultClaude)
	}
	if got := modelNameForPane(tmux.Pane{Type: tmux.AgentCodex}); got != defaults.DefaultCodex {
		t.Errorf("default codex model = %q, want %q", got, defaults.DefaultCodex)
	}
	if got := modelNameForPane(tmux.Pane{Type: tmux.AgentGemini}); got != defaults.DefaultGemini {
		t.Errorf("default gemini model = %q, want %q", got, defaults.DefaultGemini)
	}
	if got := modelNameForPane(tmux.Pane{Type: tmux.AgentGrok}); got != defaults.DefaultGrok {
		t.Errorf("default grok model = %q, want %q", got, defaults.DefaultGrok)
	}
	if got := modelNameForPane(tmux.Pane{Type: tmux.AgentMuse}); got != "muse-wonder-1.0" {
		t.Errorf("default muse model = %q, want %q", got, "muse-wonder-1.0")
	}
	if got := modelNameForPane(tmux.Pane{Type: tmux.AgentOllama}); got != defaults.DefaultOllama {
		t.Errorf("default ollama model = %q, want %q", got, defaults.DefaultOllama)
	}
	if got := modelNameForPane(tmux.Pane{Type: tmux.AgentUser}); got != "" {
		t.Errorf("default user model = %q, want empty", got)
	}

	cfg = config.Default()
	cfg.Models.DefaultClaude = "claude-test"
	cfg.Models.DefaultCodex = "codex-test"
	cfg.Models.DefaultGemini = "gemini-test"
	cfg.Models.DefaultGrok = "grok-test"
	cfg.Models.DefaultMuse = "muse-test"
	cfg.Models.DefaultOllama = "ollama-test"

	if got := modelNameForPane(tmux.Pane{Type: tmux.AgentClaude}); got != "claude-test" {
		t.Errorf("cfg claude model = %q", got)
	}
	if got := modelNameForPane(tmux.Pane{Type: tmux.AgentCodex}); got != "codex-test" {
		t.Errorf("cfg codex model = %q", got)
	}
	if got := modelNameForPane(tmux.Pane{Type: tmux.AgentGemini}); got != "gemini-test" {
		t.Errorf("cfg gemini model = %q", got)
	}
	if got := modelNameForPane(tmux.Pane{Type: tmux.AgentType("xai-grok-build")}); got != "grok-test" {
		t.Errorf("cfg grok alias model = %q", got)
	}
	if got := modelNameForPane(tmux.Pane{Type: tmux.AgentType("muse-code")}); got != "muse-test" {
		t.Errorf("cfg muse alias model = %q", got)
	}
	cfg.Models.DefaultMuse = ""
	if got := modelNameForPane(tmux.Pane{Type: tmux.AgentMuse}); got != defaults.DefaultMuse {
		t.Errorf("empty cfg muse model = %q, want compiled-in default %q", got, defaults.DefaultMuse)
	}
	if got := modelNameForPane(tmux.Pane{Type: tmux.AgentOllama}); got != "ollama-test" {
		t.Errorf("cfg ollama model = %q", got)
	}

	if got := modelNameForPane(tmux.Pane{Type: tmux.AgentClaude, Variant: "custom-variant"}); got != "custom-variant" {
		t.Errorf("variant override = %q", got)
	}
	if got := modelNameForPane(tmux.Pane{Type: tmux.AgentType("ollama"), Variant: "mistral"}); got != "mistral" {
		t.Errorf("ollama variant override = %q", got)
	}
}

// A Muse pane's title variant is the spawn alias, optionally with "@effort".
// `ntm status` must resolve it to the Muse model id, or the context registry
// falls back to the 128K default instead of Muse's 1,007,997-token window.
func TestModelNameForPaneResolvesMuseVariant(t *testing.T) {
	oldCfg := cfg
	t.Cleanup(func() { cfg = oldCfg })

	tests := []struct {
		variant string
		want    string
	}{
		{variant: "spark", want: "muse-spark-1.3"},
		{variant: "SPARK", want: "muse-spark-1.3"},
		{variant: "spark-1.2@low", want: "muse-spark-1.2"},
		{variant: "wonder-full@xhigh", want: "muse-wonder-full-1.0"},
		{variant: "muse-spark-1.2", want: "muse-spark-1.2"},
	}
	for _, withCfg := range []bool{false, true} {
		cfg = nil
		if withCfg {
			cfg = config.Default()
		}
		for _, tc := range tests {
			got := modelNameForPane(tmux.Pane{Type: tmux.AgentMuse, Variant: tc.variant})
			if got != tc.want {
				t.Errorf("cfg=%v variant %q: model = %q, want %q", withCfg, tc.variant, got, tc.want)
			}
			if limit := models.GetContextLimit(got); limit != 1007997 {
				t.Errorf("cfg=%v variant %q: context limit = %d, want 1007997", withCfg, tc.variant, limit)
			}
		}
	}

	cfg = config.Default()
	cfg.Models.Muse["fast"] = "muse-spark-1.2"
	if got := modelNameForPane(tmux.Pane{Type: tmux.AgentType("muse-code"), Variant: "fast@high"}); got != "muse-spark-1.2" {
		t.Errorf("operator alias variant = %q, want %q", got, "muse-spark-1.2")
	}
}

func TestSessionPanePresentationCanonicalizesAliases(t *testing.T) {

	th := theme.CatppuccinMocha
	ic := icons.ASCII

	tests := []struct {
		name      string
		agentType tmux.AgentType
		wantColor string
		wantIcon  string
	}{
		{name: "claude alias", agentType: tmux.AgentType("claude_code"), wantColor: string(th.Claude), wantIcon: ic.Claude},
		{name: "codex alias", agentType: tmux.AgentType("openai-codex"), wantColor: string(th.Codex), wantIcon: ic.Codex},
		{name: "gemini alias", agentType: tmux.AgentType("google-gemini"), wantColor: string(th.Gemini), wantIcon: ic.Gemini},
		{name: "grok alias", agentType: tmux.AgentType("xai-grok-build"), wantColor: string(th.Pink), wantIcon: ic.Robot},
		{name: "muse", agentType: tmux.AgentMuse, wantColor: string(th.Sky), wantIcon: ic.Robot},
		{name: "muse alias", agentType: tmux.AgentType("muse-code"), wantColor: string(th.Sky), wantIcon: ic.Robot},
		{name: "cursor", agentType: tmux.AgentCursor, wantColor: string(th.Cursor), wantIcon: ic.Cursor},
		{name: "windsurf alias", agentType: tmux.AgentType("ws"), wantColor: string(th.Windsurf), wantIcon: ic.Windsurf},
		{name: "aider", agentType: tmux.AgentAider, wantColor: string(th.Aider), wantIcon: ic.Aider},
		{name: "ollama", agentType: tmux.AgentOllama, wantColor: string(th.Ollama), wantIcon: ic.Ollama},
		{name: "user", agentType: tmux.AgentUser, wantColor: string(th.User), wantIcon: ic.Terminal},
		{name: "unknown", agentType: tmux.AgentType("mystery"), wantColor: string(th.Overlay), wantIcon: ic.Robot},
		{name: "pi", agentType: tmux.AgentPi, wantColor: string(th.Rosewater), wantIcon: ic.Robot},
		{name: "kimi alias", agentType: tmux.AgentType("moonshot"), wantColor: string(th.Sapphire), wantIcon: ic.Robot},
		{name: "opencode alias", agentType: tmux.AgentType("opencode"), wantColor: string(th.Opencode), wantIcon: ic.Robot},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			gotColor, gotIcon := sessionPanePresentation(tmux.Pane{Type: tc.agentType}, th, ic)
			if gotColor != tc.wantColor {
				t.Fatalf("sessionPanePresentation(%q) color = %q, want %q", tc.agentType, gotColor, tc.wantColor)
			}
			if gotIcon != tc.wantIcon {
				t.Fatalf("sessionPanePresentation(%q) icon = %q, want %q", tc.agentType, gotIcon, tc.wantIcon)
			}
		})
	}
}

// TestStatusAgentSummaryShowsEveryAgentType checks `ntm status` prints a row
// for Pi, Kimi and Opencode panes and counts a session of only those as
// running agents instead of "No agents running".
func TestStatusAgentSummaryShowsEveryAgentType(t *testing.T) {
	counts := output.AgentCountsResponse{}
	for _, paneType := range []tmux.AgentType{tmux.AgentPi, tmux.AgentKimi, tmux.AgentType("moonshot"), tmux.AgentOpencode} {
		incrementAgentCounts(&counts, paneType)
	}

	var buf bytes.Buffer
	plain := func(interface{}) string { return "" }
	writeStatusAgentSummary(&buf, counts, theme.CatppuccinMocha, icons.ASCII, plain, "")
	got := buf.String()
	for _, want := range []string{"Pi      1 instance(s)", "Kimi    2 instance(s)", "Opencode 1 instance(s)"} {
		if !strings.Contains(got, want) {
			t.Errorf("status agent summary missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "No agents running") {
		t.Errorf("status agent summary reports no agents for a Pi/Kimi/Opencode session:\n%s", got)
	}

	buf.Reset()
	writeStatusAgentSummary(&buf, output.AgentCountsResponse{User: 1, Total: 1}, theme.CatppuccinMocha, icons.ASCII, plain, "")
	if !strings.Contains(buf.String(), "No agents running") {
		t.Errorf("user-only session should report no agents running:\n%s", buf.String())
	}
}

// TestListAgentsSummaryShowsEveryAgentType checks the wide `ntm list` AGENTS
// column counts Pi, Kimi and Opencode panes.
func TestListAgentsSummaryShowsEveryAgentType(t *testing.T) {
	counts := &output.AgentCountsResponse{Claude: 2, Pi: 1, Kimi: 1, Muse: 1, Opencode: 3}
	if got, want := listAgentsSummary(counts), "2 CC, 1 Pi, 1 Kimi, 1 Muse, 3 OC"; got != want {
		t.Fatalf("listAgentsSummary() = %q, want %q", got, want)
	}
	if got := listAgentsSummary(&output.AgentCountsResponse{}); got != "-" {
		t.Fatalf("listAgentsSummary(empty) = %q, want -", got)
	}
	if got := listAgentsSummary(nil); got != "-" {
		t.Fatalf("listAgentsSummary(nil) = %q, want -", got)
	}
}

func TestModelNameForPaneCanonicalizesAliases(t *testing.T) {

	oldCfg := cfg
	cfg = nil
	t.Cleanup(func() { cfg = oldCfg })

	aliasDefaults := config.DefaultModels()
	if got := modelNameForPane(tmux.Pane{Type: tmux.AgentType("claude_code")}); got != aliasDefaults.DefaultClaude {
		t.Fatalf("claude alias model = %q, want %q", got, aliasDefaults.DefaultClaude)
	}
	if got := modelNameForPane(tmux.Pane{Type: tmux.AgentType("openai-codex")}); got != aliasDefaults.DefaultCodex {
		t.Fatalf("codex alias model = %q, want %q", got, aliasDefaults.DefaultCodex)
	}
	if got := modelNameForPane(tmux.Pane{Type: tmux.AgentType("google-gemini")}); got != aliasDefaults.DefaultGemini {
		t.Fatalf("gemini alias model = %q, want %q", got, aliasDefaults.DefaultGemini)
	}
	if got := modelNameForPane(tmux.Pane{Type: tmux.AgentType("meta-muse")}); got != aliasDefaults.DefaultMuse {
		t.Fatalf("muse alias model = %q, want %q", got, aliasDefaults.DefaultMuse)
	}
}

func TestZoomPanePresentationCanonicalizesAliases(t *testing.T) {

	th := theme.CatppuccinMocha
	ic := icons.ASCII

	tests := []struct {
		name      string
		agentType tmux.AgentType
		wantColor string
		wantIcon  string
	}{
		{name: "codex alias", agentType: tmux.AgentType("openai-codex"), wantColor: colorize(string(th.Codex)), wantIcon: ic.Codex},
		{name: "cursor", agentType: tmux.AgentCursor, wantColor: colorize(string(th.Cursor)), wantIcon: ic.Cursor},
		{name: "windsurf alias", agentType: tmux.AgentType("ws"), wantColor: colorize(string(th.Windsurf)), wantIcon: ic.Windsurf},
		{name: "aider", agentType: tmux.AgentAider, wantColor: colorize(string(th.Aider)), wantIcon: ic.Aider},
		{name: "user", agentType: tmux.AgentUser, wantColor: colorize(string(th.User)), wantIcon: ic.Terminal},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			gotColor, gotIcon := zoomPanePresentation(tmux.Pane{Type: tc.agentType}, th, ic)
			if gotColor != tc.wantColor {
				t.Fatalf("zoomPanePresentation(%q) color = %q, want %q", tc.agentType, gotColor, tc.wantColor)
			}
			if gotIcon != tc.wantIcon {
				t.Fatalf("zoomPanePresentation(%q) icon = %q, want %q", tc.agentType, gotIcon, tc.wantIcon)
			}
		})
	}
}

// paneIsController must classify by agent type, never pane index (ntm-qgjx):
// tmux indices are base-index dependent and the user shell sits at index 0 on
// default configs.
func TestPaneIsControllerByTypeNotIndex(t *testing.T) {
	cases := []struct {
		name string
		pane tmux.Pane
		want bool
	}{
		{"user pane at base-0 index", tmux.Pane{Index: 0, Type: tmux.AgentUser}, true},
		{"user pane at base-1 index", tmux.Pane{Index: 1, Type: tmux.AgentUser}, true},
		{"agent pane at index 1 is not controller", tmux.Pane{Index: 1, Type: tmux.AgentClaude}, false},
		{"agent pane at index 0 is not controller", tmux.Pane{Index: 0, Type: tmux.AgentCodex}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := paneIsController(tc.pane); got != tc.want {
				t.Errorf("paneIsController(%+v) = %v, want %v", tc.pane, got, tc.want)
			}
		})
	}
}
