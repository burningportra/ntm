package robot

import (
	"strings"
	"testing"

	"github.com/Dicklesworthstone/ntm/internal/agent"
)

func TestNewPatternLibrary(t *testing.T) {
	lib := NewPatternLibrary()

	if lib.Version != "1.0" {
		t.Errorf("expected version 1.0, got %s", lib.Version)
	}

	if len(lib.Patterns) == 0 {
		t.Error("expected default patterns, got none")
	}

	// Verify patterns are compiled
	for _, p := range lib.Patterns {
		if p.Regex == nil && p.RegexStr != "" {
			t.Errorf("pattern %s not compiled", p.Name)
		}
	}
}

func TestPatternLibrary_Compile(t *testing.T) {
	lib := &PatternLibrary{
		Patterns: []Pattern{
			{Name: "test1", RegexStr: `hello`, Priority: 10},
			{Name: "test2", RegexStr: `world`, Priority: 20},
		},
	}

	err := lib.Compile()
	if err != nil {
		t.Fatalf("compile failed: %v", err)
	}

	// Check patterns are compiled
	for _, p := range lib.Patterns {
		if p.Regex == nil {
			t.Errorf("pattern %s not compiled", p.Name)
		}
	}

	// Check sorted by priority (higher first)
	if lib.Patterns[0].Priority < lib.Patterns[1].Priority {
		t.Error("patterns should be sorted by priority descending")
	}
}

func TestPatternLibrary_CompileError(t *testing.T) {
	lib := &PatternLibrary{
		Patterns: []Pattern{
			{Name: "invalid", RegexStr: `[invalid`, Priority: 10},
		},
	}

	err := lib.Compile()
	if err == nil {
		t.Error("expected compile error for invalid regex")
	}
}

func TestPatternLibrary_Match(t *testing.T) {
	lib := NewPatternLibrary()

	tests := []struct {
		name      string
		content   string
		agentType string
		wantMatch bool
		wantState AgentState
	}{
		{"claude_prompt", "claude>", "claude", true, StateWaiting},
		{"claude_prompt_with_space", "claude> ", "claude", true, StateWaiting},
		{"codex_prompt", "codex>", "codex", true, StateWaiting},
		{"codex_context_left", "47% context left · ? for shortcuts", "codex", true, StateWaiting},
		{"codex_chevron_prompt", "› Write tests for @filename", "codex", true, StateWaiting},
		{"codex_working_bullet", "• Working (4m 51s • esc to interrupt)", "codex", true, StateThinking},
		{"codex_working_middle_dot", "· Working on request", "codex", true, StateThinking},
		{"codex_esc_interrupt_only", "(15s · esc to interrupt)", "codex", true, StateThinking},
		{"codex_esc_interrupt_truncated_ellipsis", "• Waiting for background terminal (4m 45s • esc to i…)", "codex", true, StateThinking},
		{"codex_esc_interrupt_truncated_ascii_dots", "(3s · esc to int...)", "codex", true, StateThinking},
		{"codex_waiting_for_background", "• Waiting for background terminal (4m 45s • esc to i…)", "codex", true, StateThinking},
		{"codex_waiting_middle_dot", "· Waiting for background cargo run", "codex", true, StateThinking},
		{"codex_thinking_bullet", "• Thinking (3s)", "codex", true, StateThinking},
		{"gemini_prompt", "gemini>", "gemini", true, StateWaiting},
		{"rate_limit", "Rate limit exceeded", "*", true, StateError},
		{"http_429", "HTTP 429 Too Many Requests", "*", true, StateError},
		{"panic", "panic: runtime error", "*", true, StateError},
		{"thinking", "Thinking...", "*", true, StateThinking},
		{"done", "Done", "*", true, StateWaiting},
		{"no_match", "just some random text", "*", false, StateUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matches := lib.Match(tt.content, tt.agentType)

			if tt.wantMatch {
				if len(matches) == 0 {
					t.Errorf("expected match for %q, got none", tt.content)
					return
				}
				if matches[0].State != tt.wantState {
					t.Errorf("expected state %s, got %s", tt.wantState, matches[0].State)
				}
			} else {
				if len(matches) > 0 {
					t.Errorf("expected no match for %q, got %v", tt.content, matches)
				}
			}
		})
	}
}

// TestCodexEscInterruptDoesNotMatchProse guards the truncation-tolerant
// `codex_esc_interrupt` regex against accidentally swallowing unrelated
// text that happens to start with "esc to i". The regex uses \S* so it
// cannot span whitespace, but it's easy to break when tweaking — keep
// this test as an anti-regression tripwire.
func TestCodexEscInterruptDoesNotMatchProse(t *testing.T) {

	lib := NewPatternLibrary()

	falsePositives := []string{
		"esc to i made a mistake",                    // trailing prose
		"The agent will esc to init something later", // mid-sentence
		"escape to interrupt",                        // wrong prefix
	}

	for _, content := range falsePositives {
		content := content
		t.Run(content, func(t *testing.T) {
			for _, m := range lib.Match(content, "codex") {
				if m.Pattern == "codex_esc_interrupt" {
					t.Errorf("codex_esc_interrupt matched false positive %q", content)
				}
			}
		})
	}
}

// TestCodexWorkingPatternsDoNotLeakToOtherAgents confirms the new
// codex thinking patterns are agent-scoped so they don't accidentally
// fire on claude / gemini panes whose scrollback happens to contain
// words like "Working" or "Waiting".
func TestCodexWorkingPatternsDoNotLeakToOtherAgents(t *testing.T) {

	lib := NewPatternLibrary()

	content := "• Working (4m 51s • esc to interrupt)"
	for _, agent := range []string{"claude", "gemini"} {
		agent := agent
		t.Run(agent, func(t *testing.T) {
			for _, m := range lib.Match(content, agent) {
				if m.Pattern == "codex_working" || m.Pattern == "codex_esc_interrupt" || m.Pattern == "codex_waiting_background" {
					t.Errorf("%s leaked to %s agent on content %q", m.Pattern, agent, content)
				}
			}
		})
	}
}

func TestPatternLibrary_MatchFirst(t *testing.T) {
	lib := NewPatternLibrary()

	// Test with matching content
	match := lib.MatchFirst("panic: something went wrong", "*")
	if match == nil {
		t.Fatal("expected match, got nil")
	}
	if match.State != StateError {
		t.Errorf("expected ERROR state, got %s", match.State)
	}

	// Test with non-matching content
	match = lib.MatchFirst("completely ordinary text", "*")
	if match != nil {
		t.Errorf("expected nil for non-matching content, got %v", match)
	}
}

func TestPatternLibrary_MatchByCategory(t *testing.T) {
	lib := NewPatternLibrary()

	// Test error category
	errorMatches := lib.MatchByCategory("Rate limit error occurred", "*", CategoryError)
	if len(errorMatches) == 0 {
		t.Error("expected error matches")
	}
	for _, m := range errorMatches {
		if m.Category != CategoryError {
			t.Errorf("expected error category, got %s", m.Category)
		}
	}

	// Test idle category
	idleMatches := lib.MatchByCategory("claude>", "claude", CategoryIdle)
	if len(idleMatches) == 0 {
		t.Error("expected idle matches")
	}
}

