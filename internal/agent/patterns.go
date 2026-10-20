package agent

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/Dicklesworthstone/ntm/internal/util"
)

// Claude Code (cc) patterns for state detection.
var (
	// ccRateLimitPatterns indicates the agent hit an API usage limit.
	// We use broad patterns here - false positives (waiting unnecessarily) are
	// acceptable, but false negatives (interrupting a blocked agent) are costly.
	ccRateLimitPatterns = []string{
		"you've hit your limit",
		"you.ve hit your limit",
		"rate limit exceeded",
		"rate limit",
		"too many requests",
		"please wait",
		"try again later",
		"usage limit",
		"request limit",
		"exceeded the limit",
		"exceeded your limit",
		"exceeded limit",
	}

	// ccContextWarnings indicates the agent is running low on context.
	// Claude Code doesn't give explicit percentages, so we rely on warning messages.

	ccContextWarnings = []string{
		"this conversation is getting long",
		"context limit",
		"context is at its limit",
		"running out of context",
		"conversation is getting long",
		"conversation is too long",
		"approaching the limit",
		"approaching its limit",
		"nearing capacity",
		"nearing its capacity",
	}

	// ccWorkingPatterns indicates the agent is actively producing output.
	// CRITICAL: When these patterns match, DO NOT INTERRUPT the agent.
	ccWorkingPatterns = []string{
		"```",         // Code block delimiter (most reliable indicator)
		"writing to ", // File write operation
		"created ",    // File creation
		"modified ",   // File modification
		"deleted ",    // File deletion
		"reading ",    // File read operation
		"searching ",  // Search in progress
		"running ",    // Command execution
		"executing ",  // Command execution
		"installing ", // Package installation
		"compiling",   // Compilation
		"building",    // Build process
		"testing",     // Test execution
		"fetching",    // Network operation
		"downloading", // Download operation
		"uploading",   // Upload operation
	}

	// ccIdlePatterns indicates the agent is waiting for user input.
	// When these match at the end of output, it's safe to restart or send new work.
	// NOTE: "bypass permissions on" was removed — it matches the permanent status bar,
	// causing agents to always appear idle even while actively working.
	ccIdlePatterns = []*regexp.Regexp{
		regexp.MustCompile(`>\s*$`),      // Prompt waiting for input
		regexp.MustCompile(`(?m)^>\s*$`), // Prompt line (empty prompt waiting for input)
		regexp.MustCompile(`Human:\s*$`), // Conversation mode prompt
		regexp.MustCompile(`waiting for input`),
		regexp.MustCompile(`(?m)^.{0,40}\?\s*$`), // Short question prompt (max 40 chars to avoid matching reasoning output)
		// Claude Code TUI patterns (welcome screen)
		regexp.MustCompile(`(?i)claude\s+code\s+v[\d.]+`), // Version banner
		regexp.MustCompile(`(?i)welcome\s+back`),          // Welcome message
		regexp.MustCompile(`╰─>\s*$`),                     // Arrow prompt
		regexp.MustCompile(`(?m)❯[\s\x{00a0}]*$`),         // Unicode heavy right-pointing angle prompt (multiline, NBSP-aware)
	}

	// ccSpinnerActivePatterns detect Claude Code's randomized spinner verbs
	// (e.g. "Bunning… (3s)", "Scurrying… (12s)", "Running…", "· thinking", "· thought for 5s").
	// When these match, the agent is actively working — NOT idle.
	ccSpinnerActivePatterns = []*regexp.Regexp{
		regexp.MustCompile(`\S+…\s+\(`),         // Timing spinners: "Bunning… (3s)"
		regexp.MustCompile(`·\s*thinking`),      // Extended thinking indicator
		regexp.MustCompile(`·\s*thought\s+for`), // Past thinking indicator (still active context)
		regexp.MustCompile(`Running…`),          // Explicit running spinner
	}

	// claudeActiveSpinnerLineRe matches a Claude Code *live* spinner line — the
	// status line Claude renders just above its bottom-pinned input box while a
	// turn is in flight. Derived from the scrubbed real-capture layouts tracked
	// in internal/agent/testdata/:
	//
	//   ✻ Monitoring 17 agents… (ctrl+c to interrupt · … · 15m 52s · …)
	//   ✻ Compacting conversation… (ctrl+c to interrupt · … · 17m 50s · …)
	//   ✻ Whirlpooling… (ctrl+c to interrupt · … · 2m 44s · thinking)
	//   ✢ Implementing Remaining BV Robot Modes… (ctrl+c to interrupt · …)
	//
	// The defining shape is a verb/phrase ending in the ellipsis glyph "…"
	// immediately followed by the timer/interrupt parenthetical "(". This is
	// deliberately distinct from the completion line (which has no "…(" and no
	// open paren) so the two never collide.
	claudeActiveSpinnerLineRe = regexp.MustCompile(`\S+…\s*\(`)

	// claudeInFlightHintRe matches the in-flight footer hint Claude shows only
	// while a turn is running ("ctrl+c to interrupt" / "esc to interrupt"). It is
	// a second, independent signal of active work for spinner frames that render
	// without the "…(" shape.
	claudeInFlightHintRe = regexp.MustCompile(`(?i)(ctrl\+c|esc)\s+to\s+interrupt`)

	// claudeThinkingSpinnerRe matches the extended-thinking spinner ("· thinking")
	// but NOT the past-tense annotation ("thought for 14s") which can appear as a
	// prose tail inside a finished completion summary.
	claudeThinkingSpinnerRe = regexp.MustCompile(`·\s*thinking\b`)

	// claudeCompletionLineRe matches Claude Code's turn-ended completion line.
	// Derived from real captures: glyph-led, whole-line, "<Verb> for <duration>"
	// with NO ellipsis and NO open paren. Observed verbs include Cooked, Baked,
	// Cogitated, Churned, Brewed, Worked, Crunched — Claude randomizes the verb,
	// so the regex keys on the structural "<Capitalized word> for <Nm Ns>" shape
	// behind one of the asterisk-family spinner glyphs rather than an enumerated
	// verb list.
	//
	//   ✻ Cooked for 1m 42s
	//   ✻ Baked for 44m 28s
	//
	// It must NOT match an active spinner ("Whirlpooling… (… · 2m 44s …)") nor a
	// "thought for 14s" prose annotation, hence: the leading glyph + verb, the
	// literal " for ", a duration, and a deliberately strict end-of-line anchor
	// that rejects any trailing "(" / "…" progress decoration.
	//
	// The verb class is the Unicode capitalized-word class `\p{Lu}\p{Ll}+`
	// rather than ASCII `[A-Z][a-z]+`: Claude's whimsical spinner verbs include
	// accented forms ("✻ Sautéed for 36s", "✻ Flambéed for 12s") whose é/ü/etc.
	// fall outside `[a-z]` and would break recognition of the turn-ended line —
	// leaving a stale active spinner higher in the capture unsuppressed and the
	// pane misclassified as still WORKING. `\p{Lu}` (capitalized first letter)
	// preserves the anti-prose constraint; the whole-line and duration anchors
	// are unchanged.
	claudeCompletionLineRe = regexp.MustCompile(`(?m)^\s*[✻✶✳✢✽✦*]\s+\p{Lu}\p{Ll}+\s+for\s+(?:\d+\s*[hms]\s*)+$`)

	// claudeNewTaskFooterRe matches the post-turn "new task?" hint Claude parks at
	// after a turn ends (often paired with a "/clear to save … tokens" line). It
	// is a turn-ended marker, not active work.
	claudeNewTaskFooterRe = regexp.MustCompile(`(?im)^\s*new\s+task\?\s*/clear\b`)

	// claudeTerminalErrorLineRe matches Claude's rendered tool-result failure
	// line. Keep this glyph-anchored so queued compose-box text or spinner labels
	// containing "Error:" cannot terminate an otherwise live turn. Claude uses
	// NBSP padding in real captures, so the post-glyph class is Unicode-aware.
	claudeTerminalErrorLineRe = regexp.MustCompile(`(?i)^\s*⎿[\s\x{00a0}]*error:`)

	// ccErrorPatterns indicates an error condition.
	ccErrorPatterns = []string{
		"error:",
		"failed:",
		"exception:",
		"panic:",
		"fatal:",
		"abort:",
		"permission denied",
		"access denied",
		"connection refused",
		"timed out",
		"timeout error",
	}

	// ccHeaderPattern confirms output is from Claude Code.
	ccHeaderPattern = regexp.MustCompile(`(?i)\b(opus|claude|sonnet|haiku)\b\s*\d*\.?\d*`)
)

