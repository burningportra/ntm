package agent

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

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
	//   ✻ Crunched for 10s · done 11:09 PM · 1 shell still running
	//
	// Current Claude Code appends " · done <time>" and " · N shell(s) still
	// running" segments; every completion line in a live swarm (2026-10)
	// carried one, so a bare end-of-line anchor recognized none of them.
	//
	// It must NOT match an active spinner ("Whirlpooling… (… · 2m 44s …)") nor a
	// "thought for 14s" prose annotation, hence: the leading glyph + verb, the
	// literal " for ", a duration, then only "·"-separated segments that carry
	// no "(" / "…" progress decoration up to the end of the line.
	//
	// The verb class is the Unicode capitalized-word class `\p{Lu}\p{Ll}+`
	// rather than ASCII `[A-Z][a-z]+`: Claude's whimsical spinner verbs include
	// accented forms ("✻ Sautéed for 36s", "✻ Flambéed for 12s") whose é/ü/etc.
	// fall outside `[a-z]` and would break recognition of the turn-ended line —
	// leaving a stale active spinner higher in the capture unsuppressed and the
	// pane misclassified as still WORKING. `\p{Lu}` (capitalized first letter)
	// preserves the anti-prose constraint; the whole-line and duration anchors
	// are unchanged.
	claudeCompletionLineRe = regexp.MustCompile(`(?m)^\s*[✻✶✳✢✽✦*]\s+\p{Lu}\p{Ll}+\s+for\s+(?:\d+\s*[hms]\s*)+(?:·[^\n…(]*)?$`)

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
		// Codex's current banner phrasing (bd-wtm0w): "You've hit your
		// usage limit ... try again at <date>", plus plan-limit variants.
		// Matching is substring containment, so the bare "hit your usage
		// limit" covers both the "you've"/"you have" prefixes. This list
		// cannot delegate to internal/ratelimit (import cycle: ratelimit
		// imports agent); keep it in sync with ratelimit's canonical
		// usageLimitBannerPatterns when provider copy changes.
		"hit your usage limit",
		"usage limit reached",
		"plan limit reached",
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
		regexp.MustCompile(`>\s*$`),                  // Standard prompt
		regexp.MustCompile(`\?\s*for\s*shortcuts`),   // Codex prompt line
		regexp.MustCompile(`codex>\s*$`),             // Codex prompt
		regexp.MustCompile(`(?m)^\s*[›»](?:\s.*)?$`), // Codex chevron prompt, empty or with prefilled input ("»" at effort ultra, GH#273)
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