func TestPatternLibrary_AgentFiltering(t *testing.T) {
	lib := NewPatternLibrary()

	// Claude-specific pattern should match for claude
	matches := lib.Match("claude>", "claude")
	found := false
	for _, m := range matches {
		if m.Pattern == "claude_prompt" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected claude_prompt match for claude agent")
	}

	// Claude-specific pattern should NOT match for codex
	matches = lib.Match("claude>", "codex")
	for _, m := range matches {
		if m.Pattern == "claude_prompt" {
			t.Error("claude_prompt should not match for codex agent")
		}
	}

	// Wildcard patterns should match for any agent
	matches = lib.Match("panic: error", "any")
	if len(matches) == 0 {
		t.Error("wildcard patterns should match any agent")
	}
}

func TestPatternLibrary_HasError(t *testing.T) {
	lib := NewPatternLibrary()

	tests := []struct {
		content string
		hasErr  bool
	}{
		{"Rate limit exceeded", true},
		{"HTTP 429 error", true},
		{"panic: crash", true},
		{"unauthorized access", true},
		{"everything is fine", false},
		{"completed successfully", false},
	}

	for _, tt := range tests {
		t.Run(tt.content, func(t *testing.T) {
			if got := lib.HasError(tt.content, "*"); got != tt.hasErr {
				t.Errorf("HasError(%q) = %v, want %v", tt.content, got, tt.hasErr)
			}
		})
	}
}

func TestPatternLibrary_HasIdlePrompt(t *testing.T) {
	lib := NewPatternLibrary()

	tests := []struct {
		content   string
		agentType string
		hasIdle   bool
	}{
		{"claude>", "claude", true},
		{"codex>", "codex", true},
		{"42% context left · ? for shortcuts", "codex", true},
		{"› Write tests for @filename", "codex", true},
		{"gemini>", "gemini", true},
		{"$", "*", true},
		{">", "*", true},
		{"running some code", "*", false},
	}

	for _, tt := range tests {
		t.Run(tt.content, func(t *testing.T) {
			if got := lib.HasIdlePrompt(tt.content, tt.agentType); got != tt.hasIdle {
				t.Errorf("HasIdlePrompt(%q, %q) = %v, want %v", tt.content, tt.agentType, got, tt.hasIdle)
			}
		})
	}
}

func TestPatternLibrary_HasThinkingIndicator(t *testing.T) {
	lib := NewPatternLibrary()

	tests := []struct {
		content  string
		hasThink bool
	}{
		{"Thinking...", true},
		{"Processing...", true},
		{"Analyzing...", true},
		{"⠋", true}, // braille spinner
		{"Loading...", true},
		{"done", false},
		{"error occurred", false},
	}

	for _, tt := range tests {
		t.Run(tt.content, func(t *testing.T) {
			if got := lib.HasThinkingIndicator(tt.content, "*"); got != tt.hasThink {
				t.Errorf("HasThinkingIndicator(%q) = %v, want %v", tt.content, got, tt.hasThink)
			}
		})
	}
}

func TestPatternLibrary_HasCompletionSignal(t *testing.T) {
	lib := NewPatternLibrary()

	tests := []struct {
		content  string
		hasCompl bool
	}{
		{"Done", true},
		{"Completed", true},
		{"Finished!", true},
		{"✓", true},
		{"Summary:", true},
		{"still working", false},
	}

	for _, tt := range tests {
		t.Run(tt.content, func(t *testing.T) {
			if got := lib.HasCompletionSignal(tt.content, "*"); got != tt.hasCompl {
				t.Errorf("HasCompletionSignal(%q) = %v, want %v", tt.content, got, tt.hasCompl)
			}
		})
	}
}

func TestPatternLibrary_AddPattern(t *testing.T) {
	lib := NewPatternLibrary()
	initialCount := lib.PatternCount()

	err := lib.AddPattern(Pattern{
		Name:     "custom_pattern",
		RegexStr: `custom\s+test`,
		Agent:    "*",
		State:    StateWaiting,
		Category: CategoryCompletion,
		Priority: 999, // Very high priority
	})

	if err != nil {
		t.Fatalf("AddPattern failed: %v", err)
	}

	if lib.PatternCount() != initialCount+1 {
		t.Errorf("expected %d patterns, got %d", initialCount+1, lib.PatternCount())
	}

	// New pattern should be first (highest priority)
	patterns := lib.GetPatterns()
	if patterns[0].Name != "custom_pattern" {
		t.Error("new high-priority pattern should be first")
	}

	// Test that the new pattern matches
	matches := lib.Match("custom test", "*")
	found := false
	for _, m := range matches {
		if m.Pattern == "custom_pattern" {
			found = true
			break
		}
	}
	if !found {
		t.Error("custom pattern should match")
	}
}

func TestPatternLibrary_AddPatternInvalidRegex(t *testing.T) {
	lib := NewPatternLibrary()

	err := lib.AddPattern(Pattern{
		Name:     "invalid",
		RegexStr: `[invalid`,
		Agent:    "*",
		State:    StateError,
	})

	if err == nil {
		t.Error("expected error for invalid regex")
	}
}

func TestPatternLibrary_GetPatterns(t *testing.T) {
	lib := NewPatternLibrary()

	patterns := lib.GetPatterns()
	if len(patterns) == 0 {
		t.Error("expected patterns, got none")
	}

	// Modifying the copy shouldn't affect original
	originalCount := lib.PatternCount()
	_ = patterns[:1] // Truncating the copy; verify it doesn't affect original
	if lib.PatternCount() != originalCount {
		t.Error("GetPatterns should return a copy")
	}
}

func TestPatternLibrary_GetPatternsByCategory(t *testing.T) {
	lib := NewPatternLibrary()

	errorPatterns := lib.GetPatternsByCategory(CategoryError)
	if len(errorPatterns) == 0 {
		t.Error("expected error patterns")
	}

	for _, p := range errorPatterns {
		if p.Category != CategoryError {
			t.Errorf("expected error category, got %s", p.Category)
		}
	}
}

func TestPatternLibrary_GetPatternsByAgent(t *testing.T) {
	lib := NewPatternLibrary()

	claudePatterns := lib.GetPatternsByAgent("claude")
	if len(claudePatterns) == 0 {
		t.Error("expected claude patterns")
	}

	// Should include both claude-specific and wildcard patterns
	hasClaudeSpecific := false
	hasWildcard := false
	for _, p := range claudePatterns {
		if p.Agent == "claude" {
			hasClaudeSpecific = true
		}
		if p.Agent == "*" {
			hasWildcard = true
		}
	}

	if !hasClaudeSpecific {
		t.Error("expected claude-specific patterns")
	}
	if !hasWildcard {
		t.Error("expected wildcard patterns")
	}
}