// Codex CLI (cod) patterns for state detection.
var (
	// codContextPattern extracts the explicit context percentage.
	// This is the most valuable metric - Codex shows "47% context left".
	// Example: "47% context left · ? for shortcuts"
	codContextPattern = regexp.MustCompile(`(\d+)%\s*context\s*left`)

	// codTokenPattern extracts token usage from response.
	// Example: "Token usage: total=219,582 input=206,150 ..."
	codTokenPattern = regexp.MustCompile(`Token usage:\s*total=(\d[\d,]*)`)

	// codRateLimitPatterns indicates the agent hit usage limits.
	codRateLimitPatterns = []string{
		"you've reached your usage limit",
		"you.ve reached your usage limit",
		"rate limit exceeded",
		"rate limit",
		"quota exceeded",
		"capacity reached",
		"maximum requests",
		"too many requests",
	}

	// codWorkingPatterns indicates active output production.
	codWorkingPatterns = []string{
		"```",       // Code block
		"editing ",  // File edit
		"creating ", // File creation
		"writing ",  // File write
		"reading ",  // File read
		"running ",  // Command execution
		"$ ",        // Shell command output indicator
		"applying ", // Applying changes
		"patching ", // Patch application
		"deleting ", // File deletion
	}

	// Codex keeps its input chevron and context footer rendered while a turn is
	// active, so those idle-looking elements cannot outrank a live spinner. Keep
	// these structural markers separate from codWorkingPatterns: substring-based
	// parser hints are useful for recent output, while these markers are strong
	// enough to override the persistent prompt chrome.
	codexActiveWorkLineRe = regexp.MustCompile(`(?im)^\s*[•·◐◑◒◓⬤]\s*(?:Working|Waiting\s+for\s+background|Thinking)\b`)
	codexInterruptHintRe  = regexp.MustCompile(`(?i)esc\s+to\s+i(?:nterrupt\b|\S*…|\S*\.\.\.)`)

	// codIdlePatterns indicates waiting for input.
	codIdlePatterns = []*regexp.Regexp{
		regexp.MustCompile(`>\s*$`),                // Standard prompt
		regexp.MustCompile(`\?\s*for\s*shortcuts`), // Codex prompt line
		regexp.MustCompile(`codex>\s*$`),           // Codex prompt
		regexp.MustCompile(`(?m)^\s*›(?:\s.*)?$`),  // Codex chevron prompt, empty or with prefilled input
	}

	// codErrorPatterns indicates error conditions.
	codErrorPatterns = []string{
		"error:",
		"failed:",
		"exception:",
		"could not",
		"unable to",
	}

	// codHeaderPattern confirms output is from Codex CLI.
	codHeaderPattern = regexp.MustCompile(`(?i)\b(codex|openai|gpt-\d)\b`)
)

const codexLiveTailLines = 15

// Gemini CLI (gmi) patterns for state detection.
var (
	// gmiMemoryPattern extracts memory usage.
	// Less precise than Codex percentage but still useful.
	// Example: "gemini-3-pro-preview /model | 396.8 MB"
	gmiMemoryPattern = regexp.MustCompile(`/model\s*\|\s*(\d+\.?\d*)\s*MB`)

	// gmiYoloPattern detects YOLO mode status.
	// YOLO mode affects execution behavior (auto-approve commands).
	gmiYoloPattern = regexp.MustCompile(`(?i)YOLO\s*mode:\s*(ON|OFF)`)

	// gmiRateLimitPatterns indicates rate limiting.
	// Gemini is less explicit about limits, so we use broader heuristics.
	gmiRateLimitPatterns = []string{
		"quota exceeded",
		"quota",
		"limit reached",
		"rate limit",
		"try again",
		"capacity",
		"resource exhausted",
	}

	// gmiWorkingPatterns indicates active output.
	gmiWorkingPatterns = []string{
		"```",         // Code block
		"creating ",   // File creation
		"writing ",    // File write
		"executing ",  // Command execution
		"running ",    // Running command
		"generating ", // Content generation
		"analyzing ",  // Analysis
		"processing",  // General processing
		"thinking",    // Chain of thought
	}

	// gmiIdlePatterns indicates waiting for input.
	gmiIdlePatterns = []*regexp.Regexp{
		regexp.MustCompile(`>\s*$`),       // Standard prompt
		regexp.MustCompile(`gemini>\s*$`), // Gemini prompt
	}

	// gmiErrorPatterns indicates error conditions.
	gmiErrorPatterns = []string{
		"error",
		"failed",
		"exception",
		"invalid",
	}

	// gmiShellModePattern detects shell mode.
	// GOTCHA: Shell mode is triggered by "!" prefix in prompts.
	gmiShellModePattern = regexp.MustCompile(`^!\s*`)

	// gmiHeaderPattern confirms output is from Gemini CLI.
	gmiHeaderPattern = regexp.MustCompile(`(?i)(gemini.*preview|gemini-\d|google\s+ai)`)
)

// Antigravity CLI (agy) patterns for state detection.
//
// Antigravity is the Gemini CLI's successor and shares its interactive TUI
// behavior (memory display, YOLO-style auto-approve, prompt rendering), so its
// working / idle / rate-limit / error / metric detection reuses the gmi*
// pattern sets above (see the AgentTypeAntigravity arms in parser.go). The only
// agy-specific signal we need is a header signature that distinguishes an
// Antigravity pane from a legacy Gemini pane during unhinted type detection.
var (
	// agyHeaderPattern confirms output is from the Antigravity CLI.
	agyHeaderPattern = regexp.MustCompile(`(?i)antigravity`)
)

