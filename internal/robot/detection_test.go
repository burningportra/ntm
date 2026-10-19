package robot

import (
	"testing"

	"github.com/Dicklesworthstone/ntm/internal/tmux"
)

func TestDetectFromProcess(t *testing.T) {
	tests := []struct {
		name     string
		command  string
		wantType string
		wantConf float64
	}{
		{"claude process", "claude --help", "claude", 0.95},
		{"claude-code process", "claude-code", "claude", 0.95},
		{"codex process", "codex-cli run", "codex", 0.95},
		{"gemini process", "gemini-cli", "gemini", 0.95},
		{"grok process", "grok --always-approve", "grok", 0.95},
		{"grok absolute path", "/home/user/.local/bin/grok --always-approve", "grok", 0.95},
		{"grok windows executable", `C:\\Tools\\grok.exe --always-approve`, "grok", 0.95},
		{"ngrok is unrelated", "ngrok http 8080", "unknown", 0.0},
		{"grok exporter is unrelated", "grok-exporter --listen=:9090", "unknown", 0.0},
		{"grok argument is unrelated", "rg grok", "unknown", 0.0},
		{"aider process", "aider-chat", "aider", 0.95},
		{"cursor process", "cursor", "cursor", 0.95},
		{"windsurf process", "windsurf", "windsurf", 0.95},
		{"ollama process", "ollama serve", "ollama", 0.95},
		{"muse launcher", "muse --yolo --model muse-wonder-1.0", "muse", 0.95},
		{"muse binary as tmux reports it", "muse-bin-1.4.11", "muse", 0.95},
		{"muse binary absolute path", "/Users/x/.local/bin/muse-bin-1.4.11-R6667.1", "muse", 0.95},
		{"muse launcher under a claude dir", "/tmp/claude-501/bin/muse --yolo", "muse", 0.95},
		{"amuse is unrelated", "amuse", "unknown", 0.0},
		{"museum is unrelated", "museum --open", "unknown", 0.0},
		{"musescore is unrelated", "/Applications/musescore", "unknown", 0.0},
		{"muse argument is unrelated", "rg muse", "unknown", 0.0},
		{"unknown process", "vim", "unknown", 0.0},
		{"bash shell", "bash", "unknown", 0.0},
		{"zsh shell", "zsh", "unknown", 0.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			detection := detectFromProcess(tt.command)
			if detection.Type != tt.wantType {
				t.Errorf("detectFromProcess(%q) type = %v, want %v", tt.command, detection.Type, tt.wantType)
			}
			if detection.Confidence != tt.wantConf {
				t.Errorf("detectFromProcess(%q) confidence = %v, want %v", tt.command, detection.Confidence, tt.wantConf)
			}
			if tt.wantType != "unknown" && detection.Method != MethodProcess {
				t.Errorf("detectFromProcess(%q) method = %v, want %v", tt.command, detection.Method, MethodProcess)
			}
		})
	}
}

func TestDetectFromContent(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		wantType string
	}{
		{"claude mention", "Claude Code is ready", "claude"},
		{"claude prompt", "claude> help", "claude"},
		{"anthropic mention", "Powered by Anthropic", "claude"},
		{"codex prompt", "codex> generate", "codex"},
		{"openai codex", "OpenAI Codex CLI", "codex"},
		{"gemini prompt", "gemini> search", "gemini"},
		{"aider prompt", "aider> fix bug", "aider"},
		{"cursor ai", "Cursor AI ready", "cursor"},
		{"windsurf codeium", "Powered by Codeium", "windsurf"},
		{"ollama serve", "ollama serve llama3.2", "ollama"},
		{"muse idle screen", museTestIdleScreen, "muse"},
		{"plain shell", "$ ls -la\nREADME.md", "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			detection := detectFromContent(tt.content)
			if detection.Type != tt.wantType {
				t.Errorf("detectFromContent(%q) type = %v, want %v", tt.content, detection.Type, tt.wantType)
			}
			if tt.wantType != "unknown" && detection.Method != MethodContent {
				t.Errorf("detectFromContent(%q) method = %v, want %v", tt.content, detection.Method, MethodContent)
			}
		})
	}
}

