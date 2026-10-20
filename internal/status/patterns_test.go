package status

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/agent"
)

func TestStripANSI(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "no ansi",
			input:    "hello world",
			expected: "hello world",
		},
		{
			name:     "color codes",
			input:    "\x1b[32mgreen\x1b[0m text",
			expected: "green text",
		},
		{
			name:     "multiple codes",
			input:    "\x1b[1m\x1b[34mbold blue\x1b[0m",
			expected: "bold blue",
		},
		{
			name:     "cursor movement",
			input:    "\x1b[2Jclear screen",
			expected: "clear screen",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := StripANSI(tt.input)
			if result != tt.expected {
				t.Errorf("StripANSI(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestIsPromptLine(t *testing.T) {
	tests := []struct {
		name      string
		line      string
		agentType string
		expected  bool
	}{
		// Claude prompts
		{name: "claude prompt lowercase", line: "claude>", agentType: "cc", expected: true},
		{name: "claude long-form alias", line: "claude>", agentType: "claude", expected: true},
		{name: "claude prompt with space", line: "claude> ", agentType: "cc", expected: true},
		{name: "Claude prompt uppercase", line: "Claude>", agentType: "cc", expected: true},

		// Codex prompts
		{name: "codex prompt", line: "codex>", agentType: "cod", expected: true},
		{name: "codex long-form alias", line: "codex>", agentType: " CodEx ", expected: true},
		{name: "codex chevron prompt", line: "› Write tests for @filename", agentType: "cod", expected: true},
		// Shell prompts should NOT match for known agent types - a shell $ in cod/cc/gmi means agent exited
		{name: "shell prompt for codex means exited", line: "user@host:~$", agentType: "cod", expected: false},
		{name: "shell prompt for codex alias means exited", line: "user@host:~$", agentType: "codex", expected: false},

		// Gemini prompts
		{name: "gemini prompt", line: "gemini>", agentType: "gmi", expected: true},
		{name: "gemini long-form alias", line: "gemini>", agentType: "gemini", expected: true},
		{name: "Gemini prompt", line: "Gemini>", agentType: "gmi", expected: true},

		// User shell prompts
		{name: "dollar prompt", line: "user@host:~$ ", agentType: "user", expected: true},
		{name: "percent prompt", line: "user@host %", agentType: "user", expected: true},
		{name: "starship prompt", line: "~/project ❯", agentType: "user", expected: true},

		// Generic prompts
		{name: "generic > prompt", line: ">", agentType: "", expected: true},
		{name: "generic > prompt with space", line: "> ", agentType: "", expected: true},

		// Non-prompts
		{name: "regular text", line: "hello world", agentType: "cc", expected: false},
		{name: "empty string", line: "", agentType: "cc", expected: false},
		{name: "whitespace only", line: "   ", agentType: "cc", expected: false},

		// With ANSI codes
		{name: "prompt with ansi", line: "\x1b[32mclaude>\x1b[0m", agentType: "cc", expected: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsPromptLine(tt.line, tt.agentType)
			if result != tt.expected {
				t.Errorf("IsPromptLine(%q, %q) = %v, want %v", tt.line, tt.agentType, result, tt.expected)
			}
		})
	}
}

func TestDetectIdleFromOutput(t *testing.T) {
	tests := []struct {
		name      string
		output    string
		agentType string
		expected  bool
	}{
		{
			name:      "claude idle at prompt",
			output:    "Some previous output\nMore text\nclaude>",
			agentType: "cc",
			expected:  true,
		},
		{
			name:      "claude working",
			output:    "Processing request...\nGenerating code...\n",
			agentType: "cc",
			expected:  false,
		},
		{
			name:      "claude prompt with trailing newlines",
			output:    "Output\nclaude>\n\n",
			agentType: "cc",
			expected:  true,
		},
		{
			name:      "codex at shell prompt means agent exited not idle",
			output:    "Command completed\nuser@host:~$",
			agentType: "cod",
			expected:  false, // shell prompt in cod pane means agent exited, not idle at codex> prompt
		},
		{
			name:      "codex alias at shell prompt still means exited not idle",
			output:    "Command completed\nuser@host:~$",
			agentType: "codex",
			expected:  false,
		},
		{
			name:      "codex at codex prompt",
			output:    "Command completed\ncodex>",
			agentType: "cod",
			expected:  true, // actual codex prompt means idle
		},
		{
			name:      "codex alias at codex prompt",
			output:    "Command completed\ncodex>",
			agentType: " CodEx ",
			expected:  true,
		},
		{
			name:      "codex at chevron prompt",
			output:    "Command completed\n› Write tests for @filename",
			agentType: "cod",
			expected:  true, // codex chevron prompt means idle
		},
		{
			name:      "gemini idle",
			output:    "Response complete.\ngemini>",
			agentType: "gmi",
			expected:  true,
		},
		{
			name:      "empty output",
			output:    "",
			agentType: "cc",
			expected:  false,
		},
		{
			name:      "only whitespace",
			output:    "\n\n   \n",
			agentType: "cc",
			expected:  false,
		},
		{
			name:      "output with ansi codes",
			output:    "\x1b[32mSuccess!\x1b[0m\n\x1b[34mclaude>\x1b[0m",
			agentType: "cc",
			expected:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := DetectIdleFromOutput(tt.output, tt.agentType)
			if result != tt.expected {
				t.Errorf("DetectIdleFromOutput(%q, %q) = %v, want %v",
					tt.output, tt.agentType, result, tt.expected)
			}
		})
	}
}