// Antigravity CLI (agy) patterns for state detection. Its prompt and live-work
// footer differ from Gemini even though both providers share model/memory and
// rate-limit vocabulary (GH#270).
var (
	// agyHeaderPattern confirms output is from the Antigravity CLI.
	agyHeaderPattern = regexp.MustCompile(`(?i)antigravity`)

	// agyCancelHintRe matches the footer shown only while a turn is active.
	agyCancelHintRe = regexp.MustCompile(`(?i)\besc\s+to\s+cancel\b`)

	// The bare composer prompt is followed by shortcut/model footer lines, so
	// Gemini's end-of-buffer prompt expression cannot see it.
	agyIdlePatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?m)^\s*>\s*$`),
		regexp.MustCompile(`(?i)\?\s+for\s+shortcuts`),
	}
)

const agyLiveTailLines = 15

// AntigravityActivelyWorking reports whether the trailing live TUI frame
// contains the provider's in-flight cancel hint.
func AntigravityActivelyWorking(output string, paneWidth int) bool {
	clean := stripANSICodes(output)
	tail := util.GetLastNLines(clean, util.WidthAdaptiveTailLines(paneWidth, agyLiveTailLines))
	return agyCancelHintRe.MatchString(tail)
}

// Pi (pi) patterns. A pane known to run Pi is read with PiActivelyWorking and
// PiIdlePromptShowing, which follow the screen Pi 1.3.0 draws; the substring
// working and idle lists below only score and read panes of unknown type.
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

// piGhostSuggestionColor is the foreground pi-prompt-suggester (0.3.10)
// draws its ghost suggestion in: 256-color grey 244, written as
// "\x1b[38;5;244m" and reported back that way by tmux capture-pane -e. Pi's
// themes use true color, and text typed into the editor is drawn in the
// default foreground.
const piGhostSuggestionColor = "38;5;244"

// PiEditorMayHoldGhostSuggestion reports whether a plain capture of a Pi pane
// fits the ghost suggestion pi-prompt-suggester draws in an empty editor after
// a turn: no live turn, and the editor's first line holds text after the
// cursor cell an empty editor starts with. Plain text cannot tell the
// suggestion from a draft, so a caller that gets true takes an escape-coded
// capture for PiClearGhostSuggestion. A draft typed from column 0 returns
// false and costs no capture.
func PiEditorMayHoldGhostSuggestion(output string) bool {
	lines := strings.Split(stripANSICodes(output), "\n")
	top, bottom, ok := piEditorBox(lines)
	if !ok || bottom-top < 2 || piStatusShowing(lines, top) || !piPlainRuleRe.MatchString(lines[top]) {
		return false
	}
	first := lines[top+1]
	return strings.HasPrefix(first, " ") && strings.TrimSpace(first) != ""
}

// PiClearGhostSuggestion returns output, a plain capture of a Pi pane, with
// the lines inside its editor blanked when styled, an escape-coded capture of
// the pane's visible screen, shows every character in the editor drawn in the
// ghost suggestion's color. The ghost shows only in an empty editor and goes
// away on the first keystroke, so such an editor is an empty one. ok is false
// and output comes back unchanged when the editor holds text in any other
// color, when either capture shows a live turn, or when the captures disagree
// about the editor's text because the screen changed between them.
func PiClearGhostSuggestion(output, styled string) (string, bool) {
	lines := strings.Split(stripANSICodes(output), "\n")
	top, bottom, ok := piEditorBox(lines)
	if !ok || piStatusShowing(lines, top) || !piPlainRuleRe.MatchString(lines[top]) {
		return output, false
	}
	styledLines := strings.Split(styled, "\n")
	styledText := strings.Split(stripANSICodes(styled), "\n")
	styledTop, styledBottom, ok := piEditorBox(styledText)
	if !ok || len(styledText) != len(styledLines) || styledBottom-styledTop != bottom-top || piStatusShowing(styledText, styledTop) {
		return output, false
	}
	for i := top; i <= bottom; i++ {
		if strings.TrimRight(lines[i], " \t") != strings.TrimRight(styledText[styledTop+i-top], " \t") {
			return output, false
		}
	}
	if !piEditorTextIsGhost(styledLines[:styledBottom], styledTop) {
		return output, false
	}
	outputLines := strings.Split(output, "\n")
	if len(outputLines) != len(lines) {
		return output, false
	}
	for i := top + 1; i < bottom; i++ {
		outputLines[i] = ""
	}
	return strings.Join(outputLines, "\n"), true
}

// piEditorTextIsGhost reports whether the editor lines of an escape-coded
// capture, lines[top+1:], hold text and all of it is in
// piGhostSuggestionColor. Colors are tracked from the first line, because
// tmux emits only changes and carries them across lines: the second line of
// a wrapped suggestion starts with no escape at all.
func piEditorTextIsGhost(lines []string, top int) bool {
	foreground := ""
	sawText := false
	for i, line := range lines {
		for line != "" {
			if line[0] == '\x1b' {
				params, size, isSGR := piEscapeAt(line)
				if isSGR {
					foreground = piSGRForeground(foreground, params)
				}
				line = line[size:]
				continue
			}
			r, size := utf8.DecodeRuneInString(line)
			line = line[size:]
			if i <= top || unicode.IsSpace(r) {
				continue
			}
			if foreground != piGhostSuggestionColor {
				return false
			}
			sawText = true
		}
	}
	return sawText
}

// piEscapeAt measures the escape sequence at the start of s and, for an SGR
// sequence ("\x1b[...m"), returns its parameters.
func piEscapeAt(s string) (params string, size int, isSGR bool) {
	if len(s) < 2 {
		return "", len(s), false
	}
	switch s[1] {
	case '[':
		for i := 2; i < len(s); i++ {
			if c := s[i]; c >= 0x40 && c <= 0x7e {
				return s[2:i], i + 1, c == 'm'
			}
		}
	case ']', 'P', '_', '^', 'X':
		// OSC, DCS, APC, PM and SOS strings end at BEL or ST.
		for i := 2; i < len(s); i++ {
			if s[i] == '\a' {
				return "", i + 1, false
			}
			if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '\\' {
				return "", i + 2, false
			}
		}
	default:
		return "", 2, false
	}
	return "", len(s), false
}

// piSGRForeground applies one SGR sequence's parameters to foreground, a
// color written as its SGR parameters ("" for the default foreground).
func piSGRForeground(foreground, params string) string {
	codes := strings.Split(params, ";")
	for i := 0; i < len(codes); i++ {
		code := codes[i]
		switch {
		case code == "" || code == "0" || code == "39":
			foreground = ""
		case code == "38" || code == "48" || code == "58":
			// An extended color, 5;N or 2;R;G;B. Only 38 sets the foreground.
			span := 0
			if i+2 < len(codes) && codes[i+1] == "5" {
				span = 2
			} else if i+4 < len(codes) && codes[i+1] == "2" {
				span = 4
			}
			if code == "38" {
				foreground = strings.Join(codes[i:i+span+1], ";")
			}
			i += span
		case strings.HasPrefix(code, "38:"):
			foreground = strings.ReplaceAll(code, ":", ";")
		case len(code) == 2 && (code[0] == '3' || code[0] == '9') && code[1] >= '0' && code[1] <= '7':
			foreground = code
		}
	}
	return foreground
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

// Grok Build (grok) patterns for state detection.
//
// Derived from live captures of the official Grok Build TUI (grok 1.0.5,
// authenticated SuperGrok session, observed at 80/120/200-column widths for
// GH#251 phase 2). The TUI renders:
//
//   - a bottom-pinned bordered composer:      │ ❯ <text>              │
//   - a status line on the composer's bottom
//     border (permanent chrome):              ─ Grok 4.6 (high) · always-approve ─╯
//   - a live activity line while a turn is
//     in flight (braille spinner + verb):     ⠹ Waiting for response… 0.7s   … [stop]
//     ⠸ Thinking… 0.0s
//     ⠙ Responding… 1.7s
//   - a footer hint that carries "Esc:cancel"
//     ONLY while a turn is in flight:         Shift+Tab:mode  │  Esc:cancel  │  Ctrl+x:shortcuts
//   - a turn-ended summary:                   Worked for 12s
//   - an interrupt acknowledgement (Ctrl+C
//     during a turn cancels it; Ctrl+C at
//     idle is a no-op):                        Turn cancelled by user in 3.0s.
//
// Like Claude Code and codex, the composer box is drawn DURING work, so the
// "❯" glyph alone is never idle evidence — GrokActivelyWorking must be
// consulted first (see detectStateFlags).
var (
	grokRateLimitPatterns = []string{
		"rate limit",
		"too many requests",
		"quota exceeded",
		"usage limit",
	}

	grokWorkingPatterns = []string{
		"```",
		"writing ",
		"reading ",
		"running ",
		"executing ",
		"searching ",
		"generating ",
	}

	// grokActiveWorkLineRe matches the live activity line grok renders while a
	// turn is in flight: a braille spinner frame followed by one of the three
	// observed phase verbs and the ellipsis glyph.
	grokActiveWorkLineRe = regexp.MustCompile(`(?m)^\s*[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏]\s*(?:Waiting for response|Thinking|Responding)…`)

	// grokCancelHintRe matches the "Esc:cancel" footer hint that grok shows
	// only while a turn is in flight (it disappears at idle) — a second,
	// independent in-flight signal for frames where the spinner line wrapped
	// or scrolled out of the live tail.
	grokCancelHintRe = regexp.MustCompile(`Esc:cancel`)

	// grokIdlePatterns indicates waiting for input. The bordered composer line
	// ("│ ❯ …") is permanent chrome; callers must gate on !GrokActivelyWorking
	// (detectStateFlags does) before trusting it as idle evidence.
	grokIdlePatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?m)^\s*│\s*❯`),               // Bordered composer line
		regexp.MustCompile(`(?m)^\s*Worked\s+for\s+\d`),   // Turn-ended summary
		regexp.MustCompile(`Turn cancelled by user`),      // Post-interrupt acknowledgement
		regexp.MustCompile(`(?i)grok\s+build\s+\d+\.\d+`), // Welcome banner (fresh spawn)
	}

	grokErrorPatterns = []string{
		"error:",
		"failed:",
		"exception:",
	}

	// grokHeaderPattern confirms output is from the Grok Build TUI. The
	// status-line chrome ("Grok 4.6 (high) · always-approve") and the welcome
	// banner ("Grok Build  1.0.5") are both matched.
	grokHeaderPattern = regexp.MustCompile(`(?i)(grok\s+build|grok\s+\d+\.\d+\s*\()`)
)

// grokLiveTailLines bounds the live-tail window scanned for grok working
// classification. Grok pins its composer and activity line to the bottom of
// the screen, like codex, so the same budget applies.
const grokLiveTailLines = 15

// GrokActivelyWorking reports whether a Grok Build pane's trailing live window
// shows an in-flight turn: the braille spinner activity line ("⠹ Waiting for
// response… / Thinking… / Responding…") or the "Esc:cancel" footer hint, both
// of which grok renders only while a turn is running. The live window is
// width-adaptive; paneWidth is the real tmux pane width, pass 0 when unknown.
func GrokActivelyWorking(output string, paneWidth int) bool {
	clean := stripANSICodes(output)
	tail := util.GetLastNLines(clean, util.WidthAdaptiveTailLines(paneWidth, grokLiveTailLines))
	return grokActiveWorkLineRe.MatchString(tail) || grokCancelHintRe.MatchString(tail)
}

// Oh My Pi (omp, https://omp.sh) patterns for state detection.
//
// Derived from live tmux captures of omp v18.2.3 launched as
// `omp --auto-approve`, observed at 120- and 220-column widths under all three
// symbol presets (nerd, unicode, ascii). omp renders inline (no alternate
// screen) and pins a bordered composer box to the bottom of its output. The
// top border carries the status line and the embedded context gauge; the
// bottom border doubles as the draft's last input row, and a multi-line draft
// grows the box upward with bordered continuation rows:
//
//	╭── 󰵗   Union Alpha   proj ────6%─────────────󰁨──────────262K───╮
//	│  first line of a multi-line draft                             │
//	╰─ last line of the draft                                      ─╯
//
// (unicode: "╭── π > ⬢ Union Alpha > 🗑 proj ▶────6%…┃…262K───╮";
// ascii: "+-- pi > [M] Union Alpha > [T] proj >----6%…|…262K---+" over "+- … -+").
//
// While a turn is in flight omp replaces the status-line icon with a spinner
// frame plus an elapsed timer ("╭── ⠧ 1s   Union Alpha", ascii "+-- \ 5s >",
// "1m" once past a minute) and draws an activity line directly above the box
// that opens with the Esc interrupt hint: "󱊷 Working…" (nerd), "⎋ Working…"
// (unicode), "esc Working…" (ascii). During tool calls the label becomes the
// model's intent ("󱊷 Sleeping 25 seconds") but the hint glyph stays. Both
// signals vanish the moment the turn ends, so the permanent box is idle
// evidence only once both are absent.
//
// Other verified chrome:
//   - Esc interrupts a turn and leaves "F5 to Retry" above the idle box;
//     Ctrl+C does NOT interrupt (it clears the draft, and two presses within
//     ~0.5s on an empty draft quit omp); Ctrl+D on an empty draft quits.
//   - A large bracketed paste collapses into a draft token (" #1",
//     "📄 #1", "txt #1") with a preview box above whose bottom edge reads
//     "╰ +15 lines ─╯" ("+ +15 lines -+" in ascii).
//   - Typing "/" opens the slash-command autocomplete list BELOW the box.
//   - Provider failures end the turn and render a rule-framed block directly
//     above the quiet composer, ending in "Dismissed when you send your next
//     message." (" 401 Invalid API Key" from a bad key, " server_error:
//     ERROR" from an OpenRouter failure in a live swarm pane); sending any
//     new message dismisses it and resumes. An unknown --model boots with
//     `Warning: Model "x" not found` and a prompt then fails with "Error: No
//     model selected.".
//   - No omp rate-limit frame has been captured yet, so omp has no
//     rate-limit patterns: transcript text that merely mentions rate limits
//     must not park a pane in RATE_LIMITED_WAIT. A provider 429 surfaces as a
//     provider error.
var (
	ompWorkingPatterns = []string{
		"working…",
		"interrupting…",
	}

	// ompErrorPatterns are the non-framed error lines; the framed provider
	// error block is recognised structurally (OmpProviderError).
	ompErrorPatterns = []string{
		"error: no model selected",
		"warning: model \"",
	}

	// ompHeaderPattern matches the welcome banner's top edge
	// ("╭─── omp v18.2.3 ───" / "+--- omp v18.2.3 ---").
	ompHeaderPattern = regexp.MustCompile(`(?m)^\s*(?:╭─+|\+-+)\s+omp\s+v\d+\.\d+`)

	// ompComposerTopRe matches the composer's top border: exactly two rule
	// glyphs after the corner, whitespace, then the status line. Tool-output
	// boxes ("╭────╮"), the banner ("╭─── omp v…") and the model selector
	// ("╭─ Models ─") never have that shape.
	ompComposerTopRe = regexp.MustCompile(`^\s*(?:╭──|\+--)\s+(\S.*)$`)
	// ompComposerBottomRe matches the composer's bottom border, which is also
	// the draft's last input row.
	ompComposerBottomRe = regexp.MustCompile(`^\s*(?:╰─|\+-)(.*?)(?:─╯|-\+)\s*$`)
	// Empty omp composers can show right-aligned shortcut hints in the bottom
	// border (GH#342). Match the shipped symbol presets and known labels only;
	// left-aligned text and continuation rows remain real draft input.
	ompComposerBottomHintRe = regexp.MustCompile(`[ \t]{2,}(?:(?:󰘶[ \t]+󰌒|⇧⇥|Shift\+Tab)[ \t]+to change thinking effort|(?:←[ \t]*←|Left[ \t]+Left)[ \t]+to see [1-9][0-9]* running agents?)[ \t]*$`)
	// ompComposerRowRe matches a bordered continuation row of the draft.
	ompComposerRowRe = regexp.MustCompile(`^\s*(?:│|\|)(.*?)(?:│|\|)\s*$`)
	// ompBusyStatusRe matches the in-flight status segment: one spinner frame
	// then the elapsed timer ("⠧ 1s", "\ 5s", "⠋ 1m").
	ompBusyStatusRe = regexp.MustCompile(`^\S\s+\d+[smh]\b`)
	// ompEscHintRe matches the activity line's interrupt hint in every symbol
	// preset, plus the trailing "⟨esc⟩" form earlier omp v18 builds rendered.
	ompEscHintRe = regexp.MustCompile(`^\s*(?:󱊷|⎋|esc)\s+\S|(?:⟨esc⟩|\[esc\])\s*$|^\s*[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏]\s*Working…`)
	// ompContextGaugeRe extracts the context gauge: the usage percentage and,
	// when shown, the window size that follows it separated only by rule
	// fill, dividers and icons ("────6%────┃────262K─", "6.0%/262K", and the
	// compressed "──69%󰁨262K─" a crowded status line renders). Requiring the
	// window to follow the percentage keeps a size-like token in the session
	// title ("… 10K rows") from being read as the window.
	ompContextGaugeRe = regexp.MustCompile(`(?:^|[\s─\-])(\d{1,3}(?:\.\d+)?)%(?:[^\p{L}\p{N}]*?(\d+(?:\.\d+)?)([KkMm])(?:[^\p{L}\p{N}]|$))?`)
	// ompPasteTokenRe matches a collapsed bracketed-paste token in the draft.
	ompPasteTokenRe = regexp.MustCompile(`(?:^|\s)#\d+(?:\s|$)`)
	// ompPastePreviewFooterRe matches the paste preview box's bottom edge.
	ompPastePreviewFooterRe = regexp.MustCompile(`^\s*(?:╰|\+) \+\d+ lines [─\-]`)
	// ompSteeringHeaderRe matches the pending-steering block header omp draws
	// above the activity line when a message is submitted mid-turn
	// (" Steering · 1", then "   1. <message>" rows and an edit hint).
	ompSteeringHeaderRe = regexp.MustCompile(`^\s*Steering\s+·\s+(\d+)\s*$`)
	// ompSubagentsHeaderRe matches the running-subagents panel header omp
	// draws in the HUD above the activity line while subagents run
	// (" Subagents", then "  └─ • <Name> ⟨<role>⟩ <task>…" rows).
	ompSubagentsHeaderRe = regexp.MustCompile(`^\s*Subagents\s*$`)
	// ompSubagentRowRe matches one running-subagent row under that header.
	ompSubagentRowRe = regexp.MustCompile(`^\s*(?:[├└]─|[|\\+` + "`" + `]-)\s+[•*]\s+\S`)
	// ompRuleRe matches the full-width horizontal rule that frames omp's
	// dismissable provider-error block.
	ompRuleRe = regexp.MustCompile(`^\s*(?:─{8,}|-{8,})\s*$`)
	// ompDismissedRe matches that block's footer line.
	ompDismissedRe = regexp.MustCompile(`^\s*Dismissed when you send your next message\.\s*$`)
	// ompRetryHintRe matches the "F5 to Retry" hint omp draws between a failed
	// or interrupted turn and the composer (nerd preset prefixes a glyph).
	ompRetryHintRe = regexp.MustCompile(`^\s*(?:\S\s+)?F5 to Retry\s*$`)
)