func TestDetectFromTitle(t *testing.T) {
	tests := []struct {
		name     string
		title    string
		wantType string
	}{
		{"claude in title", "Claude Code - Editor", "claude"},
		{"codex in title", "Codex CLI Session", "codex"},
		{"gemini in title", "Gemini AI", "gemini"},
		{"aider in title", "Aider Chat", "aider"},
		{"ollama in title", "Ollama REPL", "ollama"},
		{"plain title", "Terminal", "unknown"},
		{"mixed case", "CLAUDE Agent", "claude"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			detection := DetectFromTitle(tt.title)
			if detection.Type != tt.wantType {
				t.Errorf("detectFromTitle(%q) type = %v, want %v", tt.title, detection.Type, tt.wantType)
			}
		})
	}
}

func TestDetectFromNTMTitle(t *testing.T) {
	tests := []struct {
		name     string
		title    string
		wantType string
		wantConf float64
	}{
		{"NTM claude pane", "myproject__cc_1", "claude", 0.9},
		{"NTM codex pane", "myproject__cod_2", "codex", 0.9},
		{"NTM gemini pane", "session__gmi_1", "gemini", 0.9},
		{"NTM grok pane", "session__grok_1", "grok", 0.9},
		{"NTM cursor pane", "session__cursor_1", "cursor", 0.9},
		{"NTM windsurf pane", "session__windsurf_2", "windsurf", 0.9},
		{"NTM ws pane", "session__ws_2", "windsurf", 0.9},
		{"NTM aider pane", "session__aider_3", "aider", 0.9},
		{"NTM ollama pane", "session__ollama_4", "ollama", 0.9},
		{"NTM muse pane", "session__muse_1", "muse", 0.9},
		{"museum session is not muse", "museum__notes", "unknown", 0.0},
		{"non-NTM title", "Terminal", "unknown", 0.0},
		{"partial match", "code_cc", "unknown", 0.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			detection := DetectFromNTMTitle(tt.title)
			if detection.Type != tt.wantType {
				t.Errorf("detectFromNTMTitle(%q) type = %v, want %v", tt.title, detection.Type, tt.wantType)
			}
			if detection.Confidence != tt.wantConf {
				t.Errorf("detectFromNTMTitle(%q) confidence = %v, want %v", tt.title, detection.Confidence, tt.wantConf)
			}
		})
	}
}

func TestDetectAgentTypeEnhanced(t *testing.T) {
	tests := []struct {
		name       string
		pane       tmux.Pane
		content    string
		wantType   string
		wantMethod DetectionMethod
	}{
		{
			name:       "tmux pane type overrides weaker heuristics",
			pane:       tmux.Pane{Type: tmux.AgentClaude, Command: "bash", Title: "Terminal"},
			content:    "codex> misleading prompt",
			wantType:   "claude",
			wantMethod: MethodTmuxPane,
		},
		{
			name:       "process detection highest priority",
			pane:       tmux.Pane{Command: "claude-code", Title: "Terminal"},
			content:    "random output",
			wantType:   "claude",
			wantMethod: MethodProcess,
		},
		{
			name:       "content detection when no process match",
			pane:       tmux.Pane{Command: "bash", Title: "Terminal"},
			content:    "Claude Code is ready\n>",
			wantType:   "claude",
			wantMethod: MethodContent,
		},
		{
			name:       "title detection for ollama fallback",
			pane:       tmux.Pane{Command: "bash", Title: "Ollama REPL"},
			content:    "",
			wantType:   "ollama",
			wantMethod: MethodTitle,
		},
		{
			name:       "NTM short title detection",
			pane:       tmux.Pane{Command: "bash", Title: "project__cc_1"},
			content:    "",
			wantType:   "claude",
			wantMethod: MethodTitle,
		},
		{
			name:       "NTM full title detection for new agents",
			pane:       tmux.Pane{Command: "bash", Title: "project__ollama_4"},
			content:    "",
			wantType:   "ollama",
			wantMethod: MethodTitle,
		},
		{
			name:       "NTM alias title detection for windsurf",
			pane:       tmux.Pane{Command: "bash", Title: "project__ws_2"},
			content:    "",
			wantType:   "windsurf",
			wantMethod: MethodTitle,
		},
		{
			name:       "unknown when nothing matches",
			pane:       tmux.Pane{Command: "vim", Title: "editor"},
			content:    "editing file.txt",
			wantType:   "unknown",
			wantMethod: MethodUnknown,
		},
		{
			name:       "muse pane type",
			pane:       tmux.Pane{Type: tmux.AgentMuse, Command: "muse-bin-1.4.11", Title: "proj"},
			content:    museTestIdleScreen,
			wantType:   "muse",
			wantMethod: MethodTmuxPane,
		},
		{
			// Muse titles its pane with the cwd basename.
			name:       "muse process with cwd title",
			pane:       tmux.Pane{Command: "muse-bin-1.4.11", Title: "claude-port"},
			content:    museTestWorkingScreen,
			wantType:   "muse",
			wantMethod: MethodProcess,
		},
		{
			name:       "muse screen behind a shell command",
			pane:       tmux.Pane{Command: "zsh", Title: "claude-port"},
			content:    museTestFinishedScreen,
			wantType:   "muse",
			wantMethod: MethodContent,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			detection := DetectAgentTypeEnhanced(tt.pane, tt.content)
			if detection.Type != tt.wantType {
				t.Errorf("DetectAgentTypeEnhanced() type = %v, want %v", detection.Type, tt.wantType)
			}
			if detection.Method != tt.wantMethod {
				t.Errorf("DetectAgentTypeEnhanced() method = %v, want %v", detection.Method, tt.wantMethod)
			}
		})
	}
}