func TestDefaultLibrary(t *testing.T) {
	// DefaultLibrary should be initialized
	if DefaultLibrary == nil {
		t.Fatal("DefaultLibrary should not be nil")
	}

	if DefaultLibrary.PatternCount() == 0 {
		t.Error("DefaultLibrary should have patterns")
	}
}

func TestMatchPatterns(t *testing.T) {
	// Test convenience function
	matches := MatchPatterns("claude>", "claude")
	if len(matches) == 0 {
		t.Error("expected matches from convenience function")
	}
}

func TestMatchFirstPattern(t *testing.T) {
	// Test convenience function
	match := MatchFirstPattern("panic: error", "*")
	if match == nil {
		t.Error("expected match from convenience function")
	}
}

func TestHasErrorPattern(t *testing.T) {
	if !HasErrorPattern("Rate limit exceeded", "*") {
		t.Error("should detect rate limit error")
	}
	if HasErrorPattern("everything is fine", "*") {
		t.Error("should not detect error in normal text")
	}
}

func TestHasIdlePattern(t *testing.T) {
	if !HasIdlePattern("claude>", "claude") {
		t.Error("should detect claude idle prompt")
	}
	if HasIdlePattern("$ ", "claude") {
		t.Error("shell prompt should not look idle for claude panes")
	}
	if HasIdlePattern("$ ", "codex") {
		t.Error("shell prompt should not look idle for codex panes")
	}
	if !HasIdlePattern("$ ", "user") {
		t.Error("shell prompt should still look idle for user panes")
	}
}

func TestHasThinkingPattern(t *testing.T) {
	if !HasThinkingPattern("Thinking...", "*") {
		t.Error("should detect thinking pattern")
	}
}

func TestPatternPriority(t *testing.T) {
	lib := NewPatternLibrary()

	// Test that patterns are sorted by priority
	patterns := lib.GetPatterns()
	for i := 1; i < len(patterns); i++ {
		if patterns[i-1].Priority < patterns[i].Priority {
			t.Errorf("patterns not sorted by priority at index %d", i)
		}
	}
}

// =============================================================================
// Edge Case Tests for Pattern Library
// =============================================================================

func TestPatternStruct(t *testing.T) {

	p := Pattern{
		Name:        "test_pattern",
		RegexStr:    `test\s+pattern`,
		Agent:       "claude",
		State:       StateWaiting,
		Category:    CategoryIdle,
		Priority:    100,
		Description: "Test pattern description",
	}

	if p.Name != "test_pattern" {
		t.Errorf("expected name test_pattern, got %s", p.Name)
	}
	if p.Agent != "claude" {
		t.Errorf("expected agent claude, got %s", p.Agent)
	}
	if p.State != StateWaiting {
		t.Errorf("expected state WAITING, got %s", p.State)
	}
	if p.Priority != 100 {
		t.Errorf("expected priority 100, got %d", p.Priority)
	}
}

func TestPatternMatchStruct(t *testing.T) {

	pm := PatternMatch{
		Pattern:  "claude_prompt",
		State:    StateWaiting,
		Category: CategoryIdle,
		Priority: 100,
	}

	if pm.Pattern != "claude_prompt" {
		t.Errorf("expected pattern claude_prompt, got %s", pm.Pattern)
	}
	if pm.State != StateWaiting {
		t.Errorf("expected state WAITING, got %s", pm.State)
	}
	if pm.Category != CategoryIdle {
		t.Errorf("expected category idle, got %s", pm.Category)
	}
}

func TestPatternLibraryVersion(t *testing.T) {

	lib := NewPatternLibrary()

	if lib.Version == "" {
		t.Error("expected version to be set")
	}
	if lib.Version != "1.0" {
		t.Errorf("expected version 1.0, got %s", lib.Version)
	}
}

func TestPatternLibraryCompileIdempotent(t *testing.T) {

	lib := NewPatternLibrary()
	initialCount := lib.PatternCount()

	// Compile again should be safe
	err := lib.Compile()
	if err != nil {
		t.Fatalf("second compile failed: %v", err)
	}

	if lib.PatternCount() != initialCount {
		t.Errorf("pattern count changed after recompile")
	}
}

func TestPatternLibraryMatchEmptyContent(t *testing.T) {

	lib := NewPatternLibrary()

	matches := lib.Match("", "claude")
	if len(matches) != 0 {
		t.Errorf("expected no matches for empty content, got %d", len(matches))
	}
}

func TestPatternLibraryMatchEmptyAgentType(t *testing.T) {

	lib := NewPatternLibrary()

	// Empty agent type should still match wildcard patterns
	matches := lib.Match("panic: error", "")
	if len(matches) == 0 {
		t.Error("expected matches for wildcard patterns with empty agent type")
	}
}

func TestPatternLibraryMatchAllWildcardPatterns(t *testing.T) {

	lib := NewPatternLibrary()

	// Count wildcard patterns
	wildcardPatterns := lib.GetPatternsByAgent("*")
	if len(wildcardPatterns) == 0 {
		t.Error("expected some wildcard patterns")
	}

	// All these should be wildcards
	for _, p := range wildcardPatterns {
		if p.Agent != "*" && p.Agent != "" {
			t.Errorf("expected wildcard agent, got %s", p.Agent)
		}
	}
}

func TestPatternLibraryAllAgentTypes(t *testing.T) {

	lib := NewPatternLibrary()

	agents := []string{"claude", "codex", "gemini"}
	for _, agent := range agents {
		patterns := lib.GetPatternsByAgent(agent)
		if len(patterns) == 0 {
			t.Errorf("expected patterns for agent %s", agent)
		}
	}
}

func TestPatternLibraryAllCategories(t *testing.T) {

	lib := NewPatternLibrary()

	categories := []PatternCategory{CategoryIdle, CategoryError, CategoryThinking, CategoryCompletion}
	for _, cat := range categories {
		patterns := lib.GetPatternsByCategory(cat)
		if len(patterns) == 0 {
			t.Errorf("expected patterns for category %s", cat)
		}
	}
}

func TestPatternLibraryBrailleSpinner(t *testing.T) {

	lib := NewPatternLibrary()

	// Test all braille spinner characters
	brailleChars := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	for _, char := range brailleChars {
		if !lib.HasThinkingIndicator(char, "*") {
			t.Errorf("expected braille char %s to match thinking pattern", char)
		}
	}
}

func TestPatternLibraryCheckmarkSymbols(t *testing.T) {

	lib := NewPatternLibrary()

	// Test checkmark symbols
	checkmarks := []string{"✓", "✔"}
	for _, mark := range checkmarks {
		if !lib.HasCompletionSignal(mark, "*") {
			t.Errorf("expected checkmark %s to match completion pattern", mark)
		}
	}
}

