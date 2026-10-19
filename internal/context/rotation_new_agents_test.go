package context

import (
	"testing"
)

func TestDeriveAgentTypeFromID_NewAgents(t *testing.T) {
	t.Parallel()

	tests := []struct {
		agentID string
		want    string
	}{
		{"myproject__cursor_1", "cursor"},
		{"myproject__windsurf_2", "windsurf"},
		{"myproject__ws_2", "windsurf"},
		{"myproject__aider_3", "aider"},
		{"myproject__ollama_4", "ollama"},
		{"myproject__muse_5", "muse"},
		{"myproject__cursor_1_variant", "cursor"},
	}

	for _, tt := range tests {
		t.Run(tt.agentID, func(t *testing.T) {
			got := deriveAgentTypeFromID(tt.agentID)
			if got != tt.want {
				t.Errorf("deriveAgentTypeFromID(%q) = %q, want %q", tt.agentID, got, tt.want)
			}
		})
	}
}

func TestAgentTypeShort_NewAgents(t *testing.T) {
	t.Parallel()

	tests := []struct {
		agentType string
		want      string
	}{
		{"cursor", "cursor"},
		{"windsurf", "windsurf"},
		{"ws", "windsurf"},
		{"aider", "aider"},
		{"ollama", "ollama"},
		{"muse", "muse"},
		{"muse-code", "muse"},
	}

	for _, tt := range tests {
		t.Run(tt.agentType, func(t *testing.T) {
			got := agentTypeShort(tt.agentType)
			if got != tt.want {
				t.Errorf("agentTypeShort(%q) = %q, want %q", tt.agentType, got, tt.want)
			}
		})
	}
}

func TestAgentTypeLong_NewAgents(t *testing.T) {
	t.Parallel()

	tests := []struct {
		shortType string
		want      string
	}{
		{"cursor", "cursor"},
		{"windsurf", "windsurf"},
		{"ws", "windsurf"},
		{"aider", "aider"},
		{"ollama", "ollama"},
		{"muse", "muse"},
		{"meta-muse", "muse"},
	}

	for _, tt := range tests {
		t.Run(tt.shortType, func(t *testing.T) {
			got := agentTypeLong(tt.shortType)
			if got != tt.want {
				t.Errorf("agentTypeLong(%q) = %q, want %q", tt.shortType, got, tt.want)
			}
		})
	}
}

func TestDefaultPaneSpawnerGetAgentCommand_NewAgents(t *testing.T) {
	t.Parallel()

	// Without config
	spawner := NewDefaultPaneSpawner(nil)

	tests := []struct {
		agentType string
		want      string
	}{
		{"cursor", "cursor"},
		{"windsurf", "windsurf"},
		{"ws", "windsurf"},
		{"aider", "aider"},
		{"ollama", "ollama"},
		{"muse", "muse"},
		{"muse-code", "muse"},
	}

	for _, tt := range tests {
		t.Run(tt.agentType, func(t *testing.T) {
			got := spawner.getAgentCommand(tt.agentType)
			if got != tt.want {
				t.Errorf("getAgentCommand(%q) = %q, want %q", tt.agentType, got, tt.want)
			}
		})
	}
}
