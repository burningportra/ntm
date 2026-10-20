package agent

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/ntm/internal/util"
)

func TestMatchAny(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		patterns []string
		want     bool
	}{
		{
			name:     "exact match",
			text:     "You've hit your limit",
			patterns: ccRateLimitPatterns,
			want:     true,
		},
		{
			name:     "case insensitive match",
			text:     "RATE LIMIT EXCEEDED",
			patterns: ccRateLimitPatterns,
			want:     true,
		},
		{
			name:     "no match",
			text:     "Everything is working fine",
			patterns: ccRateLimitPatterns,
			want:     false,
		},
		{
			name:     "partial match in longer text",
			text:     "Error: You've hit your limit. Please wait and try again.",
			patterns: ccRateLimitPatterns,
			want:     true,
		},
		{
			name:     "working pattern - code block",
			text:     "Here's the code:\n```go\nfunc main() {}\n```",
			patterns: ccWorkingPatterns,
			want:     true,
		},
		{
			name:     "working pattern - file operation",
			text:     "Writing to internal/agent/types.go",
			patterns: ccWorkingPatterns,
			want:     true,
		},
		{
			name:     "cursor rate limit",
			text:     "rate limit hit, please wait",
			patterns: cursorRateLimitPatterns,
			want:     true,
		},
		{
			name:     "cursor working",
			text:     "analyzing your code for issues",
			patterns: cursorWorkingPatterns,
			want:     true,
		},
		{
			name:     "windsurf rate limit",
			text:     "too many requests, slow down",
			patterns: windsurfRateLimitPatterns,
			want:     true,
		},
		{
			name:     "windsurf working",
			text:     "searching for references in project",
			patterns: windsurfWorkingPatterns,
			want:     true,
		},
		{
			name:     "aider rate limit",
			text:     "quota exceeded for today",
			patterns: aiderRateLimitPatterns,
			want:     true,
		},
		{
			name:     "aider working - applied edit",
			text:     "applied edit to main.py",
			patterns: aiderWorkingPatterns,
			want:     true,
		},
		{
			name:     "aider working - repo-map",
			text:     "updating repo-map for context",
			patterns: aiderWorkingPatterns,
			want:     true,
		},
		{
			name:     "kimi rate limit",
			text:     "quota exceeded, try again later",
			patterns: kimiRateLimitPatterns,
			want:     true,
		},
		{
			name:     "kimi working",
			text:     "editing internal/agent/types.go",
			patterns: kimiWorkingPatterns,
			want:     true,
		},
		{
			name:     "muse rate limit",
			text:     "Rate limit reached, try again later",
			patterns: museRateLimitPatterns,
			want:     true,
		},
		{
			name:     "muse working line",
			text:     "◇ Thinking (1s · esc to interrupt)",
			patterns: museWorkingPatterns,
			want:     true,
		},
		{
			name:     "muse reply is not working",
			text:     "◆ echo: hello there",
			patterns: museWorkingPatterns,
			want:     false,
		},
		{
			name:     "cursor error",
			text:     "error: connection lost",
			patterns: cursorErrorPatterns,
			want:     true,
		},
		{
			name:     "windsurf error",
			text:     "failed: invalid configuration",
			patterns: windsurfErrorPatterns,
			want:     true,
		},
		{
			name:     "aider error",
			text:     "exception: file not found",
			patterns: aiderErrorPatterns,
			want:     true,
		},
		{
			name:     "kimi error",
			text:     "error: connection refused",
			patterns: kimiErrorPatterns,
			want:     true,
		},
		{
			name:     "muse error",
			text:     "Error: connection refused",
			patterns: museErrorPatterns,
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchAny(tt.text, tt.patterns); got != tt.want {
				t.Errorf("matchAny() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMatchAnyRegex(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		patterns []*regexp.Regexp
		want     bool
	}{
		{
			name:     "prompt match",
			text:     "What would you like me to do? >",
			patterns: ccIdlePatterns,
			want:     true,
		},
		{
			name:     "human prompt",
			text:     "Human: ",
			patterns: ccIdlePatterns,
			want:     true,
		},
		{
			name:     "codex prompt",
			text:     "47% context left · ? for shortcuts",
			patterns: codIdlePatterns,
			want:     true,
		},
		{
			name:     "codex chevron prompt",
			text:     "› Write tests for @filename",
			patterns: codIdlePatterns,
			want:     true,
		},
		{
			name:     "no match",
			text:     "Processing your request...",
			patterns: ccIdlePatterns,
			want:     false,
		},
		{
			name:     "cursor prompt",
			text:     "cursor> ",
			patterns: cursorIdlePatterns,
			want:     true,
		},
		{
			name:     "windsurf prompt",
			text:     "windsurf> ",
			patterns: windsurfIdlePatterns,
			want:     true,
		},
		{
			name:     "aider prompt",
			text:     "aider> ",
			patterns: aiderIdlePatterns,
			want:     true,
		},
		{
			name:     "kimi prompt",
			text:     "kimi> ",
			patterns: kimiIdlePatterns,
			want:     true,
		},
		{
			name:     "muse empty composer",
			text:     "── Voice input (alt(option)+v to start) ──\n❯\n──────\n  muse-wonder-1.0 · /repo · YOLO",
			patterns: museIdlePatterns,
			want:     true,
		},
		{
			name:     "muse composer with draft",
			text:     "── Voice input (alt(option)+v to start) ──\n❯ draft text here\n──────",
			patterns: museIdlePatterns,
			want:     false,
		},
		{
			name:     "muse composer showing a tip",
			text:     "── Voice input (alt(option)+v to start) ──\n❯ Type @ to search and insert workspace file paths\n──────\n  echo · /repo · YOLO",
			patterns: museIdlePatterns,
			want:     true,
		},
		{
			name:     "muse composer showing a tip cut short",
			text:     "── Voice input (alt(option)+v to start) ──\n❯ Press ? on an empty composer to see keyb…\n──────",
			patterns: museIdlePatterns,
			want:     true,
		},
		{
			name:     "muse draft that starts like a tip",
			text:     "── Voice input (alt(option)+v to start) ──\n❯ Type a summary of the bug\n──────",
			patterns: museIdlePatterns,
			want:     false,
		},
		{
			name:     "muse multi-line draft with an empty first line",
			text:     "── Voice input (alt(option)+v to start) ──\n❯\n  second line\n──────",
			patterns: museIdlePatterns,
			want:     false,
		},
		{
			name:     "muse tip text echoed as a submitted prompt",
			text:     "❯ Type @ to search and insert workspace file paths\n\n◆ echo: Type @ to search",
			patterns: museIdlePatterns,
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchAnyRegex(tt.text, tt.patterns); got != tt.want {
				t.Errorf("matchAnyRegex() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCollectMatches(t *testing.T) {
	text := "Rate limit exceeded. Please wait and try again later."
	matches := collectMatches(text, ccRateLimitPatterns)

	if len(matches) == 0 {
		t.Error("Expected at least one match")
	}

	// Should find multiple patterns
	found := map[string]bool{}
	for _, m := range matches {
		found[m] = true
	}

	if !found["rate limit"] && !found["rate limit exceeded"] {
		t.Error("Expected to find 'rate limit' pattern")
	}
	if !found["please wait"] {
		t.Error("Expected to find 'please wait' pattern")
	}
}

func TestExtractFloat(t *testing.T) {
	tests := []struct {
		name    string
		pattern *regexp.Regexp
		text    string
		want    *float64
	}{
		{
			name:    "codex context percentage",
			pattern: codContextPattern,
			text:    "47% context left · ? for shortcuts",
			want:    floatPtr(47.0),
		},
		{
			name:    "codex low context",
			pattern: codContextPattern,
			text:    "5% context left",
			want:    floatPtr(5.0),
		},
		{
			name:    "gemini memory",
			pattern: gmiMemoryPattern,
			text:    "gemini-3-pro-preview /model | 396.8 MB",
			want:    floatPtr(396.8),
		},
		{
			name:    "no match returns nil",
			pattern: codContextPattern,
			text:    "No context information here",
			want:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractFloat(tt.pattern, tt.text)
			if tt.want == nil {
				if got != nil {
					t.Errorf("extractFloat() = %v, want nil", *got)
				}
			} else {
				if got == nil {
					t.Errorf("extractFloat() = nil, want %v", *tt.want)
				} else if *got != *tt.want {
					t.Errorf("extractFloat() = %v, want %v", *got, *tt.want)
				}
			}
		})
	}
}

func TestExtractInt(t *testing.T) {
	tests := []struct {
		name    string
		pattern *regexp.Regexp
		text    string
		want    *int64
	}{
		{
			name:    "codex token count with commas",
			pattern: codTokenPattern,
			text:    "Token usage: total=219,582 input=206,150",
			want:    intPtr(219582),
		},
		{
			name:    "simple number",
			pattern: codTokenPattern,
			text:    "Token usage: total=1000",
			want:    intPtr(1000),
		},
		{
			name:    "no match",
			pattern: codTokenPattern,
			text:    "No token information",
			want:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractInt(tt.pattern, tt.text)
			if tt.want == nil {
				if got != nil {
					t.Errorf("extractInt() = %v, want nil", *got)
				}
			} else {
				if got == nil {
					t.Errorf("extractInt() = nil, want %v", *tt.want)
				} else if *got != *tt.want {
					t.Errorf("extractInt() = %v, want %v", *got, *tt.want)
				}
			}
		})
	}
}

func TestGetLastNLines(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		n     int
		lines int // expected number of lines
	}{
		{
			name:  "more lines than n",
			text:  "line1\nline2\nline3\nline4\nline5",
			n:     3,
			lines: 3,
		},
		{
			name:  "fewer lines than n",
			text:  "line1\nline2",
			n:     5,
			lines: 2,
		},
		{
			name:  "exactly n lines",
			text:  "line1\nline2\nline3",
			n:     3,
			lines: 3,
		},
		{
			name:  "empty text",
			text:  "",
			n:     3,
			lines: 1, // Split produces one empty element
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := util.GetLastNLines(tt.text, tt.n)
			// Count lines in result
			var lineCount int
			if result == "" {
				lineCount = 0
			} else {
				lineCount = 1
				for _, c := range result {
					if c == '\n' {
						lineCount++
					}
				}
			}

			// For non-empty text, verify we got the right count
			if tt.text != "" && lineCount > tt.n {
				t.Errorf("getLastNLines() returned %d lines, want at most %d", lineCount, tt.n)
			}
		})
	}
}

func TestStripANSICodes(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "color codes",
			text: "\x1b[32mSuccess\x1b[0m",
			want: "Success",
		},
		{
			name: "bold and color",
			text: "\x1b[1;31mError:\x1b[0m Something went wrong",
			want: "Error: Something went wrong",
		},
		{
			name: "no codes",
			text: "Plain text",
			want: "Plain text",
		},
		{
			name: "cursor movement",
			text: "\x1b[2J\x1b[HClear screen",
			want: "Clear screen",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stripANSICodes(tt.text); got != tt.want {
				t.Errorf("stripANSICodes() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHeaderPatterns(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		text    string
		pattern *regexp.Regexp
		want    bool
	}{
		{
			name:    "claude opus",
			text:    "Claude Opus 4.5 is ready",
			pattern: ccHeaderPattern,
			want:    true,
		},
		{
			name:    "claude sonnet",
			text:    "Using sonnet 3.5",
			pattern: ccHeaderPattern,
			want:    true,
		},
		{
			name:    "codex",
			text:    "OpenAI Codex CLI",
			pattern: codHeaderPattern,
			want:    true,
		},
		{
			name:    "gpt model",
			text:    "GPT-4 turbo",
			pattern: codHeaderPattern,
			want:    true,
		},
		{
			name:    "gemini",
			text:    "gemini-2.0-flash-preview",
			pattern: gmiHeaderPattern,
			want:    true,
		},
		{
			name:    "google ai",
			text:    "Google AI Studio",
			pattern: gmiHeaderPattern,
			want:    true,
		},
		{
			name:    "cursor header",
			text:    "Cursor AI ready",
			pattern: cursorHeaderPattern,
			want:    true,
		},
		{
			name:    "cursor plain",
			text:    "cursor session started",
			pattern: cursorHeaderPattern,
			want:    true,
		},
		{
			name:    "windsurf header",
			text:    "Windsurf IDE connected",
			pattern: windsurfHeaderPattern,
			want:    true,
		},
		{
			name:    "windsurf plain",
			text:    "windsurf is ready",
			pattern: windsurfHeaderPattern,
			want:    true,
		},
		{
			name:    "aider header",
			text:    "aider chat session",
			pattern: aiderHeaderPattern,
			want:    true,
		},
		{
			name:    "aider plain",
			text:    "Aider started",
			pattern: aiderHeaderPattern,
			want:    true,
		},
		{
			name:    "kimi header",
			text:    "Kimi Code CLI v1.0",
			pattern: kimiHeaderPattern,
			want:    true,
		},
		{
			name:    "kimi moonshot",
			text:    "Moonshot AI",
			pattern: kimiHeaderPattern,
			want:    true,
		},
		{
			name:    "kimi cli",
			text:    "kimi is ready",
			pattern: kimiHeaderPattern,
			want:    true,
		},
		{
			name:    "muse banner",
			text:    "\n  Muse Code 1.4.11\n",
			pattern: museHeaderPattern,
			want:    true,
		},
		{
			name:    "muse banner without version",
			text:    "I read the Muse Code docs",
			pattern: museHeaderPattern,
			want:    false,
		},
		{
			// IsMuseOutput applies the status-bar pattern line by line.
			name:    "muse status bar",
			text:    "  muse-wonder-full-1.0 · max · /private/tmp · YOLO",
			pattern: museStatusBarPattern,
			want:    true,
		},
		{
			name:    "muse model id in prose",
			text:    "switch to muse-spark-1.3 for speed",
			pattern: museStatusBarPattern,
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.pattern.MatchString(tt.text); got != tt.want {
				t.Errorf("HeaderPattern.MatchString(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

func TestYoloModePattern(t *testing.T) {
	tests := []struct {
		text string
		want bool
	}{
		{"YOLO mode: ON", true},
		{"YOLO mode: OFF", true},
		{"yolo mode: on", true},
		{"No YOLO here", false},
	}

	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			if got := gmiYoloPattern.MatchString(tt.text); got != tt.want {
				t.Errorf("gmiYoloPattern.MatchString(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

func TestShellModePattern(t *testing.T) {
	tests := []struct {
		text string
		want bool
	}{
		{"! ls -la", true},
		{"!ls", true},
		{"  ! pwd", false}, // Leading whitespace - pattern starts with ^
		{"Normal text", false},
	}

	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			if got := gmiShellModePattern.MatchString(tt.text); got != tt.want {
				t.Errorf("gmiShellModePattern.MatchString(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

// Test that all regex patterns compile correctly
func TestAllPatternsCompile(t *testing.T) {
	// If we got here, all patterns compiled (they're package-level vars)
	// This test documents that fact and catches any future regex errors

	patterns := []*regexp.Regexp{
		codContextPattern,
		codTokenPattern,
		gmiMemoryPattern,
		gmiYoloPattern,
		gmiShellModePattern,
		ccHeaderPattern,
		codHeaderPattern,
		gmiHeaderPattern,
		kimiHeaderPattern,
		museHeaderPattern,
		museStatusBarPattern,
		museActiveWorkLineRe,
		cursorHeaderPattern,
		windsurfHeaderPattern,
		aiderHeaderPattern,
		ansiPattern,
	}

	for i, p := range patterns {
		if p == nil {
			t.Errorf("Pattern %d is nil", i)
		}
	}

	// Check slice patterns
	patternSlices := [][]*regexp.Regexp{
		ccIdlePatterns,
		codIdlePatterns,
		gmiIdlePatterns,
		kimiIdlePatterns,
		museIdlePatterns,
		cursorIdlePatterns,
		windsurfIdlePatterns,
		aiderIdlePatterns,
	}

	for i, ps := range patternSlices {
		for j, p := range ps {
			if p == nil {
				t.Errorf("Pattern slice %d, element %d is nil", i, j)
			}
		}
	}
}

func TestIsReadyOutput(t *testing.T) {
	tests := []struct {
		name      string
		agentType AgentType
		output    string
		want      bool
	}{
		// Claude indicators
		{"claude_prompt_lowercase", AgentTypeClaudeCode, "claude>", true},
		{"claude_prompt_spaced", AgentTypeClaudeCode, "claude > ", true},
		{"claude_code_version", AgentTypeClaudeCode, "Claude Code v1.2.3", true},
		{"claude_welcome", AgentTypeClaudeCode, "Welcome back!", true},
		{"claude_bypass_permissions", AgentTypeClaudeCode, "Bypass permissions: enabled", true},
		{"claude_try_example", AgentTypeClaudeCode, "Try \"help me with X\"", true},
		{"claude_composer", AgentTypeClaudeCode, "❯ ", true},

		// Codex indicators
		{"codex_prompt", AgentTypeCodex, "codex>", true},
		{"codex_context_left", AgentTypeCodex, "42% context left · ? for shortcuts", true},
		{"codex_chevron_prompt", AgentTypeCodex, "› Write tests for @filename", true},
		{"codex_ready", AgentTypeCodex, "Ready for input", true},

		// Gemini indicators
		{"gemini_prompt", AgentTypeGemini, "gemini>", true},
		{"gemini_help", AgentTypeGemini, "How can I help you today?", true},

		// Muse indicators: the last line is the status bar, not a prompt.
		{"muse_idle_screen", AgentTypeMuse, museIdleScreen, true},
		{"muse_status_bar_only", AgentTypeMuse, "◆ echo: done\n\n❯ draft\n" + museCaptureRule + "\n  muse-wonder-1.0 · /path/to/repo · YOLO", true},
		{"muse_command_echo_only", AgentTypeMuse, "% muse --yolo --model 'muse-wonder-1.0'", false},

		// Simple prompts
		{"aider_prompt", AgentTypeAider, "> ", true},
		{"ollama_repl", AgentTypeOllama, ">>> ", true},

		// "ready" must match as a word: as a bare substring it also hit
		// "already".
		{"ready_word", AgentTypeKimi, "agent ready for input\n", true},
		{"already_up_to_date", AgentTypeKimi, "Already up to date.\n", false},

		// Pi reads only its editor box: a draft or a prompt-suggester ghost
		// in it still takes input, while a live turn, a screen with no
		// editor and another agent's screen do not.
		{"pi_draft", AgentTypePi, piDraftScreen, true},
		{"pi_ghost_suggestion", AgentTypePi, piGhostScreen, true},
		{"pi_working", AgentTypePi, piWorkingNarrowScreen, false},
		{"pi_no_editor", AgentTypePi, "/repo (main)\n0.0%/1.0M (auto)  model", false},
		{"pi_ready_word_without_editor", AgentTypePi, "🧠 ws · ready ○ 🐴 ponytail: ⚡ FULL", false},
		{"pi_codex_screen", AgentTypePi, "› Ask Codex to do anything", false},

		// opencode reads only its prompt box; the logo's "▀" runs are not it.
		// A draft in the box still takes input, while a live turn does not.
		{"opencode_logo_only", AgentTypeOpencode, strings.SplitN(opencodeReadyNarrowScreen, "\n\n\n", 2)[0], false},
		{"opencode_draft", AgentTypeOpencode, strings.Replace(opencodeReadyNarrowScreen, `Ask anything… "Fix broken tests"`, "fix the flaky test", 1), true},
		{"opencode_working", AgentTypeOpencode, strings.Replace(opencodeReadyNarrowScreen, "  /…/bd-1zadt/ws:master", "   ⬝⬝⬝■■■■■ esc interrupt", 1), false},
		{"opencode_ansi_colored", AgentTypeOpencode, "\x1b[38;5;240m  ╹" + strings.Repeat("▀", 20) + "\x1b[0m\n  shift+tab agents", true},
		{"opencode_codex_screen", AgentTypeOpencode, "› Ask Codex to do anything", false},

		// Not ready states
		{"loading", AgentTypeClaudeCode, "Loading...", false},
		{"empty", AgentTypeClaudeCode, "", false},
		{"garbage", AgentTypeClaudeCode, "xyzabc123", false},
		{"partial_prompt", AgentTypeClaudeCode, "claud", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsReadyOutput(tc.output, tc.agentType); got != tc.want {
				t.Errorf("IsReadyOutput(%q, %s) = %v, want %v", tc.output, tc.agentType, got, tc.want)
			}
		})
	}
}

// Live captures of freshly launched agents, trimmed the way tmux.Client
// returns them. No prompt was sent to any of them.
var (
	// opencode 2.0.28 in a 60x20 and a 100x30 tmux pane, run with its HOME and
	// XDG directories in a scratch dir and only a stand-in provider at
	// 127.0.0.1:9, so no real provider was reachable. The home screen centres
	// the prompt box under the logo.
	opencodeReadyNarrowScreen = "▄\n" +
		"           █▀▀█ █▀▀█ █▀▀█ █▀▀▄ █▀▀▀ █▀▀█ █▀▀█ █▀▀█\n" +
		"           █  █ █  █ █▀▀▀ █  █ █    █  █ █  █ █▀▀▀\n" +
		"           ▀▀▀▀ █▀▀▀ ▀▀▀▀ ▀▀▀▀ ▀▀▀▀ ▀▀▀▀ ▀▀▀▀ ▀▀▀▀\n\n\n" +
		"  ┃\n" +
		"  ┃  Ask anything… \"Fix broken tests\"\n" +
		"  ┃\n" +
		"  ┃  Build · Fake Model Fake\n" +
		"  ╹" + strings.Repeat("▀", 55) + "\n" +
		"  /…/bd-1zadt/ws:master  shift+tab agents  ctrl+p commands"
	opencodeReadyWideScreen = "▄\n" +
		"                               █▀▀█ █▀▀█ █▀▀█ █▀▀▄ █▀▀▀ █▀▀█ █▀▀█ █▀▀█\n" +
		"                               █  █ █  █ █▀▀▀ █  █ █    █  █ █  █ █▀▀▀\n" +
		"                               ▀▀▀▀ █▀▀▀ ▀▀▀▀ ▀▀▀▀ ▀▀▀▀ ▀▀▀▀ ▀▀▀▀ ▀▀▀▀\n\n\n" +
		"             ┃\n" +
		"             ┃  Ask anything… \"Fix broken tests\"\n" +
		"             ┃\n" +
		"             ┃  Build · Fake Model Fake\n" +
		"             ╹" + strings.Repeat("▀", 74) + "\n" +
		"             /…/c63d7f…/scratchpad/bd-1zadt/ws:master  shift+tab agents  ctrl+p commands" +
		strings.Repeat("\n", 8) + strings.Repeat(" ", 92) + "2.0.28"

	// `pi --offline --no-session --no-extensions` (Pi 1.3.0) in a 40x20 pane.
	// Pi prints its skill-loading warnings into the transcript above the
	// empty editor.
	piReadyNoExtensionsScreen = "description: Diagnosis loop for hard\n" +
		"bugs and performance regressions. Use\n" +
		"when…\n" +
		"             ^\n\n\n\n" +
		piRule40 + "\n\n" + piRule40 + "\n" +
		"/private/tmp/claude-501/-Volumes-1tb-...\n" +
		"0.0%/1.0M (auto)  google/gemini-4-argon"

	// `pi --offline --no-session` in an 80x24 pane with the operator's
	// extensions, whose status line ends the screen.
	piReadyExtensionsScreen = " Ponytail loaded: full\n\n" +
		strings.Repeat("─", 80) + "\n\n" + strings.Repeat("─", 80) + "\n" +
		"/private/tmp/claude-501/-Volumes-1tb-Projects-ntm/c63d7fe8-3892-44e4-9065-cee...\n" +
		"0.0%/1.0M (auto)                (vercel-ai-gateway) google/gemini-4-argon • high\n" +
		"🧠 ws · ready ○ 🐴 ponytail: ⚡ FULL"

	// `muse --provider echo --yolo` (Muse Code 1.4.11) in a 100x30 pane.
	museReadyScreen = "Muse Code 1.4.11\n\n" +
		"  Skills: 276 loaded · 40 warnings · 76 details hidden (ctrl+o to expand)\n\n" +
		"── Voice input (alt(option)+v to start) " + strings.Repeat("─", 60) + "\n" +
		"❯\n" +
		strings.Repeat("─", 100) + "\n" +
		"  echo · /p/t/c/-/c/s/bd-1zadt/ws · YOLO"

	// The bottom of an idle Codex pane in a 150-column tmux pane, read with
	// capture-pane; the transcript line, cwd and thread title are replaced.
	codexReadyScreen = "• Committed the fix; the retry tests pass.\n\n\n" +
		"› Ask Codex to do anything\n\n" +
		"  GPT-5.6-Sol max · ~/proj · Fix the flaky retry test · Main [default]\n" +
		"  ? for shortcuts" + strings.Repeat(" ", 105) + "⚠ 5 warnings · f2 to view"

	// The bottom of an idle Claude Code 2.1 pane in a 91-column tmux pane,
	// read with capture-pane; the tip text is replaced.
	claudeReadyScreen = "✦ Heads up · Settings now sync across your devices.\n" +
		"  1: Learn more   2: Knew this already   0: Dismiss\n" +
		strings.Repeat(" ", 51) + "✔ Update installed · Restart to update\n" +
		strings.Repeat("─", 91) + "\n" +
		"❯\n" +
		strings.Repeat("─", 91) + "\n" +
		"  ⏵⏵ bypass permissions on · 1 shell · ↓ to manage"
)

// readinessIdleScreens returns, for each launchable agent type, the idle
// screens readiness must accept: live captures where a free stand-in or an
// already-running pane allowed one, and otherwise the screens the repo's
// per-type idle detection is tested on.
func readinessIdleScreens(t *testing.T) map[AgentType]map[string]string {
	t.Helper()
	testdata := func(name string) string {
		raw, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return strings.TrimSpace(stripCaptureHeader(string(raw)))
	}
	return map[AgentType]map[string]string{
		AgentTypeClaudeCode: {
			"live 2.1 pane":                  claudeReadyScreen,
			"testdata claude_idle_completed": testdata("claude_idle_completed.txt"),
		},
		AgentTypeCodex: {
			"live pane":         codexReadyScreen,
			"testdata cod_idle": testdata("cod_idle.txt"),
		},
		AgentTypeGemini: {
			"testdata gmi_idle": testdata("gmi_idle.txt"),
		},
		AgentTypePi: {
			"live fresh, no extensions":      piReadyNoExtensionsScreen,
			"live fresh, extensions":         piReadyExtensionsScreen,
			"captured fresh":                 piIdleFreshScreen,
			"captured after a turn":          piIdleNarrowScreen,
			"captured after a turn, wide":    piIdleWideScreen,
			"captured, extension status cut": piIdleExtensionStatusScreen,
		},
		AgentTypeKimi: {
			"parser fixture": "Kimi Code CLI\nDone.\nkimi> ",
		},
		AgentTypeMuse: {
			"live fresh":    museReadyScreen,
			"captured tip":  museLiveTipScreen,
			"captured idle": museIdleScreen,
		},
		// Antigravity's idle detection is Gemini's (gmiIdlePatterns), and no
		// agy screen has been captured.
		AgentTypeAntigravity: {
			"gemini testdata gmi_idle": testdata("gmi_idle.txt"),
		},
		AgentTypeCursor: {
			"parser fixture": "Done editing.\ncursor> ",
		},
		AgentTypeWindsurf: {
			"parser fixture": "Completed task.\nwindsurf> ",
		},
		AgentTypeAider: {
			"parser fixture": "Changes applied.\naider> ",
		},
		AgentTypeOpencode: {
			"live fresh, 60 columns":       opencodeReadyNarrowScreen,
			"live fresh, 100 columns":      opencodeReadyWideScreen,
			"testdata opencode_idle":       testdata("opencode_idle.txt"),
			"source-derived hint composer": "╭──────╮\n│ Ask anything... \"fix the failing test\" │\n╰──────╯\n  /commands  @context",
		},
		AgentTypeOmp: {
			"testdata omp_nerd_idle_fresh":   testdata("omp_nerd_idle_fresh.txt"),
			"testdata omp_nerd_idle_done":    testdata("omp_nerd_idle_done.txt"),
			"testdata omp_unicode_idle_done": testdata("omp_unicode_idle_done.txt"),
			"testdata omp_ascii_idle_done":   testdata("omp_ascii_idle_done.txt"),
		},
		// Grok Build's composer and status line, as robot spawn's readiness
		// test draws them from live grok 1.0.5 captures (GH#251 phase 2).
		AgentTypeGrok: {
			"spawn fixture": "╭──────╮\n│ ❯                 │\n╰──────╯\n grok-4 · always-approve",
		},
		AgentTypeOllama: {
			"parser fixture": "Done.\n> ",
		},
	}
}

// readinessNoIdleScreen names the launchable agent types with no idle screen
// in the repo or from a free stand-in, and why.
var readinessNoIdleScreen = map[AgentType]string{}

// TestIsReadyOutput_EveryAgentTypeIdleScreen is the bd-1zadt regression:
// restore, robot spawn --spawn-wait and context injection wait on
// IsReadyOutput, so a launchable type whose idle screen it did not recognise
// waited out the whole deadline and was reported as not ready.
func TestIsReadyOutput_EveryAgentTypeIdleScreen(t *testing.T) {
	screens := readinessIdleScreens(t)
	for _, agentType := range AgentTypes() {
		cases, ok := screens[agentType]
		if !ok {
			if reason, known := readinessNoIdleScreen[agentType]; known {
				t.Logf("%s: no idle screen: %s", agentType, reason)
				continue
			}
			t.Errorf("%s: no idle screen to check readiness against", agentType)
			continue
		}
		for name, screen := range cases {
			t.Run(string(agentType)+"/"+name, func(t *testing.T) {
				if !IsReadyOutput(screen, agentType) {
					t.Errorf("IsReadyOutput(%s idle screen) = false, want true:\n%s", agentType, screen)
				}
			})
		}
	}
}

// TestIsReadyOutput_ShellOutputNeverReady keeps what a pane shows before its
// agent's TUI is up, or after the agent exited, from counting as ready for
// any agent type.
func TestIsReadyOutput_ShellOutputNeverReady(t *testing.T) {
	notReady := map[string]string{
		"git pull":      "$ git pull\nAlready up to date.\n",
		"bare $ prompt": "$ ",
		"bare % prompt": "% ",
		"launcher":      "Loading...",
		"empty":         "",
	}
	for _, agentType := range AgentTypes() {
		for name, screen := range notReady {
			t.Run(string(agentType)+"/"+name, func(t *testing.T) {
				if IsReadyOutput(screen, agentType) {
					t.Errorf("IsReadyOutput(%q) for %s = true, want false", screen, agentType)
				}
			})
		}
	}
}

// TestOpencodeRecognizers_GH333Captures reads the opencode 1.18.32 screens
// captured for GH #333 (testdata/opencode_*.txt) with the box reader, which
// was measured on opencode 2.0.28 (testdata/oc_*.txt), and through the
// parser. Once a session exists, a wide pane draws a sidebar to the right of
// the prompt box on the box's own rows, which must not read as a draft.
func TestOpencodeRecognizers_GH333Captures(t *testing.T) {
	t.Parallel()

	working := loadTestData(t, "opencode_working.txt")
	const progressFooter = "⬝⬝⬝⬝■■■■  esc interrupt"
	if !strings.Contains(working, progressFooter) {
		t.Fatalf("opencode_working.txt does not contain %q", progressFooter)
	}
	// The same session once the turn ended: the footer no longer starts with
	// the progress bar, and the sidebar's cwd stays on a box row.
	idleSession := strings.Replace(working, progressFooter, "                       ", 1)

	tests := []struct {
		name        string
		output      string
		wantWorking bool
		wantIdle    bool
		wantPrompt  bool
	}{
		{name: "home screen", output: loadTestData(t, "opencode_idle.txt"), wantIdle: true, wantPrompt: true},
		{name: "draft", output: loadTestData(t, "opencode_draft.txt"), wantPrompt: true},
		{name: "collapsed paste", output: loadTestData(t, "opencode_paste_token.txt"), wantPrompt: true},
		{name: "working", output: working, wantWorking: true},
		{name: "idle session with a sidebar", output: idleSession, wantIdle: true, wantPrompt: true},
	}
	p := NewParser()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := OpencodeActivelyWorking(tt.output, 0); got != tt.wantWorking {
				t.Errorf("OpencodeActivelyWorking() = %v, want %v", got, tt.wantWorking)
			}
			if got := OpencodeIdlePromptShowing(tt.output); got != tt.wantIdle {
				t.Errorf("OpencodeIdlePromptShowing() = %v, want %v", got, tt.wantIdle)
			}
			if got := OpencodePromptShowing(tt.output); got != tt.wantPrompt {
				t.Errorf("OpencodePromptShowing() = %v, want %v", got, tt.wantPrompt)
			}
			state, err := p.ParseWithHint(tt.output, AgentTypeOpencode)
			if err != nil {
				t.Fatalf("ParseWithHint: %v", err)
			}
			if state.IsWorking != tt.wantWorking || state.IsIdle != tt.wantIdle {
				t.Errorf("ParseWithHint: IsWorking = %v, IsIdle = %v, want %v, %v", state.IsWorking, state.IsIdle, tt.wantWorking, tt.wantIdle)
			}
			if state.IsRateLimited || state.IsInError {
				t.Errorf("ParseWithHint: IsRateLimited = %v, IsInError = %v, want false, false", state.IsRateLimited, state.IsInError)
			}
		})
	}
}

// TestOpencodeParser_NoPromptBoxFallsBackToSubstrings pins the split between
// the box reader and the substring patterns: with the prompt box drawn, a
// rate-limit or error line quoted above it changes nothing, and without the
// box the substring patterns decide.
func TestOpencodeParser_NoPromptBoxFallsBackToSubstrings(t *testing.T) {
	t.Parallel()
	p := NewParser()
	idle := loadTestData(t, "oc_idle.txt")

	quoted, err := p.ParseWithHint("error: rate limit exceeded, retry later\n"+idle, AgentTypeOpencode)
	if err != nil {
		t.Fatal(err)
	}
	if !quoted.IsIdle || quoted.IsRateLimited || quoted.IsInError {
		t.Errorf("transcript text above the box: IsIdle = %v, IsRateLimited = %v, IsInError = %v, want true, false, false",
			quoted.IsIdle, quoted.IsRateLimited, quoted.IsInError)
	}

	limited, err := p.ParseWithHint("rate limit exceeded, retry later\n", AgentTypeOpencode)
	if err != nil {
		t.Fatal(err)
	}
	if !limited.IsRateLimited || len(limited.LimitIndicators) == 0 {
		t.Errorf("no box: IsRateLimited = %v, LimitIndicators = %v, want a rate limit", limited.IsRateLimited, limited.LimitIndicators)
	}
}

// Helper functions for creating pointers
func floatPtr(v float64) *float64 {
	return &v
}

func intPtr(v int64) *int64 {
	return &v
}