func TestPatternLibraryErrorPatternVariants(t *testing.T) {

	lib := NewPatternLibrary()

	errorTexts := []string{
		"Rate limit exceeded",
		"rate limit",
		"429",
		"HTTP 429",
		"too many requests",
		"quota exceeded",
		"api error: something went wrong",
		"Error: failed",
		"exception: null pointer",
		"panic: runtime error",
		"SIGSEGV",
		"Killed",
		"SIGKILL",
		"process exited",
		"agent terminated",
		"unauthorized",
		"invalid api key",
		"authentication failed",
		"connection refused",
		"request timed out",
		"connection timeout",
		"network error",
	}

	for _, text := range errorTexts {
		if !lib.HasError(text, "*") {
			t.Errorf("expected error detection for %q", text)
		}
	}
}

func TestPatternLibraryIdlePromptVariants(t *testing.T) {

	lib := NewPatternLibrary()

	idlePrompts := map[string]string{
		"claude>":      "claude",
		"claude> ":     "claude",
		"Claude>":      "claude",
		"claude code>": "claude",
		"╰─>":          "claude",
		"codex>":       "codex",
		"Codex> ":      "codex",
		"gemini>":      "gemini",
		"Gemini> ":     "gemini",
		">>>":          "gemini",
		">>> ":         "gemini",
		"$":            "*",
		"$ ":           "user",
		">":            "*",
		"#":            "*",
		"%":            "*",
	}

	for prompt, agent := range idlePrompts {
		if !lib.HasIdlePrompt(prompt, agent) {
			t.Errorf("expected idle prompt detection for %q (agent: %s)", prompt, agent)
		}
	}

	notIdleForKnownAgents := map[string]string{
		"$":  "codex",
		"$ ": "claude",
		"#":  "gemini",
	}

	for prompt, agent := range notIdleForKnownAgents {
		if lib.HasIdlePrompt(prompt, agent) {
			t.Errorf("did not expect generic shell prompt %q to look idle for %s", prompt, agent)
		}
	}
}

func TestPatternLibraryThinkingVariants(t *testing.T) {

	lib := NewPatternLibrary()

	thinkingTexts := []string{
		"Thinking...",
		"thinking",
		"Processing...",
		"processing",
		"Analyzing...",
		"analyzing",
		"Loading...",
		"loading",
		"please wait",
		"waiting...",
		"thinking deeply",
		"extended thinking",
		"...",
	}

	for _, text := range thinkingTexts {
		if !lib.HasThinkingIndicator(text, "*") {
			t.Errorf("expected thinking detection for %q", text)
		}
	}
}

func TestPatternLibraryCompletionVariants(t *testing.T) {

	lib := NewPatternLibrary()

	completionTexts := []string{
		"Done",
		"done",
		"Done!",
		"Completed",
		"completed",
		"Finished",
		"Finished!",
		"Summary:",
		"Changes made:",
	}

	for _, text := range completionTexts {
		if !lib.HasCompletionSignal(text, "*") {
			t.Errorf("expected completion detection for %q", text)
		}
	}
}

func TestPatternLibraryAddPatternSorting(t *testing.T) {

	lib := NewPatternLibrary()

	// Add a low priority pattern
	err := lib.AddPattern(Pattern{
		Name:     "low_priority",
		RegexStr: `low_priority_test`,
		Agent:    "*",
		State:    StateWaiting,
		Priority: 1,
	})
	if err != nil {
		t.Fatalf("AddPattern failed: %v", err)
	}

	// It should be near the end (low priority)
	patterns := lib.GetPatterns()
	lastPattern := patterns[len(patterns)-1]
	if lastPattern.Name != "low_priority" {
		t.Errorf("low priority pattern should be last, got %s", lastPattern.Name)
	}
}

func TestPatternLibraryAddPatternUncompiled(t *testing.T) {

	lib := NewPatternLibrary()
	initialCount := lib.PatternCount()

	// Add pattern without pre-compiled Regex (the normal case)
	// AddPattern will compile the RegexStr for us
	newPattern := Pattern{
		Name:     "uncompiled",
		RegexStr: `uncompiled_test`,
		Agent:    "*",
		State:    StateWaiting,
		Priority: 50,
	}

	err := lib.AddPattern(newPattern)
	if err != nil {
		t.Fatalf("AddPattern failed: %v", err)
	}

	if lib.PatternCount() != initialCount+1 {
		t.Errorf("expected %d patterns after add", initialCount+1)
	}
}

func TestPatternLibraryMatchFirstNoMatch(t *testing.T) {

	lib := NewPatternLibrary()

	match := lib.MatchFirst("completely random text with no patterns 12345", "*")
	if match != nil {
		t.Errorf("expected no match, got %v", match)
	}
}

func TestPatternLibraryMatchByCategoryNoMatch(t *testing.T) {

	lib := NewPatternLibrary()

	matches := lib.MatchByCategory("normal text", "*", CategoryError)
	if len(matches) != 0 {
		t.Errorf("expected no error matches for normal text, got %d", len(matches))
	}
}

func TestPatternLibraryMultipleMatches(t *testing.T) {

	lib := NewPatternLibrary()

	// Content that could match multiple patterns
	content := "Rate limit 429 error"
	matches := lib.Match(content, "*")

	if len(matches) < 2 {
		t.Errorf("expected multiple matches for %q, got %d", content, len(matches))
	}

	// All should be error category
	for _, m := range matches {
		if m.Category != CategoryError {
			t.Errorf("expected error category, got %s", m.Category)
		}
	}
}

func TestPatternLibraryPriorityOrdering(t *testing.T) {

	lib := NewPatternLibrary()

	// Content that matches both error and idle patterns
	// Error should have higher priority
	matches := lib.Match("panic: error >", "*")

	if len(matches) == 0 {
		t.Fatal("expected matches")
	}

	// First match should be error (higher priority)
	if matches[0].Category != CategoryError {
		t.Errorf("expected error to have highest priority, got %s", matches[0].Category)
	}
}

func TestDefaultLibraryNotNil(t *testing.T) {

	if DefaultLibrary == nil {
		t.Fatal("DefaultLibrary should not be nil")
	}
	if DefaultLibrary.PatternCount() == 0 {
		t.Error("DefaultLibrary should have patterns")
	}
}

func TestPatternLibraryConcurrentAccess(t *testing.T) {

	lib := NewPatternLibrary()

	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func() {
			_ = lib.Match("testing concurrent access", "*")
			_ = lib.HasError("error test", "*")
			_ = lib.HasIdlePrompt("claude>", "claude")
			_ = lib.GetPatterns()
			done <- true
		}()
	}

	for i := 0; i < 10; i++ {
		<-done
	}
}

func TestPatternLibraryAddPatternConcurrent(t *testing.T) {

	lib := NewPatternLibrary()

	done := make(chan bool)
	for i := 0; i < 5; i++ {
		go func(idx int) {
			_ = lib.AddPattern(Pattern{
				Name:     "concurrent_" + string(rune('0'+idx)),
				RegexStr: "concurrent_test_" + string(rune('0'+idx)),
				Agent:    "*",
				State:    StateWaiting,
				Priority: 10,
			})
			done <- true
		}(i)
	}

	for i := 0; i < 5; i++ {
		<-done
	}

	// All patterns should be added
	patterns := lib.GetPatterns()
	addedCount := 0
	for _, p := range patterns {
		if len(p.Name) > 10 && p.Name[:10] == "concurrent" {
			addedCount++
		}
	}
	if addedCount < 5 {
		t.Errorf("expected 5 concurrent patterns, got %d", addedCount)
	}
}