func TestGetLastNonEmptyLine(t *testing.T) {
	tests := []struct {
		name     string
		output   string
		expected string
	}{
		{
			name:     "simple output",
			output:   "line1\nline2\nline3",
			expected: "line3",
		},
		{
			name:     "trailing newlines",
			output:   "line1\nline2\n\n\n",
			expected: "line2",
		},
		{
			name:     "with ansi",
			output:   "\x1b[32mcolored\x1b[0m\n",
			expected: "colored",
		},
		{
			name:     "empty",
			output:   "",
			expected: "",
		},
		{
			name:     "only whitespace",
			output:   "   \n\t\n  ",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GetLastNonEmptyLine(tt.output)
			if result != tt.expected {
				t.Errorf("GetLastNonEmptyLine(%q) = %q, want %q",
					tt.output, result, tt.expected)
			}
		})
	}
}

func TestIsPromptLine_LiteralMatch(t *testing.T) {
	// Test that literal matching works (for patterns that use Literal instead of Regex)
	// First add a literal pattern for testing
	originalLen := len(promptPatterns)

	// Add a test pattern with Literal
	promptPatterns = append(promptPatterns, PromptPattern{
		AgentType:   "test",
		Literal:     "test_prompt$",
		Description: "test literal prompt",
	})

	defer func() {
		// Restore original patterns
		promptPatterns = promptPatterns[:originalLen]
	}()

	// Test literal matching
	if !IsPromptLine("command test_prompt$", "test") {
		t.Error("should match literal prompt suffix")
	}
}

func TestIsPromptLine_AgentTypeFiltering(t *testing.T) {
	// Test that patterns are filtered by agent type
	// Note: Generic patterns (empty AgentType) match ALL agent types as fallback
	tests := []struct {
		line      string
		agentType string
		expected  bool
	}{
		// Cursor patterns match cursor agent type
		{"cursor>", "cursor", true},
		// Generic pattern ">$" is a fallback that matches any agent type
		{"cursor>", "cc", true}, // Falls through to generic ">$" pattern

		// Windsurf patterns match windsurf
		{"windsurf>", "windsurf", true},
		// Generic fallback pattern matches
		{"windsurf>", "cod", true}, // Falls through to generic ">$" pattern

		// Aider patterns
		{"aider>", "aider", true},
		// Generic fallback pattern matches
		{"aider>", "gmi", true}, // Falls through to generic ">$" pattern

		// But non-prompt lines don't match
		{"just some text", "cursor", false},
		{"running command...", "windsurf", false},
	}

	for _, tt := range tests {
		t.Run(tt.line+"_"+tt.agentType, func(t *testing.T) {
			result := IsPromptLine(tt.line, tt.agentType)
			if result != tt.expected {
				t.Errorf("IsPromptLine(%q, %q) = %v, want %v",
					tt.line, tt.agentType, result, tt.expected)
			}
		})
	}
}

