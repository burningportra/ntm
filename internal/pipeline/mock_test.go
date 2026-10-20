package pipeline

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/tmux"
)

func gateScriptedDeliveries(t *testing.T, mock *MockTmuxClient) func() {
	t.Helper()
	gate := make(chan struct{})
	mock.setScriptedDeliveryGate(gate)
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() { close(gate) })
	}
	t.Cleanup(func() {
		release()
		mock.waitForScriptedDeliveries()
	})
	return release
}

func TestMockTmuxClientGetPanesReturnsSortedCopy(t *testing.T) {
	mock := NewMockTmuxClient()
	mock.AddPane("session-a", tmux.Pane{ID: "%2", Index: 2, Type: tmux.AgentGemini})
	mock.AddPane("session-a", tmux.Pane{ID: "%1", Index: 1, Type: tmux.AgentCodex})
	mock.AddPane("session-b", tmux.Pane{ID: "%3", Index: 1, Type: tmux.AgentClaude})
	t.Cleanup(mock.Reset)

	panes, err := mock.GetPanes("session-a")
	if err != nil {
		t.Fatalf("GetPanes returned error: %v", err)
	}
	if len(panes) != 2 {
		t.Fatalf("GetPanes returned %d panes, want 2", len(panes))
	}
	if panes[0].ID != "%1" || panes[1].ID != "%2" {
		t.Fatalf("GetPanes order = [%s %s], want [%%1 %%2]", panes[0].ID, panes[1].ID)
	}

	panes[0].ID = "mutated"
	panes, err = mock.GetPanes("session-a")
	if err != nil {
		t.Fatalf("GetPanes after mutation returned error: %v", err)
	}
	if panes[0].ID != "%1" {
		t.Fatalf("GetPanes leaked caller mutation, got first pane %q", panes[0].ID)
	}
}

func TestMockTmuxClientPasteAndCaptureArePaneScoped(t *testing.T) {
	mock := NewMockTmuxClient(
		tmux.Pane{ID: "%1", Index: 1, Type: tmux.AgentCodex},
		tmux.Pane{ID: "%2", Index: 2, Type: tmux.AgentGemini},
	)
	t.Cleanup(mock.Reset)

	if err := mock.SetPaneOutput("%1", "seed\n"); err != nil {
		t.Fatalf("SetPaneOutput returned error: %v", err)
	}
	if err := mock.SubmitPrompt(t.Context(), "%1", tmux.AgentCodex, "first\nsecond"); err != nil {
		t.Fatalf("SubmitPrompt returned error: %v", err)
	}
	if err := mock.SubmitPrompt(t.Context(), "%2", tmux.AgentGemini, "other"); err != nil {
		t.Fatalf("SubmitPrompt second pane returned error: %v", err)
	}

	got, err := mock.CapturePaneOutput("%1", 0)
	if err != nil {
		t.Fatalf("CapturePaneOutput returned error: %v", err)
	}
	if got != "seed\nfirst\nsecond\n" {
		t.Fatalf("CapturePaneOutput = %q, want seeded prompt output", got)
	}

	tail, err := mock.CapturePaneOutput("%1", 2)
	if err != nil {
		t.Fatalf("CapturePaneOutput tail returned error: %v", err)
	}
	if tail != "first\nsecond\n" {
		t.Fatalf("CapturePaneOutput tail = %q, want last two lines", tail)
	}

	other, err := mock.CapturePaneOutput("%2", 0)
	if err != nil {
		t.Fatalf("CapturePaneOutput second pane returned error: %v", err)
	}
	if other != "other\n" {
		t.Fatalf("second pane output = %q, want isolated output", other)
	}

	history, err := mock.PasteHistory("%1")
	if err != nil {
		t.Fatalf("PasteHistory returned error: %v", err)
	}
	if len(history) != 1 || history[0].Content != "first\nsecond" || history[0].AgentType != tmux.AgentCodex {
		t.Fatalf("PasteHistory = %+v, want one codex submit", history)
	}
}