// Muse Code screens captured from muse 1.4.11 (echo provider) in a 160x45
// tmux pane, rules shortened. The cwd sits under /private/tmp/claude-501, as
// ntm scratch projects do, and the prompt mentions Claude Code, so the
// screens carry the same words Claude detection keys on.
const (
	museTestRule      = "────────────────────────────────────────────────────────────"
	museTestVoiceBar  = "── Voice input (alt(option)+v to start) ──────────────────────"
	museTestStatusBar = "  muse-wonder-1.0 · /private/tmp/claude-501/claude-port · YOLO"
	museTestBanner    = "  Muse Code 1.4.11\n\n" +
		"  Skills: 276 loaded · 40 warnings · 76 details hidden (ctrl+o to expand)\n\n"
	museTestChrome = museTestVoiceBar + "\n❯\n" + museTestRule + "\n" + museTestStatusBar

	museTestIdleScreen = museTestBanner + museTestChrome

	museTestWorkingScreen = museTestBanner +
		"❯ port the Claude Code hooks\n\n" +
		"◆ Thinking (0s · esc to interrupt)\n\n" +
		museTestChrome

	museTestFinishedScreen = museTestBanner +
		"❯ port the Claude Code hooks\n\n" +
		"◆ echo: port the Claude Code hooks\n\n" +
		museTestChrome

	// After Ctrl+C or Esc cancels a turn, Muse puts the prompt back in the
	// composer as a draft.
	museTestInterruptedScreen = museTestBanner +
		museTestVoiceBar + "\n❯ port the Claude Code hooks\n" + museTestRule + "\n" + museTestStatusBar
)

// TestDetectFromContent_MuseScreensNeverClaude guards the detection order:
// Muse shares Claude Code's "❯" composer and "esc to interrupt" hint, and a
// transcript that mentions Claude Code matches Claude's content patterns, so
// checking those first would report a Muse pane as claude.
func TestDetectFromContent_MuseScreensNeverClaude(t *testing.T) {
	screens := map[string]string{
		"idle":        museTestIdleScreen,
		"working":     museTestWorkingScreen,
		"finished":    museTestFinishedScreen,
		"interrupted": museTestInterruptedScreen,
	}
	for name, screen := range screens {
		t.Run(name, func(t *testing.T) {
			if name != "idle" && !matchesContentPatternsFor("claude", screen) {
				t.Fatalf("fixture %q should match a claude content pattern so the ordering is exercised", name)
			}
			detection := detectFromContent(screen)
			if detection.Type != "muse" {
				t.Fatalf("detectFromContent(%s Muse screen) = %q, want muse", name, detection.Type)
			}
			if detection.Method != MethodContent {
				t.Fatalf("detectFromContent(%s Muse screen) method = %q, want %q", name, detection.Method, MethodContent)
			}
		})
	}
}

