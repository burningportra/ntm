//go:build ensemble_experimental
// +build ensemble_experimental

package cli

import (
	"strings"
	"testing"
)

// TestParseAgentMixAcceptsEveryPromptableAgentType checks --agent-mix takes
// the types the old list rejected (pi, kimi, muse) plus opencode and aliases,
// rejects a bogus type with the valid list, and rejects Grok up front because
// every ensemble pane needs an injected prompt Grok can't receive yet.
func TestParseAgentMixAcceptsEveryPromptableAgentType(t *testing.T) {
	mix, err := parseAgentMix("pi=1, kimi=1, moonshot=1, muse=2, opencode=1, cc=1")
	if err != nil {
		t.Fatalf("parseAgentMix: %v", err)
	}
	want := map[string]int{"pi": 1, "kimi": 2, "muse": 2, "oc": 1, "cc": 1}
	if len(mix) != len(want) {
		t.Fatalf("parseAgentMix = %v, want %v", mix, want)
	}
	for agentType, count := range want {
		if mix[agentType] != count {
			t.Fatalf("parseAgentMix[%q] = %d, want %d (full %v)", agentType, mix[agentType], count, mix)
		}
	}

	_, err = parseAgentMix("bogus=1")
	if err == nil {
		t.Fatal("parseAgentMix(bogus=1) returned nil, want error")
	}
	for _, want := range []string{`"bogus=1"`, "muse", "kimi", "pi", "opencode|oc"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("parseAgentMix(bogus=1) error %q does not mention %q", err, want)
		}
	}

	_, err = parseAgentMix("grok=1")
	if err == nil || !strings.Contains(err.Error(), "Grok Build prompt delivery") {
		t.Fatalf("parseAgentMix(grok=1) error = %v, want the Grok prompt-delivery hint", err)
	}
}