func TestDetectIdleFromOutput_MultipleLines(t *testing.T) {
	// DetectIdleFromOutput scans up to maxIdleScanLines (12) trailing
	// non-empty lines for a prompt, then rejects the verdict if an active
	// spinner sits below the matched prompt.
	tests := []struct {
		name      string
		output    string
		agentType string
		expected  bool
	}{
		{
			// Prompt in second-to-last non-empty line
			name:      "prompt in second line from end",
			output:    "output\nclaude>\n\n",
			agentType: "cc",
			expected:  true,
		},
		{
			// Prompt within the scan window is still detected
			name:      "prompt in third line from end",
			output:    "claude>\nfollowup\nmore",
			agentType: "cc",
			expected:  true,
		},
		{
			// Prompt a handful of lines back (the old 3-line window missed
			// this; the wider window catches it).
			name:      "prompt 5 lines from end within window",
			output:    "claude>\na\nb\nc\nd",
			agentType: "cc",
			expected:  true,
		},
		{
			// REAL Claude layout (from internal/cli/outputs/): the "❯ " input
			// box is pinned to the BOTTOM, with the status footer below it and
			// the (now finished) turn's content above. No active spinner is the
			// most-recent dynamic marker, so the pane is idle and dispatchable.
			name: "cc finished turn with bottom-pinned box is idle",
			output: "● All changes applied; tests pass.\n" +
				"\n" +
				"✻ Cooked for 2m 10s\n" +
				"───────────\n" +
				"❯ \n" +
				"───────────\n" +
				"  ⏵⏵ bypass permissions on (shift+tab to cycle)\n",
			agentType: "cc",
			expected:  true,
		},
		{
			// CRITICAL false-idle guard, REAL layout: the active spinner renders
			// just ABOVE the bottom-pinned input box while a turn is in flight.
			// The box is always drawn, so its presence is NOT idleness — the
			// most-recent dynamic marker is the spinner. MUST NOT report idle.
			name: "cc working with spinner above bottom box",
			output: "● Running the suite.\n" +
				"✻ Scurrying… (ctrl+c to interrupt · 12s · thinking)\n" +
				"───────────\n" +
				"❯ \n" +
				"───────────\n" +
				"  ⏵⏵ bypass permissions on\n",
			agentType: "cc",
			expected:  false,
		},
		{
			// "new task?" footer parked below the box after a turn ends, with no
			// active spinner — idle and dispatchable.
			name: "cc new task footer is idle",
			output: "● Done.\n" +
				"✻ Baked for 5m 0s\n" +
				"───────────\n" +
				"❯ \n" +
				"───────────\n" +
				"new task? /clear to save 12.3k tokens\n",
			agentType: "cc",
			expected:  true,
		},
		{
			// Prompt beyond the scan window must NOT be detected — guard against
			// false positives from very-old prompt text deep in scrollback.
			name: "prompt beyond scan window",
			output: "claude>\n" +
				"l1\nl2\nl3\nl4\nl5\nl6\nl7\nl8\nl9\nl10\n" +
				"l11\nl12\nl13",
			agentType: "cc",
			expected:  false,
		},
		{
			name:      "prompt as last line after work output",
			output:    "exec /bin/bash --norc --noprofile\necho BASH_READY\nPS1='$ '; echo IDLE_MARKER\nIDLE_MARKER\n$",
			agentType: "user",
			expected:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := DetectIdleFromOutput(tt.output, tt.agentType)
			if result != tt.expected {
				t.Errorf("DetectIdleFromOutput = %v, want %v", result, tt.expected)
			}
		})
	}
}

// A live capture from a muse 1.4.11 echo pane in a 160x45 tmux pane,
// whitespace-trimmed the way tmux.Client returns captures (only the cwd is
// shortened), taken ten seconds after the "hi" turn ended: the idle composer
// shows a dim feature tip.
var (
	museLiveTipComposer = "❯ Type @ to search and insert workspace file paths"
	museLiveVoiceBar    = "── Voice input (alt(option)+v to start) " + strings.Repeat("─", 120)
	museLiveTipCapture  = "Muse Code 1.4.11\n\n" +
		"  Skills: 276 loaded · 40 warnings · 76 details hidden (ctrl+o to expand)\n\n" +
		"❯ hi\n\n" +
		"◆ echo: hi\n\n" +
		museLiveVoiceBar + "\n" +
		museLiveTipComposer + "\n" +
		strings.Repeat("─", 160) + "\n" +
		"  echo · /private/tmp/claude-501/scratchpad/musews52 · YOLO"
	museLiveTipWorkingCapture = strings.Replace(museLiveTipCapture,
		museLiveVoiceBar, "◇ Thinking (2s · esc to interrupt)\n\n"+museLiveVoiceBar, 1)
	museLiveDraftCapture = strings.Replace(museLiveTipCapture, museLiveTipComposer, "❯ draft not sent", 1)
)