// Claude Code 2.1 screens captured from live panes (rules shortened). Claude
// draws the same "❯" composer between rules as Muse, and its working footer
// says "esc to interrupt", so a Muse signature built on either would claim
// them.
const (
	claudeTestRule   = "────────────────────────────────────────────────────────────"
	claudeTestIdle   = "  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents"
	claudeTestBusy   = "  ⏵⏵ bypass permissions on (shift+tab to cycle) · esc to interrupt"
	claudeTestChrome = claudeTestRule + "\n❯\n" + claudeTestRule + "\n"

	claudeTestIdleScreen = "● Tests pass; the change is ready for review.\n\n" +
		"✻ Cooked for 6m 48s · done 7:21 PM\n" + claudeTestChrome + claudeTestIdle

	claudeTestWorkingScreen = "● Running the integration suite now.\n\n" +
		"✻ Burrowing… (37s · ↓ 616 tokens)\n" + claudeTestChrome + claudeTestBusy

	// Claude talking about Muse, and showing `muse --version` in a tool result.
	claudeTestMentionsMuseScreen = "● Muse Code shares the ❯ composer, so muse-spark-1.3 panes need their own idle check.\n\n" +
		"● Bash(muse --version)\n" +
		"  ⎿  Muse Code 1.4.11 (1.4.11-R6667.1)\n\n" +
		"✻ Cooked for 1m 12s · done 7:24 PM\n" + claudeTestChrome + claudeTestIdle

	// Claude summarizing results on "·"-separated lines. Muse's status bar is
	// "<model> · <cwd> · YOLO", so it is the muse- model name, not the "·"
	// layout, that identifies it.
	claudeTestDotSeparatedReplyScreen = "● Ran each package's tests:\n" +
		"  robot · 412 passed · 0 failed\n" +
		"  tmux · 96 passed · 0 failed\n\n" +
		"✻ Cooked for 2m 3s · done 7:30 PM\n" + claudeTestChrome + claudeTestIdle
)

// TestDetectFromContent_ClaudeScreensNeverMuse is the other half of the
// detection-order guard: Muse is checked first, so its signature must not
// match a Claude Code screen.
func TestDetectFromContent_ClaudeScreensNeverMuse(t *testing.T) {
	for name, screen := range map[string]string{
		"idle":          claudeTestIdleScreen,
		"working":       claudeTestWorkingScreen,
		"mentions muse": claudeTestMentionsMuseScreen,
		"dot separated": claudeTestDotSeparatedReplyScreen,
	} {
		t.Run(name, func(t *testing.T) {
			if detection := detectFromContent(screen); detection.Type == "muse" {
				t.Fatalf("detectFromContent(%s Claude screen) = muse", name)
			}
			// tmux reports a Claude Code 2.1 pane's command as its version,
			// so an untitled pane falls through to content detection.
			pane := tmux.Pane{Command: "2.1.303", Title: "claude-port"}
			if detection := DetectAgentTypeEnhanced(pane, screen); detection.Type == "muse" {
				t.Fatalf("DetectAgentTypeEnhanced(%s Claude screen) = muse via %s", name, detection.Method)
			}
		})
	}
}

func TestDetectFromContent_MuseWordsAloneAreNotMuse(t *testing.T) {
	for _, content := range []string{
		"$ open museum.html\n$",
		"amuse-bouche recipes\n%",
		"muse: a source of inspiration",
	} {
		if detection := detectFromContent(content); detection.Type == "muse" {
			t.Errorf("detectFromContent(%q) = muse, want not muse", content)
		}
	}
}

func matchesContentPatternsFor(agentType, content string) bool {
	for _, cp := range contentPatterns {
		if cp.agentType != agentType {
			continue
		}
		for _, pattern := range cp.patterns {
			if pattern.MatchString(content) {
				return true
			}
		}
	}
	return false
}