// Pi (pi) patterns. A pane known to run Pi is read with PiActivelyWorking and
// PiIdlePromptShowing, which follow the screen Pi 1.3.0 draws; the substring
// working and idle lists below only score unknown panes and feed
// GetPatternSet.
var (
	piRateLimitPatterns = []string{
		"rate limit",
		"too many requests",
		"quota exceeded",
		"usage limit",
		"try again later",
	}

	piWorkingPatterns = []string{
		"```",
		"tool call",
		"running ",
		"reading ",
		"writing ",
		"editing ",
		"searching ",
		"thinking",
	}

	piIdlePatterns = []*regexp.Regexp{
		regexp.MustCompile(`>\s*$`),
		regexp.MustCompile(`(?m)^>\s*$`),
		regexp.MustCompile(`(?i)pi\s+-\s+ai\s+coding\s+assistant`),
		regexp.MustCompile(`(?i)\bpi\b.*\b(coding\s+assistant|terminal\s+coding\s+harness)\b`),
	}

	piErrorPatterns = []string{
		"error:",
		"failed:",
		"exception:",
		"permission denied",
		"connection refused",
		"timeout",
	}

	piHeaderPattern = regexp.MustCompile(`(?im)(^\s*pi\s+-\s+ai\s+coding\s+assistant\b|\bpi\b.*\b(coding\s+assistant|terminal\s+coding\s+harness)\b|\[pi(?:[-_][[:alnum:]-]+)?\])`)

	// piWorkingLineRe matches one of Pi's status indicator lines
	// (PiWorkingLinePattern).
	piWorkingLineRe = regexp.MustCompile(PiWorkingLinePattern)

	// piBottomRuleRe matches the editor's bottom rule: a plain rule, or one
	// carrying the "↓ 3 more" label when the editor holds more text than it
	// shows.
	piBottomRuleRe = regexp.MustCompile(`^─+(?: ↓ \d+ more ─*)?[ \t]*$`)

	// piPlainRuleRe matches a rule with no label. The editor's top rule is
	// plain unless the status indicator sits in it or text scrolled above.
	piPlainRuleRe = regexp.MustCompile(`^─+[ \t]*$`)
)

// Pi's screen, measured against pi 1.3.0 in tmux in its default fullscreen
// TUI mode. The bottom of every Pi screen is the input editor, a box between
// two rules drawn from column 0, followed by the footer: the cwd line, the
// usage and model line and, when extensions publish any, a line of extension
// statuses. Pi cuts each footer line at the pane edge with "...", so in a
// narrow pane an idle screen ends in "..." ("0.3%/...", "🐴 ponytail...").
//
// While a turn runs Pi shows a status indicator: a braille spinner frame and
// "Working", or "Retrying (1/3) in 5s...", "Compacting context...",
// "Auto-compacting..." or "Summarizing branch...". Pi's own editor draws it in
// its top rule ("── ⠋ Working ─────"). An editor installed by an extension
// that does not embed it, such as pi-prompt-suggester's ghost editor, leaves
// it on a line of its own above the box (" ⠧ Working"). Pi clears the
// indicator when the turn ends, so the top rule is plain again.
const (
	// PiWorkingLinePattern matches a Pi status indicator line: the editor's
	// top rule with the indicator in it ("── ⠋ Working ───", where "↑ 3
	// more" is the scroll label of an idle editor instead), or the
	// indicator on its own line, led by a braille spinner frame.
	PiWorkingLinePattern = `(?m)^(?:──[ ][^\s─↑↓][^\n]*[ ]─+|[ \t]*[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏][ \t]+\S[^\n]*)[ \t]*$`

	// PiEmptyEditorPattern matches Pi's editor with nothing in it: two plain
	// rules with only blank lines between them.
	PiEmptyEditorPattern = `(?m)^─+[ \t]*\n(?:[ \t\x{00a0}]*\n)*─+[ \t]*$`
)

// piFooterScanLines bounds how many non-blank lines up from the bottom
// piEditorBox looks for the editor's bottom rule: the three footer lines, up
// to ten lines of extension widgets below the editor, and an open
// autocomplete list.
const piFooterScanLines = 16

// piEditorMaxLines bounds the lines between the editor's rules. Pi shows at
// most 30% of the pane's rows of editor text, five at least.
const piEditorMaxLines = 40

// piStatusLinesAboveEditor bounds how far above the editor's top rule
// piStatusShowing looks for an indicator on a line of its own. Between the
// indicator and the editor Pi draws a blank line, or up to ten lines of
// extension widgets.
const piStatusLinesAboveEditor = 12

// piLiveTailLines is the window PiActivelyWorking searches when no editor is
// drawn, as while a selector or dialog replaces it.
const piLiveTailLines = 10

// PiActivelyWorking reports whether a Pi pane shows a live turn: the status
// indicator is drawn in the editor's top rule or just above the editor.
// Footer lines are never read, so a footer Pi cut short with "..." in a narrow
// pane is not mistaken for a spinner.
func PiActivelyWorking(output string) bool {
	lines := strings.Split(stripANSICodes(output), "\n")
	top, _, ok := piEditorBox(lines)
	if !ok {
		return piWorkingLineRe.MatchString(util.GetLastNLines(strings.Join(lines, "\n"), piLiveTailLines))
	}
	return piStatusShowing(lines, top)
}

// PiIdlePromptShowing reports whether a Pi pane is waiting for input with an
// empty editor and no live turn. An editor holding a draft returns false, so
// a pane is never offered new work on top of unsent text.
// status.DetectIdleFromOutput and the agent parser's detectIdle both call it.
func PiIdlePromptShowing(output string) bool {
	lines := strings.Split(stripANSICodes(output), "\n")
	top, bottom, ok := piEditorBox(lines)
	if !ok || piStatusShowing(lines, top) || !piPlainRuleRe.MatchString(lines[top]) {
		return false
	}
	for _, line := range lines[top+1 : bottom] {
		if strings.TrimSpace(line) != "" {
			return false
		}
	}
	return true
}

// PiEditorShowing reports whether a Pi pane is waiting for input: its editor
// is drawn with no live turn, whether or not it holds a draft.
func PiEditorShowing(output string) bool {
	lines := strings.Split(stripANSICodes(output), "\n")
	top, _, ok := piEditorBox(lines)
	return ok && !piStatusShowing(lines, top)
}

// piEditorBox returns the line indexes of the top and bottom rules of the
// editor at the bottom of a Pi screen. ok is false when no editor is drawn.
// Rules in the transcript (markdown, tool output) are indented and never sit
// directly above the footer, so the bottom-most rule drawn from column 0
// near the bottom is the editor's.
func piEditorBox(lines []string) (top, bottom int, ok bool) {
	seen := 0
	for i := len(lines) - 1; i >= 0 && seen < piFooterScanLines; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		seen++
		if !piBottomRuleRe.MatchString(lines[i]) {
			continue
		}
		for j := i - 1; j >= 0 && i-j <= piEditorMaxLines+1; j-- {
			if strings.HasPrefix(lines[j], "─") {
				return j, i, true
			}
		}
		return -1, -1, false
	}
	return -1, -1, false
}