func TestAgentScripterSubstringRegexAndDefaultResponses(t *testing.T) {
	mock := NewMockTmuxClient(tmux.Pane{ID: "%1", Index: 1, Type: tmux.AgentCodex})
	scripter := NewAgentScripter().
		Match("H_ID=H-001", "EV-001 supports H-001\n").
		Default("DEFAULT RESPONSE\n")
	if err := scripter.MatchRegex(`mode=audit-[0-9]+`, "AUDIT RESPONSE\n"); err != nil {
		t.Fatalf("MatchRegex returned error: %v", err)
	}
	mock.SetAgentScripter(scripter)
	t.Cleanup(mock.Reset)

	if err := mock.SubmitPrompt(t.Context(), "%1", tmux.AgentCodex, "dispatch MO-04a-investigate H_ID=H-001"); err != nil {
		t.Fatalf("SubmitPrompt substring returned error: %v", err)
	}
	if err := mock.SubmitPrompt(t.Context(), "%1", tmux.AgentCodex, "mode=audit-42"); err != nil {
		t.Fatalf("SubmitPrompt regex returned error: %v", err)
	}
	if err := mock.SubmitPrompt(t.Context(), "%1", tmux.AgentCodex, "no registered pattern here"); err != nil {
		t.Fatalf("SubmitPrompt default returned error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := scripter.Wait(ctx, 3); err != nil {
		t.Fatalf("Wait returned error: %v", err)
	}

	output, err := mock.CapturePaneOutput("%1", 0)
	if err != nil {
		t.Fatalf("CapturePaneOutput returned error: %v", err)
	}
	for _, want := range []string{"EV-001 supports H-001", "AUDIT RESPONSE", "DEFAULT RESPONSE"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
}

func TestAgentScripterSequentialResponses(t *testing.T) {
	mock := NewMockTmuxClient(tmux.Pane{ID: "%1", Index: 1, Type: tmux.AgentCodex})
	scripter := NewAgentScripter().
		Match("repeat", "first response\n").
		Match("repeat", "second response\n")
	mock.SetAgentScripter(scripter)
	t.Cleanup(mock.Reset)

	if err := mock.SubmitPrompt(t.Context(), "%1", tmux.AgentCodex, "repeat prompt"); err != nil {
		t.Fatalf("first SubmitPrompt returned error: %v", err)
	}
	if err := mock.SubmitPrompt(t.Context(), "%1", tmux.AgentCodex, "repeat prompt"); err != nil {
		t.Fatalf("second SubmitPrompt returned error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := scripter.Wait(ctx, 2); err != nil {
		t.Fatalf("Wait returned error: %v", err)
	}

	output, err := mock.CapturePaneOutput("%1", 0)
	if err != nil {
		t.Fatalf("CapturePaneOutput returned error: %v", err)
	}
	first := strings.Index(output, "first response")
	second := strings.Index(output, "second response")
	if first == -1 || second == -1 || first >= second {
		t.Fatalf("responses not captured in registration order:\n%s", output)
	}
}

// bd-05x7b: Reset must invalidate any in-flight delayed scripter responses
// so they don't append stale output or bump produced after the test enters
// a fresh phase. We schedule a delivery with a long delay, Reset before the
// timer fires, and assert that the pane never receives the stale response
// and the produced count stays at 0.
func TestAgentScripterResetCancelsInFlightDelayedResponses(t *testing.T) {
	mock := NewMockTmuxClient(tmux.Pane{ID: "%1", Index: 1, Type: tmux.AgentCodex})
	releaseDelivery := gateScriptedDeliveries(t, mock)
	scripter := NewAgentScripter().
		Match("ping", "STALE\n").
		Delay(time.Nanosecond)
	mock.SetAgentScripter(scripter)
	t.Cleanup(mock.Reset)

	if err := mock.SubmitPrompt(t.Context(), "%1", tmux.AgentCodex, "ping prompt"); err != nil {
		t.Fatalf("SubmitPrompt returned error: %v", err)
	}

	// SubmitPrompt schedules delayed delivery before returning, so Reset can
	// deterministically invalidate that generation immediately.
	mock.Reset()
	releaseDelivery()
	mock.waitForScriptedDeliveries()

	output, err := mock.CapturePaneOutput("%1", 0)
	if err != nil {
		t.Fatalf("CapturePaneOutput returned error: %v", err)
	}
	if strings.Contains(output, "STALE") {
		t.Fatalf("pane output contaminated by stale delayed response: %q", output)
	}

	// Reset clears produced; a stale delivery would have re-incremented it.
	if got := scripter.producedCount(); got != 0 {
		t.Fatalf("produced count = %d after Reset, want 0", got)
	}
}

func TestAgentScripterDirectResetCancelsInFlightDelayedResponses(t *testing.T) {
	mock := NewMockTmuxClient(tmux.Pane{ID: "%1", Index: 1, Type: tmux.AgentCodex})
	releaseDelivery := gateScriptedDeliveries(t, mock)
	scripter := NewAgentScripter().
		Match("ping", "STALE\n").
		Delay(time.Nanosecond)
	mock.SetAgentScripter(scripter)
	t.Cleanup(mock.Reset)

	if err := mock.SubmitPrompt(t.Context(), "%1", tmux.AgentCodex, "ping prompt"); err != nil {
		t.Fatalf("SubmitPrompt returned error: %v", err)
	}
	scripter.Reset()
	releaseDelivery()
	mock.waitForScriptedDeliveries()

	output, err := mock.CapturePaneOutput("%1", 0)
	if err != nil {
		t.Fatalf("CapturePaneOutput returned error: %v", err)
	}
	if strings.Contains(output, "STALE") {
		t.Fatalf("pane output contaminated after direct scripter reset: %q", output)
	}

	if got := scripter.producedCount(); got != 0 {
		t.Fatalf("produced count = %d after direct Reset, want 0", got)
	}
}

// bd-05x7b: After Reset, a fresh delayed delivery should land cleanly even
// though a stale generation goroutine is still in flight. Locks the
// generation invariant: only deliveries from the current generation count.
func TestAgentScripterResetAllowsFreshDeliveryAfterStaleDrop(t *testing.T) {
	mock := NewMockTmuxClient(tmux.Pane{ID: "%1", Index: 1, Type: tmux.AgentCodex})
	releaseStaleDelivery := gateScriptedDeliveries(t, mock)
	scripter := NewAgentScripter().
		Match("first", "STALE\n").
		Match("second", "FRESH\n").
		Delay(time.Nanosecond)
	mock.SetAgentScripter(scripter)
	t.Cleanup(mock.Reset)

	if err := mock.SubmitPrompt(t.Context(), "%1", tmux.AgentCodex, "first prompt"); err != nil {
		t.Fatalf("first SubmitPrompt returned error: %v", err)
	}
	mock.Reset()
	mock.setScriptedDeliveryGate(nil)

	// Re-arm with a same-pattern rule and dispatch fresh — the new generation
	// should deliver normally.
	scripter.Match("second", "FRESH\n")
	if err := mock.SubmitPrompt(t.Context(), "%1", tmux.AgentCodex, "second prompt"); err != nil {
		t.Fatalf("second SubmitPrompt returned error: %v", err)
	}
	releaseStaleDelivery()
	mock.waitForScriptedDeliveries()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := scripter.Wait(ctx, 1); err != nil {
		t.Fatalf("Wait for fresh delivery returned error: %v", err)
	}

	output, err := mock.CapturePaneOutput("%1", 0)
	if err != nil {
		t.Fatalf("CapturePaneOutput returned error: %v", err)
	}
	if strings.Contains(output, "STALE") {
		t.Fatalf("pane output contaminated by stale delayed response: %q", output)
	}
	if !strings.Contains(output, "FRESH") {
		t.Fatalf("pane output missing fresh delivery: %q", output)
	}
}

func TestAgentScripterNoMatchReturnsClearError(t *testing.T) {
	mock := NewMockTmuxClient(tmux.Pane{ID: "%1", Index: 1, Type: tmux.AgentCodex})
	mock.SetAgentScripter(NewAgentScripter())
	t.Cleanup(mock.Reset)

	err := mock.SubmitPrompt(t.Context(), "%1", tmux.AgentCodex, "unmatched prompt")
	if err == nil {
		t.Fatal("SubmitPrompt returned nil error, want script exhaustion error")
	}
	if !strings.Contains(err.Error(), "no response") {
		t.Fatalf("SubmitPrompt error = %q, want clear no-response message", err.Error())
	}

	history, err := mock.PasteHistory("%1")
	if err != nil {
		t.Fatalf("PasteHistory returned error: %v", err)
	}
	if len(history) != 0 {
		t.Fatalf("PasteHistory length = %d, want no paste recorded for rejected prompt", len(history))
	}
}

func TestExecutorRunUsesMockTmuxClient(t *testing.T) {
	mock := NewMockTmuxClient()
	mock.AddPane("mock-session", tmux.Pane{
		ID:    "%1",
		Index: 1,
		Title: "mock-session__cod_1",
		Type:  tmux.AgentCodex,
	})
	t.Cleanup(mock.Reset)

	cfg := DefaultExecutorConfig("mock-session")
	cfg.ProjectDir = t.TempDir()
	executor := NewExecutor(cfg)
	executor.SetTmuxClient(mock)

	workflow := &Workflow{
		SchemaVersion: SchemaVersion,
		Name:          "mock-workflow",
		Settings:      DefaultWorkflowSettings(),
		Steps: []Step{
			{
				ID:     "send",
				Prompt: "hello from mock",
				Pane:   PaneSpec{Index: 1},
				Wait:   WaitNone,
			},
		},
	}

	state, err := executor.Run(context.Background(), workflow, nil, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	result, ok := state.Steps["send"]
	if !ok {
		t.Fatal("state missing send step result")
	}
	if result.Status != StatusCompleted {
		t.Fatalf("step status = %q, want %q; error: %+v", result.Status, StatusCompleted, result.Error)
	}
	if result.PaneUsed != "%1" {
		t.Fatalf("PaneUsed = %q, want %%1", result.PaneUsed)
	}

	history, err := mock.PasteHistory("%1")
	if err != nil {
		t.Fatalf("PasteHistory returned error: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("PasteHistory length = %d, want 1", len(history))
	}
	if history[0].Content != "hello from mock" || history[0].AgentType != tmux.AgentCodex {
		t.Fatalf("PasteHistory[0] = %+v, want the prompt submitted as codex", history[0])
	}
}

// Every pane's prompt goes through SubmitPrompt with that pane's agent type,
// so the real client picks the agent's submit protocol: no agent type is
// typed with plain send-keys anymore. Covers the prompt step and the parallel
// sub-step send paths.
func TestExecutorSubmitsEveryPromptWithItsPaneAgentType(t *testing.T) {
	mock := NewMockTmuxClient(
		tmux.Pane{ID: "%1", Index: 1, Title: "mock-session__muse_1", Type: tmux.AgentMuse},
		tmux.Pane{ID: "%2", Index: 2, Title: "mock-session__cod_1", Type: tmux.AgentCodex},
		tmux.Pane{ID: "%3", Index: 3, Title: "mock-session__cc_1", Type: tmux.AgentClaude},
		tmux.Pane{ID: "%4", Index: 4, Title: "mock-session__user_1", Type: tmux.AgentUser},
	)
	cfg := DefaultExecutorConfig("mock-session")
	cfg.ProjectDir = t.TempDir()
	executor := NewExecutor(cfg)
	executor.SetTmuxClient(mock)

	workflow := &Workflow{
		SchemaVersion: SchemaVersion,
		Name:          "agent-send-workflow",
		Settings:      DefaultWorkflowSettings(),
		Steps: []Step{
			{ID: "muse", Prompt: "review the diff\nthen report", Pane: PaneSpec{Index: 1}, Wait: WaitNone},
			{ID: "codex", Prompt: "review the diff\nthen report", Pane: PaneSpec{Index: 2}, Wait: WaitNone},
			{ID: "claude", Prompt: "review the diff\nthen report", Pane: PaneSpec{Index: 3}, Wait: WaitNone},
			{ID: "shell", Prompt: "echo one\necho two", Pane: PaneSpec{Index: 4}, Wait: WaitNone},
			{ID: "fan", Parallel: ParallelSpec{Steps: []Step{
				{ID: "fan-muse", Prompt: "fan out\nto muse", Pane: PaneSpec{Index: 1}, Wait: WaitNone},
				{ID: "fan-claude", Prompt: "fan out\nto claude", Pane: PaneSpec{Index: 3}, Wait: WaitNone},
			}}},
		},
	}

	state, err := executor.Run(context.Background(), workflow, nil, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	for _, id := range []string{"muse", "codex", "claude", "shell", "fan"} {
		if got := state.Steps[id]; got.Status != StatusCompleted {
			t.Fatalf("step %s status = %q, want %q; error: %+v", id, got.Status, StatusCompleted, got.Error)
		}
	}

	for pane, want := range map[string][]tmux.AgentType{
		"%1": {tmux.AgentMuse, tmux.AgentMuse},
		"%2": {tmux.AgentCodex},
		"%3": {tmux.AgentClaude, tmux.AgentClaude},
		"%4": {tmux.AgentUser},
	} {
		history, err := mock.PasteHistory(pane)
		if err != nil {
			t.Fatalf("PasteHistory(%s) returned error: %v", pane, err)
		}
		got := make([]tmux.AgentType, len(history))
		for i, paste := range history {
			got[i] = paste.AgentType
		}
		if !slices.Equal(got, want) {
			t.Fatalf("pane %s prompts were submitted as %q, want %q", pane, got, want)
		}
	}
}