func TestPatternLibraryEmptyRegex(t *testing.T) {

	lib := &PatternLibrary{
		Patterns: []Pattern{
			{Name: "no_regex", RegexStr: "", Priority: 10},
		},
	}

	err := lib.Compile()
	if err != nil {
		t.Fatalf("compile with empty regex should not error: %v", err)
	}

	// Empty regex pattern should never match
	matches := lib.Match("any content", "*")
	if len(matches) != 0 {
		t.Error("empty regex should not match")
	}
}

func TestPatternLibraryGetPatternsByAgentEmpty(t *testing.T) {

	lib := NewPatternLibrary()

	// Non-existent agent should still get wildcard patterns
	patterns := lib.GetPatternsByAgent("nonexistent")
	wildcardCount := 0
	for _, p := range patterns {
		if p.Agent == "*" || p.Agent == "" {
			wildcardCount++
		}
	}
	if wildcardCount == 0 {
		t.Error("should include wildcard patterns for unknown agent")
	}
}

func TestMatchFirstPattern_NormalizesAgentAliases(t *testing.T) {

	tests := []struct {
		name        string
		agentType   string
		content     string
		wantPattern string
	}{
		{name: "claude short alias", agentType: "cc", content: "Welcome back", wantPattern: "claude_welcome"},
		{name: "claude long alias", agentType: "claude_code", content: "Claude Code v1.2.3", wantPattern: "claude_code_version"},
		{name: "codex short alias", agentType: "cod", content: "12% context left", wantPattern: "codex_context_left"},
		{name: "codex provider alias", agentType: "openai-codex", content: "12% context left", wantPattern: "codex_context_left"},
		{name: "gemini short alias", agentType: "gmi", content: "gemini>", wantPattern: "gemini_prompt"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			match := MatchFirstPattern(tt.content, tt.agentType)
			if match == nil {
				t.Fatalf("MatchFirstPattern(%q, %q) = nil, want %q", tt.content, tt.agentType, tt.wantPattern)
			}
			if match.Pattern != tt.wantPattern {
				t.Fatalf("MatchFirstPattern(%q, %q) pattern = %q, want %q", tt.content, tt.agentType, match.Pattern, tt.wantPattern)
			}
		})
	}
}

func TestGetPatternsByAgent_NormalizesAlias(t *testing.T) {

	patterns := DefaultLibrary.GetPatternsByAgent("cc")
	for _, pattern := range patterns {
		if pattern.Name == "claude_welcome" {
			return
		}
	}

	t.Fatal("GetPatternsByAgent(\"cc\") did not include claude-specific patterns")
}

// Antigravity workspace-trust dialog detection (GH#230 / ntm-jhd6): a pane
// parked on the first-run trust menu must classify as ERROR, not idle/OK.
func TestAgyTrustPromptClassifiesAsError(t *testing.T) {
	dialog := "Welcome to Antigravity\n" +
		"Do you trust the contents of this project?\n" +
		"  > Yes, I trust this folder\n" +
		"    No, browse in restricted mode\n"

	for _, agentType := range []string{"agy", "antigravity"} {
		matches := DefaultLibrary.Match(dialog, agentType)
		foundError := false
		for _, m := range matches {
			if m.Category == CategoryError && m.State == StateError {
				foundError = true
			}
		}
		if !foundError {
			t.Errorf("trust dialog did not classify as error for agent type %q (matches: %+v)", agentType, matches)
		}
	}

	// The dialog patterns must not fire for other agent types...
	for _, m := range DefaultLibrary.Match(dialog, "claude") {
		if m.Pattern == "agy_trust_prompt" || m.Pattern == "agy_trust_option" {
			t.Errorf("agy trust pattern leaked to claude panes: %+v", m)
		}
	}
	// ...and a normal agy working screen must not match them.
	for _, m := range DefaultLibrary.Match("Analyzing repository structure...\n", "antigravity") {
		if m.Pattern == "agy_trust_prompt" || m.Pattern == "agy_trust_option" {
			t.Errorf("trust pattern matched normal agy output: %+v", m)
		}
	}
}

// TestMusePatterns_ComposerIdleUnlessWorkingLineLive covers Muse's screen
// shape: the "❯" composer is drawn for the whole turn, so only the live
// "(… esc to interrupt)" working line separates a busy pane from an idle one.
func TestMusePatterns_ComposerIdleUnlessWorkingLineLive(t *testing.T) {
	lib := NewPatternLibrary()

	for name, screen := range map[string]string{
		"idle":        museTestIdleScreen,
		"finished":    museTestFinishedScreen,
		"interrupted": museTestInterruptedScreen,
	} {
		// The interrupted screen's composer holds the restored prompt as a
		// draft, so it is not idle (TestMusePatterns_DraftAgreesWithMuseIdlePromptShowing).
		if wantIdle := name != "interrupted"; lib.HasIdlePrompt(screen, "muse") != wantIdle {
			t.Errorf("%s Muse screen: HasIdlePrompt = %v, want %v", name, !wantIdle, wantIdle)
		}
		if lib.HasThinkingIndicator(screen, "muse") {
			t.Errorf("%s Muse screen matched a thinking pattern", name)
		}
		if IsLiveBusy(screen, "muse") {
			t.Errorf("IsLiveBusy(%s Muse screen) = true, want false", name)
		}
		if lib.HasError(screen, "muse") {
			t.Errorf("%s Muse screen matched an error pattern", name)
		}
	}

	if !lib.HasThinkingIndicator(museTestWorkingScreen, "muse") {
		t.Fatal("working Muse screen has no thinking pattern match")
	}
	if !IsLiveBusy(museTestWorkingScreen, "muse-code") {
		t.Fatal("IsLiveBusy(working Muse screen) = false, want true")
	}
	if got := lib.MatchFirst(museTestWorkingScreen, "muse"); got == nil || got.Pattern != "muse_working" {
		t.Fatalf("MatchFirst(working Muse screen) = %+v, want muse_working ahead of the composer", got)
	}
}

func TestMusePatterns_ClassifierStates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		screen string
		want   AgentState
	}{
		{name: "idle", screen: museTestIdleScreen, want: StateWaiting},
		{name: "working", screen: museTestWorkingScreen, want: StateThinking},
		{name: "trust prompt", screen: museTestTrustPromptScreen, want: StateError},
		{name: "idle composer showing a tip, banner scrolled away", screen: museTestTipScreen, want: StateWaiting},
		{name: "working above a tip composer", screen: museTestTipWorkingScreen, want: StateThinking},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := NewStateClassifier("%1", &ClassifierConfig{AgentType: "muse", HysteresisDuration: 0})
			if _, err := sc.ClassifyWithOutput(tc.screen); err != nil {
				t.Fatalf("baseline: %v", err)
			}
			activity, err := sc.ClassifyWithOutput(tc.screen)
			if err != nil {
				t.Fatalf("ClassifyWithOutput: %v", err)
			}
			if activity.State != tc.want {
				t.Fatalf("State = %q, want %q (patterns %v)", activity.State, tc.want, activity.DetectedPatterns)
			}
		})
	}
}