// piStatusShowing reports whether Pi's status indicator is drawn in the
// editor's top rule (lines[top]) or on its own line just above the editor.
func piStatusShowing(lines []string, top int) bool {
	if piWorkingLineRe.MatchString(lines[top]) {
		return true
	}
	for i := top - 1; i >= 0 && top-i <= piStatusLinesAboveEditor; i-- {
		if piWorkingLineRe.MatchString(lines[i]) {
			return true
		}
	}
	return false
}

// Kimi (kimi) patterns.
var (
	kimiRateLimitPatterns = []string{
		"rate limit",
		"too many requests",
		"quota exceeded",
		"usage limit",
		"try again later",
	}

	kimiWorkingPatterns = []string{
		"```",
		"tool call",
		"running ",
		"reading ",
		"writing ",
		"editing ",
		"searching ",
		"thinking",
	}

	kimiIdlePatterns = []*regexp.Regexp{
		regexp.MustCompile(`>\s*$`),
		regexp.MustCompile(`(?m)^>\s*$`),
		regexp.MustCompile(`(?m)^kimi>\s*$`),
		regexp.MustCompile(`(?i)kimi\s+(code|cli)`),
	}

	kimiErrorPatterns = []string{
		"error:",
		"failed:",
		"exception:",
		"permission denied",
		"connection refused",
		"timeout",
	}

	kimiHeaderPattern = regexp.MustCompile(`(?i)\b(kimi|moonshot)(\s+(code|cli|ai))?\b`)
)