// DetectIdleFromOutput feeds robot health, the robot idle check and the swarm
// prompt injector. Before Muse had its own branch, an idle Muse pane read as
// busy here as soon as its composer showed a tip, so those callers never saw
// it idle.
func TestDetectIdleFromOutput_Muse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{"live capture with a tip in the composer", museLiveTipCapture, true},
		{"empty composer", museIdleCapture, true},
		{"working line above a tip composer", museLiveTipWorkingCapture, false},
		{"working line above an empty composer", museWorkingCapture, false},
		{"working line above a draft", museWorkingWithDraftCapture, false},
		// Unsent text is not an idle prompt, even though the composer is
		// drawn; a draft ending in ">" must not reach the generic scan either.
		{"draft in the composer", museLiveDraftCapture, false},
		{"multi-line draft", museDraftCapture, false},
		{"draft ending in a chevron", strings.Replace(museLiveTipCapture, museLiveTipComposer, "❯ compare a>", 1), false},
		{"trust prompt", museTrustPromptCapture, false},
	}
	for _, tt := range tests {
		for _, agentType := range []string{"muse", "muse-code", "meta-muse"} {
			t.Run(tt.name+"/"+agentType, func(t *testing.T) {
				if got := DetectIdleFromOutput(tt.output, agentType); got != tt.want {
					t.Errorf("DetectIdleFromOutput(%s) = %v, want %v", agentType, got, tt.want)
				}
			})
		}
	}
}

// TestMuseIdleAgreesWithUnifiedDetector pins the three Muse idle readers to
// one rule: DetectIdleFromOutput (robot health, swarm injector), the unified
// detector's state plus dispatch confidence, and the agent parser. A draft
// leaves the pane idle in the unified state but never dispatchable.
func TestMuseIdleAgreesWithUnifiedDetector(t *testing.T) {
	t.Parallel()

	d := NewDetector()
	observedAt := time.Date(2026, 10, 19, 21, 0, 0, 0, time.UTC)
	parser := agent.NewParser()

	tests := []struct {
		name      string
		output    string
		wantState AgentState
		wantIdle  bool
	}{
		{"live tip", museLiveTipCapture, StateIdle, true},
		{"captured tip", museIdleTipCapture, StateIdle, true},
		{"empty composer", museIdleCapture, StateIdle, true},
		{"live draft", museLiveDraftCapture, StateIdle, false},
		{"multi-line draft", museDraftCapture, StateIdle, false},
		{"working above a tip", museLiveTipWorkingCapture, StateWorking, false},
		{"working above an empty composer", museWorkingCapture, StateWorking, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, activity := range []time.Time{observedAt, observedAt.Add(-time.Hour)} {
				state, _ := d.determineStateAt(tt.output, "muse", activity, observedAt)
				if state != tt.wantState {
					t.Errorf("determineStateAt() = %v, want %v", state, tt.wantState)
				}
				confidence := observationConfidence(AgentStatus{AgentType: "muse", State: state}, tt.output)
				dispatchable := state == StateIdle && ObservationConfidenceIsActionable(confidence)
				if dispatchable != tt.wantIdle {
					t.Errorf("unified dispatchable = %v (state %v, confidence %.2f), want %v", dispatchable, state, confidence, tt.wantIdle)
				}
			}
			if got := DetectIdleFromOutput(tt.output, "muse"); got != tt.wantIdle {
				t.Errorf("DetectIdleFromOutput() = %v, want %v", got, tt.wantIdle)
			}
			parsed, err := parser.ParseWithHint(tt.output, agent.AgentTypeMuse)
			if err != nil {
				t.Fatalf("ParseWithHint error: %v", err)
			}
			if parsed.IsIdle != tt.wantIdle {
				t.Errorf("parser IsIdle = %v, want %v", parsed.IsIdle, tt.wantIdle)
			}
		})
	}
}