// TestMusePatterns_DraftAgreesWithMuseIdlePromptShowing pins the robot
// verdicts for a Muse composer holding a draft to agent.MuseIdlePromptShowing,
// which the parser and status detectors use. The muse_banner idle pattern
// matched the banner anywhere in the capture, so the same drafted pane read
// idle while the banner was on screen and active once it scrolled off.
func TestMusePatterns_DraftAgreesWithMuseIdlePromptShowing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		screen string
		idle   bool
	}{
		{name: "empty composer under the banner", screen: museTestIdleScreen, idle: true},
		{name: "finished turn under the banner", screen: museTestFinishedScreen, idle: true},
		{name: "tip composer, banner scrolled off", screen: museTestTipScreen, idle: true},
		{name: "draft restored by an interrupt, banner on screen", screen: museTestInterruptedScreen},
		{name: "draft, banner scrolled off", screen: museTestTipDraftScreen},
		{name: "draft under the banner", screen: strings.Replace(museTestIdleScreen, "\n❯\n", "\n❯ draft not sent\n", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := agent.MuseIdlePromptShowing(tc.screen); got != tc.idle {
				t.Fatalf("fixture: agent.MuseIdlePromptShowing = %v, want %v", got, tc.idle)
			}
			wantState := "active"
			if tc.idle {
				wantState = "idle"
			}
			if got := determineState(tc.screen, "muse"); got != wantState {
				t.Errorf("determineState = %q, want %q", got, wantState)
			}
			if got := HasIdlePattern(tc.screen, "muse"); got != tc.idle {
				t.Errorf("HasIdlePattern = %v, want %v", got, tc.idle)
			}
			sc := NewStateClassifier("%1", &ClassifierConfig{AgentType: "muse", HysteresisDuration: 0})
			if _, err := sc.ClassifyWithOutput(tc.screen); err != nil {
				t.Fatalf("baseline: %v", err)
			}
			activity, err := sc.ClassifyWithOutput(tc.screen)
			if err != nil {
				t.Fatalf("ClassifyWithOutput: %v", err)
			}
			if gotIdle := activity.State == StateWaiting; gotIdle != tc.idle {
				t.Errorf("classifier State = %q, want idle=%v (patterns %v)", activity.State, tc.idle, activity.DetectedPatterns)
			}
		})
	}
}

// TestMusePatterns_TipComposerIsIdle covers a Muse pane deep in a session:
// the banner has scrolled away, so the composer is the only idle signal, and
// about seven seconds after a turn Muse draws a dim tip into it. Every tip in
// agent.MuseComposerTipPrefixes, whole or cut short, must read as idle; a
// draft must not, and a live working line must still win.
func TestMusePatterns_TipComposerIsIdle(t *testing.T) {
	lib := NewPatternLibrary()
	const liveTip = "❯ Type @ to search and insert workspace file paths"

	hasComposerMatch := func(screen string) bool {
		for _, m := range lib.MatchByCategory(screen, "muse", CategoryIdle) {
			if m.Pattern == "muse_composer" {
				return true
			}
		}
		return false
	}

	if !hasComposerMatch(museTestTipScreen) {
		t.Fatal("live tip screen: muse_composer did not match the tipped composer")
	}
	if len(agent.MuseComposerTipPrefixes) == 0 {
		t.Fatal("agent.MuseComposerTipPrefixes is empty")
	}
	for _, prefix := range agent.MuseComposerTipPrefixes {
		for _, composer := range []string{"❯ " + prefix + " and the rest of the tip", "❯ " + prefix + "…"} {
			screen := strings.Replace(museTestTipScreen, liveTip, composer, 1)
			if !hasComposerMatch(screen) {
				t.Errorf("composer %q: muse_composer did not match", composer)
			}
			working := strings.Replace(museTestTipWorkingScreen, liveTip, composer, 1)
			if got := lib.MatchFirst(working, "muse"); got == nil || got.Pattern != "muse_working" {
				t.Errorf("composer %q under a working line: MatchFirst = %+v, want muse_working", composer, got)
			}
		}
	}

	if hasComposerMatch(museTestTipDraftScreen) {
		t.Error("draft screen: muse_composer matched a composer holding unsent text")
	}
	if lib.HasIdlePrompt(museTestTipDraftScreen, "muse") {
		t.Error("draft screen with the banner scrolled away has an idle pattern match")
	}

	// A pane narrower than the tip shows only its first wrapped line, with
	// no ellipsis (muse 1.4.11: "❯ Type @ to search and" at 26 columns,
	// "❯ Type @" at 12). Each such cut is still the empty composer.
	for _, composer := range []string{"❯ Type @ to search and", "❯ Type @", "❯ /goal pins a"} {
		screen := strings.Replace(museTestTipScreen, liveTip, composer, 1)
		if !hasComposerMatch(screen) {
			t.Errorf("narrow-pane composer %q: muse_composer did not match", composer)
		}
		sc := NewStateClassifier("%1", &ClassifierConfig{AgentType: "muse", HysteresisDuration: 0})
		if _, err := sc.ClassifyWithOutput(screen); err != nil {
			t.Fatalf("baseline: %v", err)
		}
		activity, err := sc.ClassifyWithOutput(screen)
		if err != nil {
			t.Fatalf("ClassifyWithOutput: %v", err)
		}
		if activity.State != StateWaiting {
			t.Errorf("narrow-pane composer %q: State = %q, want %q (patterns %v)", composer, activity.State, StateWaiting, activity.DetectedPatterns)
		}
	}
	for _, draft := range []string{"❯ Type", "❯ Type @ me when done"} {
		if hasComposerMatch(strings.Replace(museTestTipScreen, liveTip, draft, 1)) {
			t.Errorf("draft %q: muse_composer matched", draft)
		}
	}
}

// TestMusePatterns_BusyScreensReadBusy covers two live busy screens the robot
// layer read as idle: a long draft typed during the turn pushes the working
// line out of the fixed 15-line live window, and a pane too narrow for the
// working line cuts it before its closing parenthesis.
func TestMusePatterns_BusyScreensReadBusy(t *testing.T) {
	longDraft := "❯ start a turn\n\n" +
		"◆ Thinking (1s · esc to interrupt)\n\n" +
		museTestVoiceBar + "\n" +
		"❯ " + strings.Repeat("word ", 9) + "\n" +
		strings.Repeat("  "+strings.Repeat("word ", 9)+"\n", 13) +
		museTestRule + "\n" +
		"  echo · /private/tmp/claude-501/claude-port · YOLO"
	narrow := "❯ go\n\n" +
		"◈ Thinking (1s · esc to in\n\n" +
		"── Voice input (alt(opti…\n" +
		"❯\n" +
		"──────────────────────────\n" +
		"  echo · …/ws · YOLO"

	for name, screen := range map[string]string{"long draft": longDraft, "26 columns": narrow} {
		if !IsLiveBusy(screen, "muse") {
			t.Errorf("%s: IsLiveBusy = false, want true", name)
		}
		if got := determineState(screen, "muse"); got != "active" {
			t.Errorf("%s: determineState = %q, want active", name, got)
		}
	}
	if !HasThinkingPattern(narrow, "muse") {
		t.Error("26 columns: muse_working did not match the cut working line")
	}
}