const (
	// ompComposerScanLines bounds how many trailing non-blank rows are searched
	// for the composer's bottom border. The box is bottom-pinned, but the
	// slash-command autocomplete list renders below it.
	ompComposerScanLines = 40
	// ompComposerMaxRows bounds the continuation rows walked between the
	// bottom and top borders (omp caps the editor at 18 rows).
	ompComposerMaxRows = 24
	// ompSteeringScanLines bounds how far above the composer the pending-
	// steering header is searched for (header + queued-message previews +
	// edit hint + activity line).
	ompSteeringScanLines = 16
	// ompSubagentsScanLines bounds how far above the composer the subagents
	// panel header is searched for (header + one row per running subagent +
	// steering block + activity line).
	ompSubagentsScanLines = 24
	// ompProviderErrorMaxRows bounds the error text walked between the
	// provider-error block's footer and its opening rule.
	ompProviderErrorMaxRows = 12
)

// OmpComposer is the parsed live composer box of an omp pane.
type OmpComposer struct {
	// Found: a structurally complete composer box (bottom border, bordered
	// rows, status-line top border) is visible near the bottom of the capture.
	Found bool
	// Busy: the top border carries the in-flight spinner + elapsed timer.
	Busy bool
	// EscHint: the activity line directly above the box carries the Esc
	// interrupt hint.
	EscHint bool
	// Draft is the unsubmitted composer text (rows joined by "\n"); "" when
	// the composer is empty.
	Draft string
	// PasteToken: the draft holds a collapsed bracketed-paste token ("#1")
	// with its preview box drawn above the composer.
	PasteToken bool
	// RowsBelow counts non-blank rows rendered under the box (the
	// slash-command autocomplete list, or a shell prompt after omp exited).
	RowsBelow int
	// Steering is the count from the pending-steering block ("Steering · N")
	// omp draws above the activity line after a message is submitted while a
	// turn runs; 0 when no steering is pending. Pending steering means omp
	// is busy and will act on it, so it is in-flight evidence too.
	Steering int
	// Subagents counts the rows of the running-subagents panel (" Subagents"
	// header, "└─ • <Name> …" rows) in the HUD above the composer; 0 when no
	// subagent is listed. Listed subagents are in-flight work.
	Subagents int
	// ProviderError is the first line of the dismissable provider-error block
	// ("server_error: ERROR", "401 Invalid API Key") framed by rules directly
	// above the composer, with any leading icon glyph removed; "" when no such
	// block is shown. omp clears it when the next message is sent.
	ProviderError string
	// StatusLine is the top border's text after the corner glyphs.
	StatusLine string
	// TopLine is the index (in the capture's "\n"-split lines) of the
	// composer's top border; ActivityLine is the index of the Esc-hint
	// activity line above it, or -1. Both are meaningful only when Found.
	TopLine      int
	ActivityLine int
}