// Pi 1.3.0 screen bottoms captured from `pi --offline` panes in tmux,
// trimmed the way tmux.Client returns captures. Pi was never sent a prompt;
// the working screens carry the indicator Pi's own editor draws in its top
// rule, and piWorkingAboveEditorCapture is a live capture of a busy Pi whose
// extension-installed editor shows the indicator above the box.
var (
	piRule40 = strings.Repeat("─", 40)

	// After a turn in 40 columns: Pi cut the usage line to "0.3%/...".
	piIdleNarrowCapture = " tests pass. I'm running nothing else;\n" +
		" tell me what to change next...\n\n" +
		piRule40 + "\n\n" + piRule40 + "\n" +
		"/private/tmp/claude-501/-Volumes-1tb-...\n" +
		"↑2.6k ↓120 R1.2k CH46.2% $0.004 0.3%/..."

	// 32 columns with extensions loaded: their status line is cut short.
	piIdleExtensionStatusCapture = " Ponytail loaded: full\n\n" +
		strings.Repeat("─", 32) + "\n\n" + strings.Repeat("─", 32) + "\n" +
		"/private/tmp/claude-501/-Volu...\n" +
		"0.0%/1.0M (auto)  google/gemini-\n" +
		"🧠 work · ready ○ 🐴 ponytail..."

	piDraftCapture = strings.Replace(piIdleNarrowCapture, piRule40+"\n\n"+piRule40,
		piRule40+"\nrename main.go to app.go and update\n"+piRule40, 1)

	piWorkingNarrowCapture = strings.Replace(piIdleNarrowCapture, piRule40+"\n\n",
		"── ⠋ Working "+strings.Repeat("─", 27)+"\n\n", 1)

	piWorkingAboveEditorCapture = " Elapsed 1m 2s\n ⠧ Working\n" +
		strings.Repeat("─", 140) + "\n" + strings.Repeat("─", 140) + "\n" +
		"/Volumes/1tb/Projects/ManchesterSquare (main)\n" +
		"↑166k ↓20k R2.4M CH99.7% $0.181 8.8%/1.0M (auto)            (vercel-ai-gateway) deepseek/d\n" +
		"🧠 114-manchester · ready ● 🐴 ponytail: ⚡ FULL"
)

// TestPiIdleAgreesWithUnifiedDetector pins the Pi idle readers to one rule
// (bd-7bzre): DetectIdleFromOutput, the unified detector's state plus
// dispatch confidence, and the agent parser. Pi draws no prompt glyph, so
// before Pi had its own branch an idle Pi read idle only when an extension
// status happened to say "ready", and never as an idle prompt.
func TestPiIdleAgreesWithUnifiedDetector(t *testing.T) {
	t.Parallel()

	d := NewDetector()
	observedAt := time.Date(2026, 10, 19, 21, 0, 0, 0, time.UTC)
	parser := agent.NewParser()

	tests := []struct {
		name      string
		output    string
		wantState AgentState
		wantIdle  bool
	}{
		{"usage line cut short", piIdleNarrowCapture, StateIdle, true},
		{"extension status cut short", piIdleExtensionStatusCapture, StateIdle, true},
		{"draft", piDraftCapture, StateIdle, false},
		{"indicator in the top rule", piWorkingNarrowCapture, StateWorking, false},
		{"indicator above the editor", piWorkingAboveEditorCapture, StateWorking, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, activity := range []time.Time{observedAt, observedAt.Add(-time.Hour)} {
				state, _ := d.determineStateAt(tt.output, "pi", activity, observedAt)
				if state != tt.wantState {
					t.Errorf("determineStateAt() = %v, want %v", state, tt.wantState)
				}
				confidence := observationConfidence(AgentStatus{AgentType: "pi", State: state}, tt.output)
				dispatchable := state == StateIdle && ObservationConfidenceIsActionable(confidence)
				if dispatchable != tt.wantIdle {
					t.Errorf("unified dispatchable = %v (state %v, confidence %.2f), want %v", dispatchable, state, confidence, tt.wantIdle)
				}
			}
			for _, agentType := range []string{"pi", "pi-coding-agent"} {
				if got := DetectIdleFromOutput(tt.output, agentType); got != tt.wantIdle {
					t.Errorf("DetectIdleFromOutput(%s) = %v, want %v", agentType, got, tt.wantIdle)
				}
			}
			parsed, err := parser.ParseWithHint(tt.output, agent.AgentTypePi)
			if err != nil {
				t.Fatalf("ParseWithHint error: %v", err)
			}
			if parsed.IsIdle != tt.wantIdle {
				t.Errorf("parser IsIdle = %v, want %v", parsed.IsIdle, tt.wantIdle)
			}
			if parsed.IsWorking != (tt.wantState == StateWorking) {
				t.Errorf("parser IsWorking = %v, want %v", parsed.IsWorking, tt.wantState == StateWorking)
			}
		})
	}
}