func TestMusePatterns_DoNotLeakToClaude(t *testing.T) {
	lib := NewPatternLibrary()
	for _, screen := range []string{museTestWorkingScreen, museTestTrustPromptScreen} {
		for _, m := range lib.Match(screen, "claude") {
			if strings.HasPrefix(m.Pattern, "muse_") {
				t.Errorf("%s leaked to a claude pane", m.Pattern)
			}
		}
	}
	if !isKnownAgentPatternType("muse") || !isKnownAgentPatternType("muse-code") {
		t.Fatal("muse should be a known agent pattern type so shell prompts in its pane are not idle")
	}
}

// museTestTrustPromptScreen is Muse's first screen in an untrusted workspace
// when launched without --yolo or --trust-workspace, captured live from muse
// 1.4.11 (`muse --provider echo`) in a 120-column pane, trimmed the way
// tmux.Client returns captures, with the workspace path shortened. It carries
// no "Muse Code" banner and no status bar.
const museTestTrustPromptScreen = "Do you trust this workspace?\n" +
	"Workspace:\n" +
	"/private/tmp/claude-501/claude-port/untrusted\n\n" +
	"Trusting allows project-local skills, rules, hooks, and plugin config to load before the model runs.\n" +
	"Only trust this workspace when you trust its contents.\n" +
	"⟩ 1  Trust and continue\n" +
	"  2  Quit\n\n" +
	"Use Up/Down or 1/2, then Enter. Esc quits."

const (
	// museTestTipScreen is the bottom of a capture from a muse 1.4.11 echo
	// pane (`muse --provider echo --yolo`, 160x45 tmux pane, prompts and
	// rules shortened) eleven seconds after its sixteenth turn: the banner
	// has scrolled out of the pane and the idle composer shows a dim tip.
	museTestTipScreen = "◆ echo: line 15 of a longer prompt\n\n" +
		"❯ line 16 of a longer prompt\n\n" +
		"◆ echo: line 16 of a longer prompt\n\n" +
		museTestVoiceBar + "\n" +
		"❯ Type @ to search and insert workspace file paths\n" +
		museTestRule + "\n" +
		"  echo · /private/tmp/claude-501/claude-port · YOLO"

	// museTestTipWorkingScreen is the same screen with a live working line
	// above the tipped composer.
	museTestTipWorkingScreen = "◆ echo: line 15 of a longer prompt\n\n" +
		"❯ line 16 of a longer prompt\n\n" +
		"◇ Thinking (2s · esc to interrupt)\n\n" +
		museTestVoiceBar + "\n" +
		"❯ Type @ to search and insert workspace file paths\n" +
		museTestRule + "\n" +
		"  echo · /private/tmp/claude-501/claude-port · YOLO"

	// museTestTipDraftScreen is the same screen with unsent text in the
	// composer instead of the tip.
	museTestTipDraftScreen = "◆ echo: line 15 of a longer prompt\n\n" +
		"❯ line 16 of a longer prompt\n\n" +
		"◆ echo: line 16 of a longer prompt\n\n" +
		museTestVoiceBar + "\n" +
		"❯ draft not sent\n" +
		museTestRule + "\n" +
		"  echo · /private/tmp/claude-501/claude-port · YOLO"
)

// Pi 1.3.0 screens: the bottoms of tmux captures from panes running
// `pi --offline`, trimmed the way tmux.Client returns them. No prompt was
// sent, so the idle screens after a turn come from a synthetic session, and
// piTestWorkingNarrowScreen puts the status indicator into the editor's top
// rule the way Pi's CustomEditor renders it at 40 columns.
// piTestWorkingAboveEditorScreen is a live capture of a busy Pi whose
// extension-installed editor draws the indicator above the box.
var (
	piTestRule40 = strings.Repeat("─", 40)

	// 40 columns after a turn. Pi cut the usage line to "0.3%/...", and the
	// collapsed thinking block and the reply hold "Thinking..." and "running ".
	piTestIdleNarrowScreen = " list the go files and run the tests\n\n\n" +
		" Thinking...\n\n\n" +
		" $ ls *.go && go test ./...\n\n" +
		" main.go\n" +
		" ok     example   0.012s\n\n\n" +
		" There is one Go file, main.go, and the\n" +
		" tests pass. I'm running nothing else;\n" +
		" tell me what to change next...\n\n" +
		piTestRule40 + "\n\n" + piTestRule40 + "\n" +
		"/private/tmp/claude-501/-Volumes-1tb-...\n" +
		"↑2.6k ↓120 R1.2k CH46.2% $0.004 0.3%/..."

	// The same session in 160 columns (cwd shortened): nothing is cut.
	piTestIdleWideScreen = " There is one Go file, main.go, and the tests pass. I'm running nothing else; tell me what to change next...\n" +
		strings.Repeat("\n", 15) +
		strings.Repeat("─", 160) + "\n\n" + strings.Repeat("─", 160) + "\n" +
		"/private/tmp/claude-501/scratchpad/bd-7bzre/gitwork (main)\n" +
		"↑2.6k ↓120 R1.2k CH46.2% $0.004 0.3%/1.0M (auto)" + strings.Repeat(" ", 64) +
		"(vercel-ai-gateway) google/gemini-4-argon • high"

	// A fresh 32-column pane with the operator's extensions loaded. Their
	// status line is last and Pi cut it to "ponytail...".
	piTestIdleExtensionStatusScreen = " Ponytail loaded: full\n\n" +
		strings.Repeat("─", 32) + "\n\n" + strings.Repeat("─", 32) + "\n" +
		"/private/tmp/claude-501/-Volu...\n" +
		"0.0%/1.0M (auto)  google/gemini-\n" +
		"🧠 work · ready ○ 🐴 ponytail..."

	// Text typed into the editor after the turn and not sent.
	piTestDraftScreen = " tell me what to change next...\n\n" +
		piTestRule40 + "\n" +
		"rename main.go to app.go and update\n" +
		"the imports in every package that uses\n" +
		"it\n" +
		piTestRule40 + "\n" +
		"/private/tmp/claude-501/-Volumes-1tb-...\n" +
		"↑2.6k ↓120 R1.2k CH46.2% $0.004 0.3%/..."

	piTestWorkingNarrowScreen = strings.Replace(piTestIdleNarrowScreen, piTestRule40+"\n\n"+piTestRule40,
		"── ⠋ Working "+strings.Repeat("─", 27)+"\n\n"+piTestRule40, 1)

	piTestWorkingAboveEditorScreen = " Elapsed 1m 2s\n ⠧ Working\n" +
		strings.Repeat("─", 140) + "\n" + strings.Repeat("─", 140) + "\n" +
		"/Volumes/1tb/Projects/ManchesterSquare (main)\n" +
		"↑166k ↓20k R2.4M CH99.7% $0.181 8.8%/1.0M (auto)            (vercel-ai-gateway) deepseek/d\n" +
		"🧠 114-manchester · ready ● 🐴 ponytail: ⚡ FULL"
)