// Working reports whether the composer shows an in-flight turn.
func (c OmpComposer) Working() bool {
	return c.Found && (c.Busy || c.EscHint || c.Steering > 0 || c.Subagents > 0)
}

// ParseOmpComposer locates and parses the bottom-most omp composer box.
func ParseOmpComposer(output string) OmpComposer {
	lines := strings.Split(stripANSICodes(output), "\n")
	nonBlankSeen := 0
	rowsBelow := 0
	for i := len(lines) - 1; i >= 0 && nonBlankSeen < ompComposerScanLines; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		nonBlankSeen++
		bottom := ompComposerBottomRe.FindStringSubmatch(lines[i])
		if bottom == nil {
			rowsBelow++
			continue
		}
		composer, ok := parseOmpComposerAt(lines, i, bottom[1])
		if !ok {
			rowsBelow++
			continue
		}
		composer.RowsBelow = rowsBelow
		return composer
	}
	return OmpComposer{}
}

// parseOmpComposerAt validates a candidate bottom border at lines[bottomIdx]
// by walking up through bordered rows to a status-line top border.
func parseOmpComposerAt(lines []string, bottomIdx int, bottomText string) (OmpComposer, bool) {
	bottomText = ompComposerBottomHintRe.ReplaceAllString(bottomText, "")
	draftRows := []string{strings.TrimSpace(bottomText)}
	for j := bottomIdx - 1; j >= 0 && bottomIdx-j <= ompComposerMaxRows+1; j-- {
		line := lines[j]
		if top := ompComposerTopRe.FindStringSubmatch(line); top != nil {
			status := strings.TrimSpace(top[1])
			composer := OmpComposer{
				Found:        true,
				Busy:         ompBusyStatusRe.MatchString(status),
				StatusLine:   status,
				TopLine:      j,
				ActivityLine: -1,
			}
			// Rows were collected bottom-up; restore reading order and drop
			// blank leading/trailing rows so an empty box reads as "".
			for l, r := 0, len(draftRows)-1; l < r; l, r = l+1, r-1 {
				draftRows[l], draftRows[r] = draftRows[r], draftRows[l]
			}
			composer.Draft = strings.TrimSpace(strings.Join(draftRows, "\n"))
			for k := j - 1; k >= 0 && j-k <= 2; k-- {
				above := lines[k]
				if strings.TrimSpace(above) == "" {
					continue
				}
				if ompEscHintRe.MatchString(above) {
					composer.EscHint = true
					composer.ActivityLine = k
				}
				break
			}
			for k := j - 1; k >= 0 && j-k <= ompSteeringScanLines; k-- {
				if m := ompSteeringHeaderRe.FindStringSubmatch(lines[k]); m != nil {
					if n, err := strconv.Atoi(m[1]); err == nil {
						composer.Steering = n
					}
					break
				}
			}
			for k := j - 1; k >= 0 && j-k <= ompSubagentsScanLines; k-- {
				if !ompSubagentsHeaderRe.MatchString(lines[k]) {
					continue
				}
				for r := k + 1; r < j && ompSubagentRowRe.MatchString(lines[r]); r++ {
					composer.Subagents++
				}
				break
			}
			composer.ProviderError = ompProviderErrorAbove(lines, j)
			if composer.Draft != "" && ompPasteTokenRe.MatchString(composer.Draft) {
				for k := j - 1; k >= 0 && j-k <= 2; k-- {
					if ompPastePreviewFooterRe.MatchString(lines[k]) {
						composer.PasteToken = true
						break
					}
				}
			}
			return composer, true
		}
		row := ompComposerRowRe.FindStringSubmatch(line)
		if row == nil {
			return OmpComposer{}, false
		}
		draftRows = append(draftRows, strings.TrimSpace(row[1]))
	}
	return OmpComposer{}, false
}