// TestOpencodeIdleAgreesWithUnifiedDetector is the bd-qbs61 regression:
// DetectIdleFromOutput read opencode with the generic prompt patterns, which
// never match its prompt box, so an idle opencode pane never read idle and
// was never dispatched to. The captures are opencode 2.0.28 screens from the
// agent package's testdata, trimmed the way tmux.Client returns captures.
func TestOpencodeIdleAgreesWithUnifiedDetector(t *testing.T) {
	t.Parallel()

	d := NewDetector()
	observedAt := time.Date(2026, 10, 20, 4, 0, 0, 0, time.UTC)
	parser := agent.NewParser()

	tests := []struct {
		file      string
		wantState AgentState // empty: the velocity heuristics decide
		wantIdle  bool
	}{
		{"oc_idle_fresh.txt", StateIdle, true},
		{"oc_idle_fresh_narrow.txt", StateIdle, true},
		{"oc_idle.txt", StateIdle, true},
		{"oc_idle_narrow.txt", StateIdle, true},
		{"oc_idle_after_tool.txt", StateIdle, true},
		{"oc_idle_interrupted.txt", StateIdle, true},
		{"oc_idle_styled.txt", StateIdle, true},
		{"oc_draft.txt", StateIdle, false},
		{"oc_question.txt", "", false},
		{"oc_working.txt", StateWorking, false},
		{"oc_working_narrow.txt", StateWorking, false},
		{"oc_working_tool.txt", StateWorking, false},
		{"oc_working_tool_narrow.txt", StateWorking, false},
		{"oc_working_esc_again.txt", StateWorking, false},
		{"oc_working_styled.txt", StateWorking, false},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "agent", "testdata", tt.file))
			if err != nil {
				t.Fatalf("read capture: %v", err)
			}
			output := strings.TrimSpace(string(raw))
			for _, activity := range []time.Time{observedAt, observedAt.Add(-time.Hour)} {
				state, _ := d.determineStateAt(output, "oc", activity, observedAt)
				if tt.wantState != "" && state != tt.wantState {
					t.Errorf("determineStateAt() = %v, want %v", state, tt.wantState)
				}
				confidence := observationConfidence(AgentStatus{AgentType: "oc", State: state}, output)
				dispatchable := state == StateIdle && ObservationConfidenceIsActionable(confidence)
				if dispatchable != tt.wantIdle {
					t.Errorf("unified dispatchable = %v (state %v, confidence %.2f), want %v", dispatchable, state, confidence, tt.wantIdle)
				}
			}
			for _, agentType := range []string{"oc", "opencode"} {
				if got := DetectIdleFromOutput(output, agentType); got != tt.wantIdle {
					t.Errorf("DetectIdleFromOutput(%s) = %v, want %v", agentType, got, tt.wantIdle)
				}
			}
			parsed, err := parser.ParseWithHint(output, agent.AgentTypeOpencode)
			if err != nil {
				t.Fatalf("ParseWithHint error: %v", err)
			}
			if parsed.IsIdle != tt.wantIdle {
				t.Errorf("parser IsIdle = %v, want %v", parsed.IsIdle, tt.wantIdle)
			}
			if parsed.IsWorking != (tt.wantState == StateWorking) {
				t.Errorf("parser IsWorking = %v, want %v", parsed.IsWorking, tt.wantState == StateWorking)
			}
		})
	}
}
