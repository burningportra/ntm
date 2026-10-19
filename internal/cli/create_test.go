package cli

import (
	"testing"

	"github.com/Dicklesworthstone/ntm/internal/output"
	"github.com/Dicklesworthstone/ntm/internal/tmux"
)

func TestCreateAgentCountsMuse(t *testing.T) {
	for _, raw := range []tmux.AgentType{tmux.AgentMuse, "muse-code"} {
		if got := agentTypeToString(raw); got != "muse" {
			t.Fatalf("agentTypeToString(%q) = %q, want muse", raw, got)
		}
	}

	counts := output.AgentCountsResponse{}
	incrementAgentCounts(&counts, tmux.AgentMuse)
	incrementAgentCounts(&counts, tmux.AgentType("muse-code"))
	incrementAgentCounts(&counts, tmux.AgentKimi)

	if counts.Muse != 2 || counts.Kimi != 1 || counts.Claude != 0 || counts.Other != 0 {
		t.Fatalf("counts = %+v, want Muse=2 Kimi=1 and nothing else", counts)
	}
}