// ompProviderErrorAbove returns the summary line of a provider-error block
// whose closing rule sits directly above the composer top border at
// lines[top], or "" when there is none. Blank rows and omp's "F5 to Retry"
// hint (live swarm capture) may sit between the rule and the composer; any
// other row means the block is history. The block must be complete: closing
// rule, "Dismissed when …" footer, error text, opening rule.
func ompProviderErrorAbove(lines []string, top int) string {
	k := top - 1
	for k >= 0 && top-k <= 4 && (strings.TrimSpace(lines[k]) == "" || ompRetryHintRe.MatchString(lines[k])) {
		k--
	}
	if k < 1 || !ompRuleRe.MatchString(lines[k]) || !ompDismissedRe.MatchString(lines[k-1]) {
		return ""
	}
	summary := ""
	for r := k - 2; r >= 0 && k-2-r < ompProviderErrorMaxRows; r-- {
		if ompRuleRe.MatchString(lines[r]) {
			if summary == "" {
				summary = "provider error"
			}
			return summary
		}
		if text := strings.TrimLeftFunc(lines[r], func(c rune) bool {
			return !unicode.IsLetter(c) && !unicode.IsDigit(c)
		}); strings.TrimSpace(text) != "" {
			summary = strings.TrimSpace(text) // walking upward: the last kept is the first row
		}
	}
	return ""
}

// OmpRetryKey is omp's retry key: pressing F5 at the "F5 to Retry" hint
// re-runs the failed or interrupted turn (verified live, omp v18.2.3).
const OmpRetryKey = "F5"

// OmpProviderErrorRecovery is the recovery guidance for an omp provider error.
const OmpProviderErrorRecovery = "retryable: press " + OmpRetryKey + " (omp's retry key) to re-run the failed turn, " +
	"or send a continue prompt (omp dismisses the error on the next message)"

// OmpProviderError reports the summary of the dismissable provider-error
// block an omp pane shows after a failed turn ("server_error: ERROR"). It
// requires the block to sit directly above a quiet composer: once any
// in-flight signal returns the turn is running again and the pane is not in
// error. Sending any new message (a "continue" prompt) dismisses the block
// and retries, so the state is retryable rather than terminal.
func OmpProviderError(output string) (string, bool) {
	c := ParseOmpComposer(output)
	if !c.Found || c.Working() || c.ProviderError == "" {
		return "", false
	}
	return c.ProviderError, true
}

// OmpActivelyWorking reports whether an omp pane shows an in-flight turn: the
// composer's top border carries the spinner + elapsed timer, the activity
// line above it carries the Esc interrupt hint, steering is pending, or the
// HUD lists running subagents. paneWidth is accepted for
// parity with the other detectors; omp's chrome is anchored to the composer
// box, not to a width-dependent tail window.
func OmpActivelyWorking(output string, _ int) bool {
	return ParseOmpComposer(output).Working()
}

// OmpIdlePromptShowing reports whether an omp pane is waiting for input: its
// composer box is the bottom-most chrome and no in-flight signal is present.
// A draft in the box still counts as idle (the agent is not working); callers
// that care about unsubmitted text inspect the composer draft separately.
func OmpIdlePromptShowing(output string) bool {
	c := ParseOmpComposer(output)
	return c.Found && !c.Working() && c.RowsBelow == 0
}