// Muse Code (muse) patterns, measured against muse 1.4.11 in tmux.
//
// Muse draws the same "❯" composer glyph as Claude Code and keeps it on screen
// while a turn runs, with the live "◇ Thinking (1s · esc to interrupt)" line
// just above the composer rule. So the empty composer only means idle once
// MuseActivelyWorking has ruled out a live turn, and DetectAgentType checks the
// Muse signatures before Claude's: a Muse status bar whose cwd contains
// "claude" would otherwise match ccHeaderPattern.
var (
	museRateLimitPatterns = []string{
		"rate limit",
		"too many requests",
		"quota exceeded",
		"usage limit",
		"try again later",
	}

	// museWorkingPatterns are the in-flight markers of a Muse turn. Both are
	// shared with Claude Code, so they say nothing about which agent is running
	// and are left out of detectByPatternFrequency.
	museWorkingPatterns = []string{
		"thinking (",
		"esc to interrupt",
	}

	// museIdlePatterns match a composer waiting for input: a "❯" line that is
	// empty or shows one of Muse's dim tips (MuseComposerTipPrefixes), with
	// the composer rule directly below it. A composer holding a draft is not
	// idle, and submitted prompts echoed into the transcript ("❯ fix the bug")
	// never match because no rule follows them. Callers rule out
	// MuseActivelyWorking first: the empty composer stays drawn below the live
	// working line for the whole turn.
	museIdlePatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?m)^[ \t\x{00a0}]*❯[ \t\x{00a0}]*\r?\n[ \t]*─`),
		regexp.MustCompile(`(?m)^[ \t\x{00a0}]*❯[ \t\x{00a0}]+` + MuseComposerTipPattern() + `\n[ \t]*─`),
	}

	// museComposerTipTextRe matches the whole text of a composer line, after
	// the "❯" marker and with surrounding blanks trimmed, that is one of
	// Muse's dim tips.
	museComposerTipTextRe = regexp.MustCompile(`^` + MuseComposerTipPattern() + `$`)

	museErrorPatterns = []string{
		"error:",
		"failed:",
		"exception:",
		"permission denied",
		"connection refused",
		"timeout",
	}

	// museHeaderPattern matches the "  Muse Code 1.4.11" banner at the top of
	// a Muse session, alone on its line. Another agent quoting
	// `muse --version` prints "Muse Code 1.4.11 (1.4.11-R6667.1)", indented
	// under its tool call, so neither the build suffix nor a deeper indent
	// counts.
	museHeaderPattern = regexp.MustCompile(`(?m)^ {0,2}Muse Code \d+\.\d+(?:\.\d+)?[ \t]*$`)

	// museStatusBarPattern matches the status bar ("muse-wonder-1.0 · /repo ·
	// YOLO" or "muse-wonder-full-1.0 · max · /repo · YOLO"), which stays on
	// screen after the banner scrolls into history. IsMuseOutput applies it
	// only to the bottom lines of a capture, where Muse draws it.
	museStatusBarPattern = regexp.MustCompile(`^\s*muse-[a-z0-9.-]+ · \S`)

	// museActiveWorkLineRe matches Muse's live working line
	// (MuseWorkingLinePattern).
	museActiveWorkLineRe = regexp.MustCompile(MuseWorkingLinePattern)
)

// MuseWorkingLinePattern matches Muse's live working line, whole or cut short
// by a narrow pane. The first alternative is the parenthetical of the whole
// line ("(1s · esc to interrupt)", "(1m 5s · esc to interrupt)"); it is
// verb-agnostic because the label varies (Thinking, Working, Calling tools,
// Compacting context) and a missed working line makes a busy pane look idle.
// The second is the line Muse truncates at the pane edge, with no ellipsis,
// when the pane is narrower than it (measured on muse 1.4.11: "◈ Thinking
// (1s · esc to in" at 26 columns, "◆ Thinking (3s" at 14): a spinner glyph
// (◇/◈/◆), a label and the elapsed time, with the line ending before the
// closing parenthesis. A reply line that only starts with ◆ does not match:
// it needs "(" and an elapsed time, and a reply such as "◆ Done (3s)" closes
// the parenthesis.
const MuseWorkingLinePattern = `(?im)\([^()]*\besc\s+to\s+interrupt\s*\)` +
	`|^[ \t\x{00a0}]*[◇◈◆] [^\n()]*\(\d+(?:\.\d+)?[hms](?: \d+(?:\.\d+)?[hms])*(?: \d+(?:\.\d+)?)?(?: ·[^\n()]*|[ \t]*)$`

// MuseComposerTipPrefixes are the leading words of the feature tips Muse
// draws in an empty composer. About seven seconds after a turn ends, an idle
// Muse pane shows one tip ("❯ Type @ to search and insert workspace file
// paths" in a live echo pane) and keeps it while the composer stays empty.
// The list covers every tip text in the muse 1.4.11 binary's tip table.
// MuseComposerTipPattern turns it into the matcher that idle detection
// (museIdlePatterns), the robot pattern library and the tmux composer-clear
// check all use.
var MuseComposerTipPrefixes = []string{
	"Type @ to search and insert",
	"Start a message with ! to run",
	"Paste an image with Ctrl+V",
	"Press Alt+V to dictate",
	"Tab queues or steers",
	"Press ? on an empty composer",
	"Press Ctrl+O to expand",
	"/side asks a quick question",
	"/fork branches the conversation",
	"/goal pins a session objective",
	"/resume reopens a past session",
	"Ask to 'use a workflow'",
	"Say 'use a workflow'",
	"Ask to monitor a running",
	"/agent opens the subagent",
	"/tasks shows workflows",
	"/loop 10m <prompt>",
	"Ctrl+B sends the selected task",
	"/skills browses ready-made",
	"/plugins marketplace installs",
	"/effort adjusts reasoning",
	"/usage shows this session",
	"/export saves the conversation",
}

// museTipMinWords is the fewest leading words of a tip that still read as the
// tip when a narrow pane cuts it short. A single word is not enough: a draft
// such as "Type" or "/fork" would read as an empty composer.
const museTipMinWords = 2

// MuseComposerTipPattern returns an unanchored regular expression for the
// text of a Muse composer line showing one of MuseComposerTipPrefixes, from
// the first character after the blanks that follow "❯" to the end of the
// line. Muse word-wraps a tip too long for the pane and draws only its first
// line, with no ellipsis, so a narrow pane shows fewer words than the prefix
// (measured on muse 1.4.11: "❯ Type @ to search and" at 26 to 30 columns,
// "❯ Type @" at 12). A line matches when it holds a whole prefix followed by
// anything, or at least the prefix's first museTipMinWords words followed only
// by blanks or "…".
func MuseComposerTipPattern() string {
	alternatives := make([]string, 0, 2*len(MuseComposerTipPrefixes))
	for _, prefix := range MuseComposerTipPrefixes {
		alternatives = append(alternatives, regexp.QuoteMeta(prefix)+`[^\n]*`)
		words := strings.Fields(prefix)
		if len(words) < museTipMinWords {
			continue
		}
		// Each later word is optional only if every word before it is shown:
		// "a(?: b(?: c)?)?".
		rest := ""
		for i := len(words) - 1; i >= museTipMinWords; i-- {
			rest = `(?: ` + regexp.QuoteMeta(words[i]) + rest + `)?`
		}
		lead := regexp.QuoteMeta(strings.Join(words[:museTipMinWords], " "))
		alternatives = append(alternatives, lead+rest+`[ \t\x{00a0}…]*`)
	}
	return `(?:` + strings.Join(alternatives, "|") + `)`
}

// IsMuseComposerTip reports whether text, the content of a Muse composer line
// after the "❯" marker with surrounding blanks trimmed, is one of Muse's dim
// tips, whole or cut short by a narrow pane. Such a composer is empty.
func IsMuseComposerTip(text string) bool {
	return museComposerTipTextRe.MatchString(text)
}

// museLiveTailLines bounds the window MuseActivelyWorking scans when no
// composer line is on screen. The working line is the sixth line from the
// bottom (working line, blank, rule, composer, rule, status bar); ten leaves
// room to spare without reaching transcript history.
const museLiveTailLines = 10

// museWorkingLinesAboveComposer bounds how far above the composer's "❯" line
// MuseActivelyWorking looks for the live working line. The working line sits
// three lines above it (working line, blank, voice-input rule), so seven
// leaves four lines of slack, as the ten-line tail did.
const museWorkingLinesAboveComposer = 7

// museIdleScanLines is the trailing window MuseIdlePromptShowing searches for
// the idle composer (composer line, rule, status bar, with room to spare). It
// matches the window the agent parser uses for Muse, so both report the same
// idle verdict for a capture.
const museIdleScanLines = 5

// MuseActivelyWorking reports whether a Muse pane shows a live turn. Muse
// replaces the working line with the reply when the turn ends, so a match
// just above the composer means the turn is still running, even though the
// "❯" composer is drawn below it. The window is anchored on the composer, not
// on the bottom of the capture: text sent while a turn runs stays in the
// composer as a draft, and a draft that wraps (a 480-character one took nine
// lines in a 60-column pane) pushed the working line out of a fixed bottom
// window, so a busy pane read as idle.
func MuseActivelyWorking(output string) bool {
	clean := stripANSICodes(output)
	lines := strings.Split(clean, "\n")
	composer := museComposerLine(lines)
	if composer < 0 {
		return museActiveWorkLineRe.MatchString(util.GetLastNLines(clean, museLiveTailLines))
	}
	start := max(0, composer-museWorkingLinesAboveComposer)
	return museActiveWorkLineRe.MatchString(strings.Join(lines[start:composer], "\n"))
}

// museComposerLine returns the index of the bottom-most line led by the "❯"
// composer marker, or -1 when there is none. Submitted prompts echoed into
// the transcript also start with "❯", but Muse draws its composer below them,
// and the continuation lines of a wrapped draft are indented.
func museComposerLine(lines []string) int {
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(strings.TrimLeft(lines[i], " \t "), "❯") {
			return i
		}
	}
	return -1
}

// MuseIdlePromptShowing reports whether a Muse pane is waiting for input at
// an empty composer: no live turn, and the bottom composer is empty or shows
// one of Muse's dim tips. A composer holding a draft returns false, so a
// pane is never offered new work on top of unsent text.
// status.DetectIdleFromOutput (and through it the unified status detector's
// dispatch confidence) calls this directly; the agent parser's detectIdle
// applies the same museIdlePatterns over the same window.
func MuseIdlePromptShowing(output string) bool {
	if MuseActivelyWorking(output) {
		return false
	}
	tail := util.GetLastNLines(stripANSICodes(output), museIdleScanLines)
	return matchAnyRegex(tail, museIdlePatterns)
}

// museStatusBarScanLines is how many non-empty lines at the bottom of a
// capture IsMuseOutput checks for the status bar. Muse always draws it last,
// below the composer and any slash or @ menu; the second line covers a status
// bar that wraps in a narrow pane.
const museStatusBarScanLines = 2

// IsMuseOutput reports whether output carries a Muse Code signature: the
// version banner, or the model-led status bar on the bottom lines. The status
// bar counts only at the bottom so that another agent's transcript quoting a
// Muse screen (a capture-pane dump, a README) is not read as Muse.
func IsMuseOutput(output string) bool {
	clean := stripANSICodes(output)
	if museHeaderPattern.MatchString(clean) {
		return true
	}
	lines := strings.Split(clean, "\n")
	seen := 0
	for i := len(lines) - 1; i >= 0 && seen < museStatusBarScanLines; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		seen++
		if museStatusBarPattern.MatchString(lines[i]) {
			return true
		}
	}
	return false
}

// Cursor (cursor) patterns.
var (
	cursorRateLimitPatterns = []string{
		"rate limit",
		"too many requests",
		"quota exceeded",
	}

	cursorWorkingPatterns = []string{
		"```",
		"writing ",
		"reading ",
		"searching ",
		"analyzing ",
		"generating ",
	}

	cursorIdlePatterns = []*regexp.Regexp{
		regexp.MustCompile(`>\s*$`),
		regexp.MustCompile(`cursor>\s*$`),
	}

	cursorErrorPatterns = []string{
		"error:",
		"failed:",
		"exception:",
	}

	cursorHeaderPattern = regexp.MustCompile(`(?i)(cursor|cursor\s+ai)`)
)

