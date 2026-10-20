//go:build ensemble_experimental
// +build ensemble_experimental

package robot

import (
	"strings"
	"testing"
)

func TestNormalizeEnsembleAgentType_Aliases(t *testing.T) {

	tests := []struct {
		input string
		want  string
	}{
		{"cc", "cc"},
		{"claude", "cc"},
		{"claude-code", "cc"},
		{"claude_code", "cc"},
		{"cod", "cod"},
		{"codex", "cod"},
		{"openai", "cod"},
		{"openai-codex", "cod"},
		{"codex-cli", "cod"},
		{"codex_cli", "cod"},
		{"gmi", "gmi"},
		{"gemini", "gmi"},
		{"google", "gmi"},
		{"google-ai", "gmi"},
		{"google-gemini", "gmi"},
		{"gemini-cli", "gmi"},
		{"gemini_cli", "gmi"},
		{"cursor", "cursor"},
		{"ws", "windsurf"},
		{"windsurf", "windsurf"},
		{"aider", "aider"},
		{"ollama", "ollama"},
		{"muse", "muse"},
		{"muse-code", "muse"},
		{"opencode", "oc"},
		{"oc", "oc"},
		{"grok", "grok"},
		{"unknown", ""},
		{"user", ""},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := normalizeEnsembleAgentType(tt.input); got != tt.want {
				t.Fatalf("normalizeEnsembleAgentType(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseEnsembleAgentMix_AcceptsSharedAliases(t *testing.T) {

	mix, err := parseEnsembleAgentMix("openai-codex=2, google-gemini=1, ws=1")
	if err != nil {
		t.Fatalf("parseEnsembleAgentMix error: %v", err)
	}

	if mix["cod"] != 2 {
		t.Fatalf("cod count = %d, want 2", mix["cod"])
	}
	if mix["gmi"] != 1 {
		t.Fatalf("gmi count = %d, want 1", mix["gmi"])
	}
	if mix["windsurf"] != 1 {
		t.Fatalf("windsurf count = %d, want 1", mix["windsurf"])
	}
}

// TestEnsembleAgentTypeAcceptsEveryPromptableAgentType covers --agents and
// explicit mode:agent specs: opencode (missing from the old list) and the
// newer types resolve, a bogus type fails with the valid list, and Grok fails
// with its prompt-delivery hint before any pane is created.
func TestEnsembleAgentTypeAcceptsEveryPromptableAgentType(t *testing.T) {
	mix, err := parseEnsembleAgentMix("pi=1, kimi=1, k2=1, muse-code=1, opencode=2")
	if err != nil {
		t.Fatalf("parseEnsembleAgentMix: %v", err)
	}
	want := map[string]int{"pi": 1, "kimi": 2, "muse": 1, "oc": 2}
	if len(mix) != len(want) {
		t.Fatalf("parseEnsembleAgentMix = %v, want %v", mix, want)
	}
	for agentType, count := range want {
		if mix[agentType] != count {
			t.Fatalf("parseEnsembleAgentMix[%q] = %d, want %d (full %v)", agentType, mix[agentType], count, mix)
		}
	}

	_, err = parseEnsembleAgentMix("bogus=1")
	if err == nil {
		t.Fatal("parseEnsembleAgentMix(bogus=1) returned nil, want error")
	}
	for _, want := range []string{`"bogus"`, "muse", "kimi", "pi", "oc"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("parseEnsembleAgentMix(bogus=1) error %q does not mention %q", err, want)
		}
	}

	if _, err := parseEnsembleAgentMix("grok=1"); err == nil || !strings.Contains(err.Error(), "Grok Build prompt delivery") {
		t.Fatalf("parseEnsembleAgentMix(grok=1) error = %v, want the Grok prompt-delivery hint", err)
	}
	if _, err := ensembleAgentType("grok-build"); err == nil || !strings.Contains(err.Error(), "Grok Build prompt delivery") {
		t.Fatalf("ensembleAgentType(grok-build) error = %v, want the Grok prompt-delivery hint", err)
	}
	if got, err := ensembleAgentType("opencode"); err != nil || got != "oc" {
		t.Fatalf("ensembleAgentType(opencode) = %q, %v; want oc", got, err)
	}
}