// OmpContextUsage extracts the context gauge from the composer's status line:
// the used percentage and, when rendered, the context window in tokens.
func OmpContextUsage(output string) (usedPct float64, windowTokens int64, ok bool) {
	c := ParseOmpComposer(output)
	if !c.Found {
		return 0, 0, false
	}
	m := ompContextGaugeRe.FindStringSubmatch(c.StatusLine)
	if m == nil {
		return 0, 0, false
	}
	pct, err := strconv.ParseFloat(m[1], 64)
	if err != nil || pct < 0 || pct > 100 {
		return 0, 0, false
	}
	if m[2] != "" {
		if size, err := strconv.ParseFloat(m[2], 64); err == nil {
			multiplier := 1000.0
			if strings.EqualFold(m[3], "m") {
				multiplier = 1000000.0
			}
			windowTokens = int64(size * multiplier)
		}
	}
	return pct, windowTokens, true
}

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

// OpenCode (oc, https://opencode.ai) patterns (ntm#261). Derived from the
// TUI source (packages/tui/src/component/prompt/index.tsx): the composer
// hint text reads `Ask anything... "<example>"` and is drawn only while
// the prompt is empty; while a turn runs the TUI renders a spinner plus an
// `esc interrupt` (after one press: `esc again to interrupt`) footer hint
// that disappears at idle.
//
// These substring and hint patterns read an opencode pane whose prompt box
// the box reader below (opencodePromptBox, from live captures) does not
// find. When the box is drawn, the box reader decides alone: an earlier
// turn's error, or a reply that quotes a rate limit or the interrupt hint,
// stays in the transcript and must not change the pane's state.
var (
	ocRateLimitPatterns = []string{
		"rate limit",
		"too many requests",
		"quota exceeded",
		"usage limit",
	}

	ocWorkingPatterns = []string{
		"esc interrupt",
		"esc again to interrupt",
	}

	// ocInterruptHintRe matches the footer hint OpenCode shows only while a
	// turn is in flight.
	ocInterruptHintRe = regexp.MustCompile(`(?i)\besc\s+(?:again\s+to\s+)?interrupt\b`)

	// ocIdlePatterns indicates the empty composer is waiting for input.
	// Callers gate on !OpencodeActivelyWorking first (detectStateFlags does).
	ocIdlePatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\bAsk anything\b`),
		regexp.MustCompile(`(?m)^\s*Interrupted\s*$`), // post-interrupt acknowledgement
	}

	ocErrorPatterns = []string{
		"error:",
		"failed:",
		"exception:",
	}
)

// ocLiveTailLines bounds the live-tail window scanned for the OpenCode
// working veto; the footer sits at the bottom of the screen like grok's.
const ocLiveTailLines = 15

// ocIdleScanLines is the trailing window OpencodeIdlePromptShowing searches
// for ocIdlePatterns when no prompt box is drawn, the window the agent
// parser's detectIdle reads for most agents.
const ocIdleScanLines = 5

// Opencode (oc) screen, measured against opencode 2.0.28 in tmux with a
// stand-in provider streaming slow replies (testdata/oc_*.txt), and matching
// the opencode 1.18.32 screens captured for GH #333 (testdata/opencode_*.txt).
// Its prompt is a box with a "┃" left edge and a bottom edge of "╹" and a run
// of "▀". The box holds the input between blank lines, then a line naming
// the agent, model and provider; the footer line below it shows the cwd and
// key hints:
//
//	┃
//	┃  Ask anything… "Fix broken tests"
//	┃
//	┃  Build · Fake Model Standin
//	╹▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀
//	/…/repo:main  shift+tab agents  ctrl+p commands
//
// The dim "Ask anything…" hint shows only on the home screen, which
// centres the box under the logo with the version line at the bottom of the
// pane. A session pins the box to the bottom, and draws the user's messages
// and tool output in the transcript with the same "┃" edge but no bottom
// edge.
//
// The box stays drawn while a turn runs. The footer line then starts with a
// progress bar of "⬝" and "■" cells, followed by the interrupt hint:
//
//	⬝⬝⬝⬝■■■■ esc interrupt          1.3K (1%)  ctrl+p commands
//
// The hint reads "esc again to interrupt" after one esc and is cut to
// "esc...upt" in a narrow pane, but the bar is drawn at every width. When
// the turn ends the footer shows the cwd again. A question the agent asks,
// or a request for permission to run a tool, replaces the box and footer
// until it is answered.
//
// The transcript ends each turn with a header naming the agent and model,
// which gains the turn's duration and output rate when the turn ends. When a
// request fails opencode draws a notice above that header. It retries rate
// limits and server errors without end, warning while the turn runs:
//
//	⚠ Retrying in 8s · attempt 11 · Rate limit reached for fake-model in
//	organization org-standin on requests per min (RPM): Limit 3, Used 3,
//
//	Build · Fake Model
//
// and any other error ends the turn, with no output rate in the header:
//
//	Error: Incorrect API key provided: not-a-key.
//
//	Build · Fake Model · 54ms
const (
	// OpencodeWorkingFooterPattern matches the prompt box's bottom edge with
	// a live turn's progress bar on the footer line below it.
	OpencodeWorkingFooterPattern = `(?m)^[ \t]*╹▀+[ \t]*\n[ \t]*[⬝■]{3,}`

	// OpencodeEmptyPromptPattern matches the prompt box with nothing typed
	// in it: from the line above the box, which is not part of it, through
	// box lines that are empty or hold the home screen's hint, the
	// agent and model line, and the bottom edge, with at most three
	// non-blank lines below it (opencodeFooterScanLines).
	OpencodeEmptyPromptPattern = `(?m)(?:\A|^[ \t]*(?:[^┃ \t\n][^\n]*)?\n)(?:[ \t]*┃[ \t]*(?:Ask anything…[^\n]*)?\n)+[ \t]*┃[^\n]*\n[ \t]*╹▀+[ \t]*(?:\n[ \t]*)*(?:\n[^\n]*\S[^\n]*(?:\n[ \t]*)*){0,3}\z`
)

// opencodeFooterScanLines bounds how many non-blank lines up from the bottom
// opencodePromptBox looks for the box's bottom edge: the footer, and on the
// home screen the version line below it.
const opencodeFooterScanLines = 4

var (
	// opencodePromptBoxBottomRe matches the prompt box's bottom edge. The
	// logo's "▀" runs are broken by spaces and other blocks, so they never
	// match.
	opencodePromptBoxBottomRe = regexp.MustCompile(`^[ \t]*╹▀+[ \t]*$`)

	// opencodeProgressBarRe matches the start of the footer line while a
	// turn runs.
	opencodeProgressBarRe = regexp.MustCompile(`^[ \t]*[⬝■]{3,}`)

	// opencodeTurnHeaderRe matches a turn's header: the agent, then the
	// model and, once the turn ends, its duration and output rate.
	opencodeTurnHeaderRe = regexp.MustCompile(`^[^┃\s·]+ · \S`)

	// opencodeRetryNoticeRe matches the warning shown while opencode waits
	// to retry a failed request ("Retrying in 8s") or sends the retry
	// ("Retry due"), and captures the provider's message.
	opencodeRetryNoticeRe = regexp.MustCompile(`^⚠ Retry.*?\battempt \d+\s*·?\s*(.*)$`)

	// opencodeRateLimitRe matches a provider's message for a rate limit or
	// an exhausted quota.
	opencodeRateLimitRe = regexp.MustCompile(`(?i)rate.?limit|too many requests|\b429\b|quota|usage limit|resource.?exhausted`)
)

// OpencodeActivelyWorking reports whether an opencode pane shows a live
// turn. When the prompt box is drawn, the progress bar on the footer line
// below it decides and the transcript is never read, so tool output or prose
// that looks like a spinner, a progress bar or the interrupt hint does not
// count. Without the box, the `esc interrupt` footer hint in the trailing
// live window decides (ocInterruptHintRe). paneWidth is the real tmux pane
// width; pass 0 when unknown.
func OpencodeActivelyWorking(output string, paneWidth int) bool {
	clean := stripANSICodes(output)
	lines := strings.Split(clean, "\n")
	if bottom, ok := opencodePromptBox(lines); ok {
		return opencodeTurnShowing(lines, bottom)
	}
	tail := util.GetLastNLines(clean, util.WidthAdaptiveTailLines(paneWidth, ocLiveTailLines))
	return ocInterruptHintRe.MatchString(tail)
}

// OpencodeIdlePromptShowing reports whether an opencode pane is waiting for
// input with an empty prompt box and no live turn. A box holding a draft
// returns false, so a pane is never offered new work on top of unsent text.
// Without the box, the empty composer's hint text in the last lines, with no
// live interrupt hint, counts (ocIdlePatterns).
// status.DetectIdleFromOutput and the agent parser's detectIdle both call it.
func OpencodeIdlePromptShowing(output string) bool {
	clean := stripANSICodes(output)
	lines := strings.Split(clean, "\n")
	bottom, ok := opencodePromptBox(lines)
	if !ok {
		return !OpencodeActivelyWorking(output, 0) && matchAnyRegex(util.GetLastNLines(clean, ocIdleScanLines), ocIdlePatterns)
	}
	if opencodeTurnShowing(lines, bottom) {
		return false
	}
	top := bottom
	for top > 0 && strings.HasPrefix(strings.TrimSpace(lines[top-1]), "┃") {
		top--
	}
	// The input line and the agent and model line at least.
	if bottom-top < 2 {
		return false
	}
	// Once a session exists, a wide pane draws a sidebar (session title,
	// context, cwd) to the right of the box on the same rows, so only the
	// columns the bottom edge spans belong to the box (opencode 1.18.32,
	// GH #333).
	width := utf8.RuneCountInString(strings.TrimRight(lines[bottom], " \t"))
	// The box's last line names the agent and model; the lines above it
	// hold the input.
	for _, line := range lines[top : bottom-1] {
		if runes := []rune(line); len(runes) > width {
			line = string(runes[:width])
		}
		text := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "┃"))
		if text != "" && !strings.HasPrefix(text, "Ask anything…") {
			return false
		}
	}
	return true
}

// OpencodePromptShowing reports whether an opencode pane is waiting for
// input: its prompt box is drawn with no live turn, whether or not it holds
// a draft.
func OpencodePromptShowing(output string) bool {
	lines := strings.Split(stripANSICodes(output), "\n")
	bottom, ok := opencodePromptBox(lines)
	return ok && !opencodeTurnShowing(lines, bottom)
}

// opencodePromptBox returns the line index of the prompt box's bottom edge.
// ok is false when no box is drawn near the bottom of the screen.
func opencodePromptBox(lines []string) (bottom int, ok bool) {
	seen := 0
	for i := len(lines) - 1; i >= 0 && seen < opencodeFooterScanLines; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		seen++
		if opencodePromptBoxBottomRe.MatchString(lines[i]) {
			return i, true
		}
	}
	return 0, false
}

// opencodePromptBoxDrawn reports whether output ends with opencode's prompt
// box, the screen the box reader decides alone. Without it the agent parser
// falls back to the substring patterns (ocRateLimitPatterns,
// ocErrorPatterns).
func opencodePromptBoxDrawn(output string) bool {
	_, ok := opencodePromptBox(strings.Split(stripANSICodes(output), "\n"))
	return ok
}

// opencodeTurnShowing reports whether the footer line below the prompt box's
// bottom edge starts with the progress bar of a live turn.
func opencodeTurnShowing(lines []string, bottom int) bool {
	for _, line := range lines[bottom+1:] {
		if strings.TrimSpace(line) != "" {
			return opencodeProgressBarRe.MatchString(line)
		}
	}
	return false
}

// OpencodeRateLimited reports whether an opencode pane's last turn is held
// up by a rate limit or an exhausted quota: opencode is retrying a request
// the provider refused for that, or the refusal ended the turn. The agent
// parser's detectRateLimit calls it.
func OpencodeRateLimited(output string) bool {
	message, ok := opencodeFailureNotice(output)
	return ok && opencodeRateLimitRe.MatchString(message)
}

// OpencodeErrorShowing reports whether an opencode pane's last turn failed
// or is failing: opencode is retrying a failed request, or an error ended
// the turn. Notices above earlier turns, and a retry warning left above a
// reply that streamed once the retry went through, do not count. The agent
// parser's detectError calls it.
func OpencodeErrorShowing(output string) bool {
	_, ok := opencodeFailureNotice(output)
	return ok
}

// opencodeFailureNotice returns the provider's message from the notice
// directly above the last turn's header, which sits directly above the
// prompt box: a retry warning while the turn runs, or the error that ended
// it. ok is false when the box is not drawn or no such notice shows.
func opencodeFailureNotice(output string) (message string, ok bool) {
	lines := strings.Split(stripANSICodes(output), "\n")
	bottom, found := opencodePromptBox(lines)
	if !found {
		return "", false
	}
	top := bottom
	for top > 0 && strings.HasPrefix(strings.TrimSpace(lines[top-1]), "┃") {
		top--
	}
	header, start := opencodeBlockAbove(lines, top)
	notice, _ := opencodeBlockAbove(lines, start)
	if !opencodeTurnHeaderRe.MatchString(header) {
		return "", false
	}
	if opencodeTurnShowing(lines, bottom) {
		if m := opencodeRetryNoticeRe.FindStringSubmatch(notice); m != nil {
			return m[1], true
		}
		return "", false
	}
	// A reply's header ends with its output rate; a failed turn's does not,
	// which keeps a reply whose last paragraph starts with "Error:" from
	// reading as one.
	if strings.Contains(header, "tok/s") {
		return "", false
	}
	return strings.CutPrefix(notice, "Error: ")
}

// opencodeBlockAbove returns the run of non-blank lines nearest above line
// end, trimmed and joined with spaces to undo the transcript's wrapping, and
// the index of its first line.
func opencodeBlockAbove(lines []string, end int) (text string, start int) {
	for end > 0 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	start = end
	for start > 0 && strings.TrimSpace(lines[start-1]) != "" {
		start--
	}
	parts := make([]string, 0, end-start)
	for _, line := range lines[start:end] {
		parts = append(parts, strings.TrimSpace(line))
	}
	return strings.Join(parts, " "), start
}

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
//
// The live window is width-adaptive (bd-eeifh): the fixed line budget counts
// physical capture rows, so on narrow panes a wrapped spinner frame can fall
// outside it and a working pane reads idle. paneWidth is the real tmux pane
// width; pass 0 when unknown to keep the calibrated budget.
func DetectClaudeTurnState(output string, paneWidth int) ClaudeTurnState {
	if output == "" {
		return ClaudeTurnUnknown
	}
	clean := stripANSICodes(output)
	tail := util.GetLastNLines(clean, util.WidthAdaptiveTailLines(paneWidth, claudeLiveTailLines))
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
// live spinner rather than completion or a terminal tool-result error. The
// live window is width-adaptive (bd-eeifh); pass the real tmux pane width,
// or 0 when unknown.
func ClaudeActivelyWorking(output string, paneWidth int) bool {
	return DetectClaudeTurnState(output, paneWidth) == ClaudeTurnWorking
}

// CodexActivelyWorking reports whether Codex's trailing live window contains a
// structural in-flight marker. Codex leaves old spinner lines in scrollback and
// keeps its input chrome visible during work, so the bounded tail is required:
// matching the full capture would make completed turns appear busy forever,
// while treating the persistent chevron as idle would make active panes unsafe
// for dispatch.
//
// The live window is width-adaptive (bd-eeifh). paneWidth is the real tmux
// pane width; pass 0 when unknown to keep the calibrated budget.
func CodexActivelyWorking(output string, paneWidth int) bool {
	clean := stripANSICodes(output)
	tail := util.GetLastNLines(clean, util.WidthAdaptiveTailLines(paneWidth, codexLiveTailLines))
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

// readyWordRe matches "ready" as a whole word. A bare substring check also hit
// "already", so lines like "Already up to date." read as an agent coming up.
var readyWordRe = regexp.MustCompile(`\bready\b`)

// readyPromptPatterns are the prompts IsReadyOutput accepts, anchored to the
// end of a line or of the output. A shell's "$" or "%" prompt is not one: no
// agent's prompt looks like it, and in an agent pane it means the agent never
// started or has exited.
var readyPromptPatterns = []*regexp.Regexp{
	regexp.MustCompile(`❯\s*$`),      // Modern prompts (U+276F)
	regexp.MustCompile(`›\s*$`),      // Codex prompt (U+203A) empty
	regexp.MustCompile(`(?m)^›`),     // Codex prompt with hint text
	regexp.MustCompile(`>\s*$`),      // Simple prompt at end of output
	regexp.MustCompile(`(?m)^>\s*$`), // Simple prompt on its own line
}

// IsReadyOutput reports whether the output of a pane freshly launched as
// agentType shows the agent's TUI up and taking input. Robot spawn,
// restart-pane and checkpoint restore wait on it before sending an agent
// work. Callers still corroborate it with the pane's process, since a
// launcher can print anything before the agent starts.
func IsReadyOutput(output string, agentType AgentType) bool {
	switch agentType.Canonical() {
	case AgentTypePi:
		// Pi draws no prompt glyph: its prompt is the editor box above the
		// footer, the box its idle detection (PiIdlePromptShowing) reads
		// too. None of the indicators below appear on a Pi screen, apart
		// from an extension status that happens to say "ready".
		return PiEditorShowing(output)
	case AgentTypeOpencode:
		// opencode draws none either: its prompt is the box above its
		// footer, the box its idle detection (OpencodeIdlePromptShowing)
		// reads too. In a layout the box reader does not know, the empty
		// composer's "Ask anything" hint counts. The generic indicators
		// below would also fire on the launching shell's prompt or on the
		// "OpenCode" splash, before the composer exists.
		if opencodePromptBoxDrawn(output) {
			return OpencodePromptShowing(output)
		}
		return strings.Contains(strings.ToLower(output), "ask anything")
	case AgentTypeOmp:
		// omp paints nothing for ~1s after launch, then draws its banner and
		// its status-line composer box in the same frame. The box (not a
		// banner substring such as "Welcome back!") is the signal that a
		// typed prompt will land in the composer.
		return ParseOmpComposer(output).Found
	}

	lower := strings.ToLower(output)

	// Common ready indicators (case-insensitive)
	lowerPatterns := []string{
		"claude>",
		"claude >",
		"codex>",
		"openai codex",
		"context left",
		"gemini>",
		">>>", // Python REPL
		"waiting for input",
		"how can i help",
		// Claude Code TUI indicators
		"claude code v",      // Version banner
		"welcome back",       // Greeting
		"bypass permissions", // Status line
		"try \"",             // Example prompt
		// Grok Build TUI indicators (GH#251 phase 2, live grok 1.0.5
		// captures): the welcome banner and the bottom status line both
		// carry "Grok Build  1.0.5", and the composer's bottom border
		// carries "· always-approve".
		"grok build",
		"· always-approve",
	}

	for _, pattern := range lowerPatterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}

	// Muse Code's last line is its status bar, not a prompt, so the prompt
	// patterns below never match it; its banner or status bar means the TUI
	// is up and taking input.
	if IsMuseOutput(output) {
		return true
	}

	// "ready" needs a word boundary: as a bare substring it also matched
	// "already", so ordinary output such as "Already up to date." counted as an
	// agent coming up.
	if readyWordRe.MatchString(lower) {
		return true
	}

	for _, p := range readyPromptPatterns {
		if p.MatchString(output) {
			return true
		}
	}

	return false
}