// TestPiPatterns_EditorIdleUnlessStatusShowing is the bd-7bzre regression in
// the pattern library. Pi's footer runs below its editor and Pi cuts it short
// with "..." in a narrow pane, which dots_spinner read as a spinner, so an
// idle Pi pane read as working. An idle Pi shows an empty editor box; a busy
// one shows its status indicator in or just above the editor.
func TestPiPatterns_EditorIdleUnlessStatusShowing(t *testing.T) {
	lib := NewPatternLibrary()

	for name, screen := range map[string]string{
		"usage line cut short, 40 columns":       piTestIdleNarrowScreen,
		"extension status cut short, 32 columns": piTestIdleExtensionStatusScreen,
		"160 columns":                            piTestIdleWideScreen,
	} {
		if !lib.HasIdlePrompt(screen, "pi") {
			t.Errorf("idle Pi screen (%s) has no idle pattern match", name)
		}
		if lib.HasThinkingIndicator(screen, "pi") {
			t.Errorf("idle Pi screen (%s) matched a thinking pattern: %v", name, lib.MatchByCategory(screen, "pi", CategoryThinking))
		}
		if IsLiveBusy(screen, "pi") {
			t.Errorf("IsLiveBusy(idle Pi screen, %s) = true, want false", name)
		}
	}

	for name, screen := range map[string]string{
		"indicator in the top rule":  piTestWorkingNarrowScreen,
		"indicator above the editor": piTestWorkingAboveEditorScreen,
	} {
		if !IsLiveBusy(screen, "pi") {
			t.Errorf("IsLiveBusy(working Pi screen, %s) = false, want true", name)
		}
		if got := lib.MatchFirst(screen, "pi"); got == nil || got.Pattern != "pi_working" {
			t.Errorf("MatchFirst(working Pi screen, %s) = %+v, want pi_working ahead of the editor", name, got)
		}
	}
}

func TestPiPatterns_ClassifierStates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		screen string
		want   AgentState
	}{
		{name: "idle, usage line cut short", screen: piTestIdleNarrowScreen, want: StateWaiting},
		{name: "idle, extension status cut short", screen: piTestIdleExtensionStatusScreen, want: StateWaiting},
		{name: "idle, 160 columns", screen: piTestIdleWideScreen, want: StateWaiting},
		{name: "working, indicator in the top rule", screen: piTestWorkingNarrowScreen, want: StateThinking},
		{name: "working, indicator above the editor", screen: piTestWorkingAboveEditorScreen, want: StateThinking},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := NewStateClassifier("%1", &ClassifierConfig{AgentType: "pi", HysteresisDuration: 0})
			if _, err := sc.ClassifyWithOutput(tc.screen); err != nil {
				t.Fatalf("baseline: %v", err)
			}
			activity, err := sc.ClassifyWithOutput(tc.screen)
			if err != nil {
				t.Fatalf("ClassifyWithOutput: %v", err)
			}
			if activity.State != tc.want {
				t.Fatalf("State = %q, want %q (patterns %v)", activity.State, tc.want, activity.DetectedPatterns)
			}
		})
	}
}

// TestPiPatterns_AgreeWithPiIdlePromptShowing pins the robot verdicts for Pi
// panes to agent.PiIdlePromptShowing, which the parser and status detectors
// use. The old pi_path_status and pi_model_status idle patterns matched the
// footer, which Pi draws for the whole turn, so a busy pane and a pane
// holding a draft both read idle.
func TestPiPatterns_AgreeWithPiIdlePromptShowing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		screen string
		idle   bool
	}{
		{name: "idle, usage line cut short", screen: piTestIdleNarrowScreen, idle: true},
		{name: "idle, extension status cut short", screen: piTestIdleExtensionStatusScreen, idle: true},
		{name: "idle, 160 columns", screen: piTestIdleWideScreen, idle: true},
		{name: "draft", screen: piTestDraftScreen},
		{name: "working, indicator in the top rule", screen: piTestWorkingNarrowScreen},
		{name: "working, indicator above the editor", screen: piTestWorkingAboveEditorScreen},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := agent.PiIdlePromptShowing(tc.screen); got != tc.idle {
				t.Fatalf("fixture: agent.PiIdlePromptShowing = %v, want %v", got, tc.idle)
			}
			wantState := "active"
			if tc.idle {
				wantState = "idle"
			}
			if got := determineState(tc.screen, "pi"); got != wantState {
				t.Errorf("determineState = %q, want %q", got, wantState)
			}
			// A busy pane keeps its empty editor box below the indicator,
			// so pi_editor may match there; pi_working outranks it.
			if !strings.Contains(tc.name, "working") {
				if got := HasIdlePattern(tc.screen, "pi"); got != tc.idle {
					t.Errorf("HasIdlePattern = %v, want %v", got, tc.idle)
				}
			}
		})
	}
}

// TestDotsSpinnerSkipsAgentsThatDrawAFooterLast covers the wildcard behind
// bd-7bzre. tmux trims a capture to its last non-empty line, so dots_spinner
// only ever reads the last line. Claude, Codex, Muse and Pi always draw a
// footer there, so a trailing "..." in their panes is a footer cut short and
// their own patterns decide. Agents whose output can end on a "..." progress
// line keep the wildcard.
func TestDotsSpinnerSkipsAgentsThatDrawAFooterLast(t *testing.T) {
	lib := NewPatternLibrary()
	hasDots := func(content, agentType string) bool {
		for _, m := range lib.Match(content, agentType) {
			if m.Pattern == "dots_spinner" {
				return true
			}
		}
		for _, m := range lib.MatchByCategory(content, agentType, CategoryThinking) {
			if m.Pattern == "dots_spinner" {
				return true
			}
		}
		return false
	}

	const cutFooter = "────────────────────────────────────────\n/private/tmp/claude-501/-Volumes-1tb-..."
	for _, agentType := range []string{"claude", "cc", "codex", "cod", "muse", "pi"} {
		if hasDots(cutFooter, agentType) {
			t.Errorf("dots_spinner matched a cut-off footer for %q", agentType)
		}
	}
	for _, agentType := range []string{"", "aider", "gemini", "ollama"} {
		if !hasDots("Generating response...", agentType) {
			t.Errorf("dots_spinner did not match a trailing progress line for %q", agentType)
		}
	}
}