// Windsurf (windsurf) patterns.
var (
	windsurfRateLimitPatterns = []string{
		"rate limit",
		"too many requests",
		"quota exceeded",
	}

	windsurfWorkingPatterns = []string{
		"```",
		"writing ",
		"reading ",
		"searching ",
		"analyzing ",
		"generating ",
	}

	windsurfIdlePatterns = []*regexp.Regexp{
		regexp.MustCompile(`>\s*$`),
		regexp.MustCompile(`windsurf>\s*$`),
	}

	windsurfErrorPatterns = []string{
		"error:",
		"failed:",
		"exception:",
	}

	windsurfHeaderPattern = regexp.MustCompile(`(?i)(windsurf|windsurf\s+ide)`)
)

// Aider (aider) patterns.
var (
	aiderRateLimitPatterns = []string{
		"rate limit",
		"too many requests",
		"quota exceeded",
	}

	aiderWorkingPatterns = []string{
		"```",
		"applied edit",
		"committing",
		"repo-map",
		"analyzing",
		"searching",
	}

	aiderIdlePatterns = []*regexp.Regexp{
		regexp.MustCompile(`>\s*$`),
		regexp.MustCompile(`aider>\s*$`),
	}

	aiderErrorPatterns = []string{
		"error:",
		"failed:",
		"exception:",
	}

	aiderHeaderPattern = regexp.MustCompile(`(?i)(aider|aider\s+chat)`)
)

// Ollama (ollama) patterns.
var (
	ollamaRateLimitPatterns = []string{
		"rate limit",
		"too many requests",
		"quota exceeded",
	}

	ollamaWorkingPatterns = []string{
		"```",
		"pulling manifest",
		"verifying sha256 digest",
		"writing manifest",
		"removing any unused layers",
		"generating ",
		"thinking",
	}

	ollamaIdlePatterns = []*regexp.Regexp{
		regexp.MustCompile(`>\s*$`),
		regexp.MustCompile(`ollama>\s*$`),
	}

	ollamaErrorPatterns = []string{
		"error:",
		"failed:",
		"exception:",
	}

	ollamaHeaderPattern = regexp.MustCompile(`(?im)(^ollama>\s*$|\bollama\s+(run|chat|serve|pull)\b|^\s*ollama\s+cli\b)`)
)

// claudeLiveTailLines bounds the live-tail window scanned for Claude
// working/idle classification. Claude Code pins its input box to the BOTTOM of
// the screen and renders the live spinner just above it, so the most-recent
// dynamic state always lives in the last handful of lines. 16 is wide enough to
// span the spinner line, the box border rules, and the status footer while
// staying short of historical scrollback that could carry a stale spinner.
const claudeLiveTailLines = 16

// claudeIdleScanLines bounds how many trailing lines are scanned for an idle
// PROMPT pattern (as opposed to the spinner/footer live tail). It is narrower
// than claudeLiveTailLines so a stale prompt deep in scrollback is not matched,
// and matches the CC window detectIdle and status.maxIdleScanLines already use.
const claudeIdleScanLines = 12

// ClaudeTurnState describes the newest dynamic marker in a Claude Code pane.
//
// Claude Code keeps a persistent "❯ " input box pinned to the BOTTOM of the
// screen at all times, even while busy. The live activity spinner (e.g.
// "✻ Monitoring 17 agents… (ctrl+c to interrupt · 15m 52s · …)") renders just
// ABOVE that box while a turn is in flight; when the turn ends Claude replaces
// the spinner with a completion line ("✻ Cooked for 1m 42s") and/or a
// "new task?" footer. Position relative to the input box therefore tells you
// nothing — only the relative ORDER of the spinner vs. the turn-ended markers
// does.
//
// Semantics: a pane is WORKING iff an active spinner is the MOST-RECENT dynamic
// marker — i.e. no turn-ended marker (completion line or "new task?" hint) or
// terminal error appears AFTER the last spinner line within the live tail.
//
// The classifier is deliberately biased to the SAFE failure: when a spinner is
// present but ordering is ambiguous it returns true (false-WORKING). A
// dispatcher must NEVER inject a second task into a working agent, so the only
// acceptable error is to treat a maybe-idle pane as busy.
type ClaudeTurnState uint8

const (
	ClaudeTurnUnknown ClaudeTurnState = iota
	ClaudeTurnWorking
	ClaudeTurnEnded
	ClaudeTurnError
)

// DetectClaudeTurnState returns the state represented by Claude's newest
// dynamic marker in the live tail. Completion/error markers are evaluated
// after spinner markers on each line so an unexpected overlap fails closed.
func DetectClaudeTurnState(output string) ClaudeTurnState {
	if output == "" {
		return ClaudeTurnUnknown
	}
	clean := stripANSICodes(output)
	tail := util.GetLastNLines(clean, claudeLiveTailLines)
	if strings.TrimSpace(tail) == "" {
		return ClaudeTurnUnknown
	}

	lines := strings.Split(tail, "\n")
	latestState := ClaudeTurnUnknown
	for _, line := range lines {
		if claudeIsActiveSpinnerLine(line) {
			latestState = ClaudeTurnWorking
		}
		if claudeIsTurnEndedLine(line) {
			latestState = ClaudeTurnEnded
		}
		if claudeIsTerminalErrorLine(line) {
			latestState = ClaudeTurnError
		}
	}
	return latestState
}

// ClaudeActivelyWorking reports whether the newest Claude turn marker is a
// live spinner rather than completion or a terminal tool-result error.
func ClaudeActivelyWorking(output string) bool {
	return DetectClaudeTurnState(output) == ClaudeTurnWorking
}

// CodexActivelyWorking reports whether Codex's trailing live window contains a
// structural in-flight marker. Codex leaves old spinner lines in scrollback and
// keeps its input chrome visible during work, so the bounded tail is required:
// matching the full capture would make completed turns appear busy forever,
// while treating the persistent chevron as idle would make active panes unsafe
// for dispatch.
func CodexActivelyWorking(output string) bool {
	tail := util.GetLastNLines(stripANSICodes(output), codexLiveTailLines)
	return codexActiveWorkLineRe.MatchString(tail) || codexInterruptHintRe.MatchString(tail)
}

// claudeIsActiveSpinnerLine reports whether a single line is a live Claude
// spinner frame. A completion line ("✻ Cooked for 1m 42s") is explicitly
// excluded so it can never masquerade as active work.
func claudeIsActiveSpinnerLine(line string) bool {
	if claudeIsCompletionLine(line) {
		return false
	}
	if claudeActiveSpinnerLineRe.MatchString(line) {
		return true
	}
	if claudeInFlightHintRe.MatchString(line) {
		return true
	}
	if claudeThinkingSpinnerRe.MatchString(line) {
		return true
	}
	return false
}

// claudeIsTurnEndedLine reports whether a single line is a turn-ended marker
// (completion summary or the post-turn "new task?" hint).
func claudeIsTurnEndedLine(line string) bool {
	return claudeIsCompletionLine(line) || claudeNewTaskFooterRe.MatchString(line)
}

func claudeIsTerminalErrorLine(line string) bool {
	return claudeTerminalErrorLineRe.MatchString(line)
}

// claudeChevronBoxRe matches Claude Code's bottom-pinned input box prompt on
// its own line, whether empty ("❯ ") or holding queued/prefilled text
// ("❯ Continue working on your assigned bead…"). The chevron at line-start
// (after optional indentation) is the load-bearing signal; the existing
// ccIdlePatterns only match an EMPTY chevron, so this broadens idle recognition
// to the finished-turn-with-queued-text case.
var claudeChevronBoxRe = regexp.MustCompile(`(?m)^\s*❯(?:[\s\x{00a0}].*|[\s\x{00a0}]*)$`)

// claudeComposeBoxFooterRe matches the "⏵⏵" mode footer Claude Code renders at
// the very bottom of its compose box (e.g. "⏵⏵ bypass permissions on (shift+tab
// to cycle)" or "⏵⏵ accept edits on"). A freshly-spawned agent shows its init
// prompt prefilled in the input box — or just the "…" ellipsis indicator — with
// NO completion line and NO "new task?" hint yet, so every other turn-ended
// recognizer misses it and the swarm never starts ("Idle Agents: 0"). The
// "⏵⏵" footer proves the Claude TUI is alive at its compose box.
//
// CRITICAL: this footer is drawn DURING work too (it is permanent chrome, like
// the input box). It is therefore only a positive idle signal when combined
// with `!ClaudeActivelyWorking` — which every caller of ClaudeIdlePromptShowing
// already gates on. Used ungated it would false-idle a working pane (this is
// exactly the trap the removed "bypass permissions on" ccIdlePattern fell into).
var claudeComposeBoxFooterRe = regexp.MustCompile(`⏵⏵`)

// ClaudeIdlePromptShowing reports whether a Claude Code pane is displaying an
// idle / finished-turn prompt: the bottom-pinned input box (empty, holding
// queued text, or an "…" ellipsis), a glyph-led completion summary, or the
// post-turn "new task?" footer. It is the idle counterpart to
// ClaudeActivelyWorking and is the single shared recognizer used by every
// Claude state-detection path (parser, status, robot) so they agree.
//
// IMPORTANT: callers must gate on `!ClaudeActivelyWorking(output)` first — this
// recognizer intentionally matches the input box even while a turn is in flight
// (the box is always drawn). Used alone it would false-idle a working pane.
func ClaudeIdlePromptShowing(output string) bool {
	// Bound the prompt scan to the same trailing window detectIdle uses for
	// Claude (claudeIdleScanLines) so a stale prompt deep in scrollback is not
	// matched, then check the finished-turn footer over the slightly wider live
	// tail. Empty/whitespace output is NOT a positive idle signal here: callers
	// that want "empty ⇒ idle" (parser.detectIdle) handle that explicitly.
	tail := util.GetLastNLines(stripANSICodes(output), claudeIdleScanLines)
	if strings.TrimSpace(tail) == "" {
		return false
	}
	// Scan each trailing line for an idle prompt pattern. Joining the tail and
	// matching `>\s*$` against the whole block would miss a prompt that is not
	// the very last line (e.g. "claude>\nfollowup"), so we match per line — the
	// same line-oriented semantics the legacy prompt scan used.
	for _, line := range strings.Split(tail, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if matchAnyRegex(line, ccIdlePatterns) {
			return true
		}
	}
	return claudeFinishedTurnIdle(output)
}

// claudeFinishedTurnIdle reports whether the live tail shows a finished-turn
// idle state: the bottom input box (empty, or holding queued text / an "…"
// ellipsis) and/or the post-turn "new task?" footer. Callers must already
// have ruled out ClaudeActivelyWorking; this only broadens idle recognition
// beyond the empty-chevron-only ccIdlePatterns and never overrides a working
// verdict.
func claudeFinishedTurnIdle(output string) bool {
	clean := stripANSICodes(output)
	tail := util.GetLastNLines(clean, claudeLiveTailLines)
	if strings.TrimSpace(tail) == "" {
		return false
	}
	if claudeNewTaskFooterRe.MatchString(tail) {
		return true
	}
	if claudeCompletionLineRe.MatchString(tail) {
		return true
	}
	if claudeChevronBoxRe.MatchString(tail) {
		return true
	}
	// The "⏵⏵" compose-box footer means the Claude TUI is alive at its input
	// box. Combined with the caller's required `!ClaudeActivelyWorking` gate,
	// "box alive + no active work = idle" — this is what lets a fresh-spawn
	// capture (init prompt prefilled or just an "…" ellipsis, no spinner) start
	// the swarm. It is NOT gated here because ClaudeIdlePromptShowing's contract
	// already requires callers to have ruled out active work.
	if claudeComposeBoxFooterRe.MatchString(tail) {
		return true
	}
	return false
}

// claudeIsCompletionLine reports whether a single line is Claude's glyph-led
// turn-ended completion summary ("✻ Cooked for 16m 20s"). It rejects active
// spinner lines (which carry a "…(" progress decoration) and "thought for"
// prose annotations.
func claudeIsCompletionLine(line string) bool {
	if strings.Contains(line, "…") || strings.Contains(line, "(") {
		return false
	}
	return claudeCompletionLineRe.MatchString(line)
}

// matchAny returns true if text contains any of the patterns (case-insensitive).
func matchAny(text string, patterns []string) bool {
	if len(patterns) == 0 {
		return false
	}
	textLower := strings.ToLower(text)
	for _, p := range patterns {
		// Optimization: patterns are already lowercase in ntm.
		if strings.Contains(textLower, p) {
			return true
		}
	}
	return false
}

// matchAnyRegex returns true if text matches any of the regex patterns.
func matchAnyRegex(text string, patterns []*regexp.Regexp) bool {
	for _, p := range patterns {
		if p.MatchString(text) {
			return true
		}
	}
	return false
}

// lastLineIdxMatching returns the highest line index (0-based, counting from
// the start of `text`) that matches any of the regex patterns. Returns -1 when
// nothing matches. Used by detectIdle to compare ordering of competing markers
// (e.g. a fresh idle prompt below a stale spinner) so the most-recent state
// wins instead of an unconditional override.
func lastLineIdxMatching(text string, patterns []*regexp.Regexp) int {
	if text == "" || len(patterns) == 0 {
		return -1
	}
	lines := strings.Split(text, "\n")
	last := -1
	for i, line := range lines {
		for _, p := range patterns {
			if p.MatchString(line) {
				last = i
				break
			}
		}
	}
	return last
}

// collectMatches returns all patterns that matched in the text.
func collectMatches(text string, patterns []string) []string {
	var matches []string
	if len(patterns) == 0 {
		return matches
	}
	textLower := strings.ToLower(text)
	for _, p := range patterns {
		// Optimization: patterns are already lowercase in ntm.
		if strings.Contains(textLower, p) {
			matches = append(matches, p)
		}
	}
	return matches
}

// extractFloat extracts the last float value from a regex match group.
// Returns nil if no match or parse error.
func extractFloat(pattern *regexp.Regexp, text string) *float64 {
	matches := pattern.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}
	match := matches[len(matches)-1]
	if len(match) < 2 {
		return nil
	}
	// Handle comma-separated numbers (e.g., "219,582")
	cleaned := strings.ReplaceAll(match[1], ",", "")
	val, err := strconv.ParseFloat(cleaned, 64)
	if err == nil {
		return &val
	}
	return nil
}

// extractInt extracts the last integer value from a regex match group.
// Returns nil if no match or parse error.
func extractInt(pattern *regexp.Regexp, text string) *int64 {
	matches := pattern.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}
	match := matches[len(matches)-1]
	if len(match) < 2 {
		return nil
	}
	// Handle comma-separated numbers
	cleaned := strings.ReplaceAll(match[1], ",", "")
	val, err := strconv.ParseInt(cleaned, 10, 64)
	if err == nil {
		return &val
	}
	return nil
}

// ansiPattern matches ANSI escape sequences.
// Matches CSI sequences (with private mode ?) and OSC sequences (title setting etc)
var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]|\x1b\][^\a\x1b]*(\a|\x1b\\)`)

// stripANSICodes removes ANSI escape sequences and other problematic control characters.
// This ensures pattern matching works correctly on terminal output.
func stripANSICodes(text string) string {
	// First, remove common ANSI escape sequences
	clean := ansiPattern.ReplaceAllString(text, "")

	// Second, handle carriage returns by keeping only the text after the last CR in each line.
	// This simulates the terminal's behavior of overwriting lines.
	if !strings.Contains(clean, "\r") {
		return clean
	}

	var sb strings.Builder
	sb.Grow(len(clean))

	start := 0
	for {
		newlineIdx := strings.Index(clean[start:], "\n")
		var line string
		if newlineIdx == -1 {
			line = clean[start:]
		} else {
			line = clean[start : start+newlineIdx]
		}

		if lastCR := strings.LastIndex(line, "\r"); lastCR != -1 {
			sb.WriteString(line[lastCR+1:])
		} else {
			sb.WriteString(line)
		}

		if newlineIdx == -1 {
			break
		}
		sb.WriteByte('\n')
		start += newlineIdx + 1
	}

	return sb.String()
}

// PatternSet groups all patterns for a specific agent type.
// This makes it easier to pass around and test pattern collections.
type PatternSet struct {
	RateLimitPatterns []string
	WorkingPatterns   []string
	IdlePatterns      []*regexp.Regexp
	ErrorPatterns     []string
	ContextWarnings   []string       // Only used by Claude Code
	ContextPattern    *regexp.Regexp // Explicit context extraction (Codex)
	TokenPattern      *regexp.Regexp // Token usage extraction
	MemoryPattern     *regexp.Regexp // Memory usage (Gemini)
	HeaderPattern     *regexp.Regexp
}

// GetPatternSet returns the pattern set for the given agent type.
func GetPatternSet(agentType AgentType) *PatternSet {
	switch agentType {
	case AgentTypeClaudeCode:
		return &PatternSet{
			RateLimitPatterns: ccRateLimitPatterns,
			WorkingPatterns:   ccWorkingPatterns,
			IdlePatterns:      ccIdlePatterns,
			ErrorPatterns:     ccErrorPatterns,
			ContextWarnings:   ccContextWarnings,
			HeaderPattern:     ccHeaderPattern,
		}
	case AgentTypeCodex:
		return &PatternSet{
			RateLimitPatterns: codRateLimitPatterns,
			WorkingPatterns:   codWorkingPatterns,
			IdlePatterns:      codIdlePatterns,
			ErrorPatterns:     codErrorPatterns,
			ContextPattern:    codContextPattern,
			TokenPattern:      codTokenPattern,
			HeaderPattern:     codHeaderPattern,
		}
	case AgentTypeGemini, AgentTypeAntigravity:
		// Antigravity (agy) shares the Gemini CLI's TUI behavior, so it reuses
		// the gmi* detection pattern set (see parser.go).
		return &PatternSet{
			RateLimitPatterns: gmiRateLimitPatterns,
			WorkingPatterns:   gmiWorkingPatterns,
			IdlePatterns:      gmiIdlePatterns,
			ErrorPatterns:     gmiErrorPatterns,
			MemoryPattern:     gmiMemoryPattern,
			HeaderPattern:     gmiHeaderPattern,
		}
	case AgentTypeKimi:
		return &PatternSet{
			RateLimitPatterns: kimiRateLimitPatterns,
			WorkingPatterns:   kimiWorkingPatterns,
			IdlePatterns:      kimiIdlePatterns,
			ErrorPatterns:     kimiErrorPatterns,
			HeaderPattern:     kimiHeaderPattern,
		}
	case AgentTypeMuse:
		return &PatternSet{
			RateLimitPatterns: museRateLimitPatterns,
			WorkingPatterns:   museWorkingPatterns,
			IdlePatterns:      museIdlePatterns,
			ErrorPatterns:     museErrorPatterns,
			HeaderPattern:     museHeaderPattern,
		}
	case AgentTypeCursor:
		return &PatternSet{
			RateLimitPatterns: cursorRateLimitPatterns,
			WorkingPatterns:   cursorWorkingPatterns,
			IdlePatterns:      cursorIdlePatterns,
			ErrorPatterns:     cursorErrorPatterns,
			HeaderPattern:     cursorHeaderPattern,
		}
	case AgentTypeWindsurf:
		return &PatternSet{
			RateLimitPatterns: windsurfRateLimitPatterns,
			WorkingPatterns:   windsurfWorkingPatterns,
			IdlePatterns:      windsurfIdlePatterns,
			ErrorPatterns:     windsurfErrorPatterns,
			HeaderPattern:     windsurfHeaderPattern,
		}
	case AgentTypeAider:
		return &PatternSet{
			RateLimitPatterns: aiderRateLimitPatterns,
			WorkingPatterns:   aiderWorkingPatterns,
			IdlePatterns:      aiderIdlePatterns,
			ErrorPatterns:     aiderErrorPatterns,
			HeaderPattern:     aiderHeaderPattern,
		}
	case AgentTypeOllama:
		return &PatternSet{
			RateLimitPatterns: ollamaRateLimitPatterns,
			WorkingPatterns:   ollamaWorkingPatterns,
			IdlePatterns:      ollamaIdlePatterns,
			ErrorPatterns:     ollamaErrorPatterns,
			HeaderPattern:     ollamaHeaderPattern,
		}
	default:
		return &PatternSet{} // Empty pattern set for unknown types
	}
}
