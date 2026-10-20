package swarm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/ratelimit"
	"github.com/Dicklesworthstone/ntm/internal/tmux"
	"github.com/Dicklesworthstone/ntm/tests/testutil"
)

// countingPromptInjectionTmuxClient is the injector's fake tmux. It shows
// captureOutput to readiness polls, lists panes (a default cc/grok/cod
// session unless panes is set) and records each prompt handed to the shared
// submit. commands are the pane's foreground process on successive polls, the
// last one repeating; none means the lookup finds nothing.
type countingPromptInjectionTmuxClient struct {
	captureCount  int
	commandCount  int
	getPanesCount int
	submitCount   int

	// captureOutput is returned from readiness polls; an idle-looking prompt
	// keeps WaitForReady from burning its full 30s timeout per pane.
	captureOutput string
	commands      []string
	panes         []tmux.Pane
	afterCapture  func()
	// submitted records every prompt handed to SubmitPrompt; submitErr fails it.
	submitted []submittedPrompt
	submitErr error
}

type submittedPrompt struct {
	target    string
	agentType tmux.AgentType
	prompt    string
}

func (c *countingPromptInjectionTmuxClient) CaptureForStatusDetectionContext(context.Context, string) (string, error) {
	c.captureCount++
	if c.afterCapture != nil {
		c.afterCapture()
	}
	return c.captureOutput, nil
}

func (c *countingPromptInjectionTmuxClient) PaneCurrentCommand(context.Context, string) (string, error) {
	defer func() { c.commandCount++ }()
	if len(c.commands) == 0 {
		return "", nil
	}
	return c.commands[min(c.commandCount, len(c.commands)-1)], nil
}

func (c *countingPromptInjectionTmuxClient) GetPanesContext(context.Context, string) ([]tmux.Pane, error) {
	c.getPanesCount++
	if c.panes != nil {
		return c.panes, nil
	}
	return []tmux.Pane{
		{ID: "%1", Index: 1, WindowIndex: 1, Title: "proj__cc_1", Type: tmux.AgentClaude, Command: "claude"},
		{ID: "%2", Index: 2, WindowIndex: 1, Title: "proj__grok_2", Type: tmux.AgentGrok, Command: "grok"},
		{ID: "%3", Index: 3, WindowIndex: 1, Title: "proj__cod_3", Type: tmux.AgentCodex, Command: "codex"},
	}, nil
}

func (c *countingPromptInjectionTmuxClient) RunContext(context.Context, ...string) (string, error) {
	return "proj", nil
}

func (c *countingPromptInjectionTmuxClient) SubmitPrompt(_ context.Context, target string, agentType tmux.AgentType, prompt string) error {
	c.submitCount++
	c.submitted = append(c.submitted, submittedPrompt{target: target, agentType: agentType, prompt: prompt})
	return c.submitErr
}

// agentPaneAt12 is session proj's one pane, proj:1.2 (%2), running agentType.
func agentPaneAt12(agentType, command string) []tmux.Pane {
	canonical := tmux.AgentType(agentType).Canonical()
	return []tmux.Pane{{ID: "%2", Index: 2, WindowIndex: 1, Title: fmt.Sprintf("proj__%s_2", canonical), Type: canonical, Command: command}}
}

// TestPromptInjector_SubmitsThroughSharedProtocol checks that the injector
// hands each prompt, whole and with its pane's agent type, to the shared
// submit on the bound pane and sends no keys of its own.
func TestPromptInjector_SubmitsThroughSharedProtocol(t *testing.T) {
	for _, tc := range []struct{ agentType, command string }{
		{"cc", "claude"},
		{"cod", "codex"},
		{"muse-code", "muse"},
	} {
		t.Run(tc.agentType, func(t *testing.T) {
			client := &countingPromptInjectionTmuxClient{
				captureOutput: "────────\n❯\n────────\n›\n",
				panes:         agentPaneAt12(tc.agentType, tc.command),
			}
			injector := NewPromptInjector()
			injector.tmuxClientOverride = client

			prompt := "first read AGENTS.md\nthen run bv --robot-triage\nthen claim a bead"
			if err := injector.InjectPrompt("proj:1.2", tc.agentType, prompt); err != nil {
				t.Fatalf("InjectPrompt() error = %v", err)
			}
			want := submittedPrompt{target: "%2", agentType: tmux.AgentType(tc.agentType).Canonical(), prompt: prompt}
			if len(client.submitted) != 1 || client.submitted[0] != want {
				t.Fatalf("submitted = %+v, want [%+v]", client.submitted, want)
			}
		})
	}
}

func TestPromptInjectorContextDeadlineNeverSendsIntoUnreadyPane(t *testing.T) {
	client := &countingPromptInjectionTmuxClient{}
	injector := NewPromptInjector()
	injector.tmuxClientOverride = client
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	result, err := injector.InjectBatchWithContext(ctx, []InjectionTarget{{SessionPane: "%1", AgentType: "cc"}}, "implement the feature")
	if !errors.Is(err, context.DeadlineExceeded) || result == nil || result.Successful != 0 || len(result.Results) != 1 || result.Results[0].Success {
		t.Fatalf("expected retained failed readiness receipt, got %+v, %v", result, err)
	}
	if client.submitCount != 0 {
		t.Fatal("prompt was submitted despite failed readiness")
	}
}

func TestPromptInjectorRevalidatesAgentAfterReadiness(t *testing.T) {
	for _, change := range []string{"shell", "dead", "provider", "disappeared"} {
		t.Run(change, func(t *testing.T) {
			client := &countingPromptInjectionTmuxClient{captureOutput: "claude>"}
			client.afterCapture = func() {
				pane := tmux.Pane{ID: "%1", Title: "proj__cc_1", Type: tmux.AgentClaude, Command: "claude"}
				switch change {
				case "shell":
					pane.Command = "bash"
				case "dead":
					pane.Dead = true
				case "provider":
					pane.Type = tmux.AgentCodex
				case "disappeared":
					pane.ID = "%99"
				}
				client.panes = []tmux.Pane{pane}
			}
			injector := NewPromptInjector()
			injector.tmuxClientOverride = client
			if err := injector.InjectPromptContext(context.Background(), "%1", "cc", "implement the feature"); err == nil {
				t.Fatal("changed pane was accepted after a stale ready capture")
			}
			if client.submitCount != 0 {
				t.Fatal("prompt reached a changed pane")
			}
		})
	}
}

// TestPromptInjector_FailsFastOnShellPane injects into a pane whose foreground
// process is zsh and whose prompt looks like Claude's idle composer. The
// injector has to give up after its grace period with ErrPaneNotAgent and send
// nothing: the old injector read the prompt as an idle agent and typed the
// marching orders into the shell, and on a plainer prompt it waited out its
// full 30 s before typing them anyway.
func TestPromptInjector_FailsFastOnShellPane(t *testing.T) {
	client := &countingPromptInjectionTmuxClient{
		captureOutput: "────────\n❯\n────────\n",
		commands:      []string{"zsh"},
		panes:         agentPaneAt12("cc", "zsh"),
	}
	injector := NewPromptInjector()
	injector.tmuxClientOverride = client
	injector.nonAgentPaneGrace = 300 * time.Millisecond

	start := time.Now()
	err := injector.InjectPrompt("proj:1.2", "cc", "first read AGENTS.md")
	elapsed := time.Since(start)
	if !errors.Is(err, ErrPaneNotAgent) || !strings.Contains(err.Error(), "%2 has been running zsh") {
		t.Fatalf("InjectPrompt() error = %v, want ErrPaneNotAgent naming the pane and its shell", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("InjectPrompt() took %s to refuse a shell pane, want about the 300ms grace", elapsed)
	}
	if len(client.submitted) != 0 {
		t.Fatalf("submitted = %+v to a shell pane, want nothing", client.submitted)
	}
}

// TestPromptInjector_WaitsOutLauncherShell injects into a pane whose agent was
// typed into a shell: the shell is the foreground process for the first polls,
// then the agent. The shell phase is shorter than the grace period, so the
// prompt goes to the agent once it shows its idle composer.
func TestPromptInjector_WaitsOutLauncherShell(t *testing.T) {
	client := &countingPromptInjectionTmuxClient{
		captureOutput: "────────\n❯\n────────\n",
		commands:      []string{"zsh", "zsh", "zsh", "claude"},
		panes:         agentPaneAt12("cc", "claude"),
	}
	injector := NewPromptInjector()
	injector.tmuxClientOverride = client
	injector.nonAgentPaneGrace = 5 * time.Second

	if err := injector.InjectPrompt("proj:1.2", "cc", "first read AGENTS.md"); err != nil {
		t.Fatalf("InjectPrompt() error = %v", err)
	}
	if client.commandCount < 4 {
		t.Fatalf("prompt sent after %d process checks, want it held until the agent replaced the shell", client.commandCount)
	}
	if len(client.submitted) != 1 {
		t.Fatalf("submitted = %+v, want one prompt", client.submitted)
	}
}

func TestPromptInjector_SubmitErrorFailsInjection(t *testing.T) {
	injector := NewPromptInjector()
	injector.tmuxClientOverride = &countingPromptInjectionTmuxClient{
		captureOutput: "────────\n❯\n────────\n",
		commands:      []string{"claude"},
		panes:         agentPaneAt12("cc", "claude"),
		submitErr:     errors.New("claude submission unconfirmed: prompt still in composer after rescue"),
	}

	err := injector.InjectPrompt("proj:1.2", "cc", "continue")
	if err == nil || !strings.Contains(err.Error(), "claude submission unconfirmed") {
		t.Fatalf("InjectPrompt() error = %v, want the submit error", err)
	}
}

// TestPromptInjector_SubmitsMultiLinePromptOnItsClient injects a 3-line
// prompt into a pane whose "agent" draws an idle composer, turns on bracketed
// paste and records its raw input, once per agent type. tmux.DefaultClient
// points at an unreachable host meanwhile, so only the injector's own client
// (cli/swarm.go passes a remote one) can reach the pane. The prompt has to
// arrive as one paste followed by the shared protocol's two Enters; the old
// injector gave Claude and Muse one Enter 100 or 300 ms after the paste.
func TestPromptInjector_SubmitsMultiLinePromptOnItsClient(t *testing.T) {
	testutil.RequireTmuxThrottled(t)

	for _, agentType := range []string{"cc", "cod", "muse"} {
		t.Run(agentType, func(t *testing.T) {
			dir := t.TempDir()
			received := filepath.Join(dir, "received.txt")
			idleScreen := `────────\n❯\n────────\n`
			if agentType == "cod" {
				idleScreen = `›\n`
			}
			// stty -echo keeps the input off the screen, where the Claude and
			// Codex submission checks would read it as a stranded composer;
			// cat -u writes each line to the file as it arrives.
			script := filepath.Join(dir, "agent.sh")
			body := fmt.Sprintf("#!/bin/sh\nstty -echo\nprintf '\\033[?2004h%s'\nexec cat -uv > %q\n", idleScreen, received)
			if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
				t.Fatalf("write agent script: %v", err)
			}

			local := tmux.NewClient("")
			session := fmt.Sprintf("ntm-test-injector-%s-%d", agentType, time.Now().UnixNano())
			if err := local.RunSilent("new-session", "-d", "-s", session, "-x", "120", "-y", "30", script); err != nil {
				t.Fatalf("create session: %v", err)
			}
			t.Cleanup(func() { _ = local.RunSilent("kill-session", "-t", session) })
			paneID, err := local.Run("display-message", "-p", "-t", session, "#{pane_id}")
			if err != nil {
				t.Fatalf("pane id: %v", err)
			}
			paneID = strings.TrimSpace(paneID)
			// The injector delivers only to a pane that carries the agent's identity.
			if err := local.SetPaneAgentIdentityContext(context.Background(), paneID, fmt.Sprintf("%s__%s_1", session, agentType), tmux.AgentType(agentType)); err != nil {
				t.Fatalf("set pane identity: %v", err)
			}

			defaultClient := tmux.DefaultClient
			tmux.DefaultClient = tmux.NewClient("ntm-test-unreachable.invalid")
			t.Cleanup(func() { tmux.DefaultClient = defaultClient })

			prompt := "first read AGENTS.md\nthen run bv --robot-triage\nthen claim a bead"
			if err := NewPromptInjectorWithClient(local).InjectPrompt(paneID, agentType, prompt); err != nil {
				t.Fatalf("InjectPrompt() error = %v", err)
			}

			// The tty turns the paste's CRs and each Enter into newlines.
			const want = "^[[200~first read AGENTS.md\nthen run bv --robot-triage\nthen claim a bead^[[201~\n\n"
			var got string
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				data, _ := os.ReadFile(received)
				got = string(data)
				if got == want {
					return
				}
				time.Sleep(100 * time.Millisecond)
			}
			t.Fatalf("%s pane input = %q, want %q", agentType, got, want)
		})
	}
}

func TestNewPromptInjector(t *testing.T) {
	injector := NewPromptInjector()

	if injector == nil {
		t.Fatal("NewPromptInjector returned nil")
	}

	if injector.TmuxClient != nil {
		t.Error("expected TmuxClient to be nil for default client")
	}

	if injector.StaggerDelay != 300*time.Millisecond {
		t.Errorf("expected StaggerDelay of 300ms, got %v", injector.StaggerDelay)
	}

	if injector.Logger == nil {
		t.Error("expected non-nil Logger")
	}

	if len(injector.Templates) == 0 {
		t.Error("expected non-empty Templates map")
	}
}

func TestGetTemplate(t *testing.T) {
	injector := NewPromptInjector()

	tests := []struct {
		name        string
		expectEmpty bool
	}{
		{"default", false},
		{"review", false},
		{"test", false},
		{"nonexistent", false}, // Falls back to default
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpl := injector.GetTemplate(tt.name)
			if tt.expectEmpty && tmpl != "" {
				t.Errorf("expected empty template for %q, got %q", tt.name, tmpl)
			}
			if !tt.expectEmpty && tmpl == "" {
				t.Errorf("expected non-empty template for %q", tt.name)
			}
		})
	}
}

func TestSetTemplate(t *testing.T) {
	injector := NewPromptInjector()

	customTemplate := "This is a custom template"
	injector.SetTemplate("custom", customTemplate)

	result := injector.GetTemplate("custom")
	if result != customTemplate {
		t.Errorf("expected template %q, got %q", customTemplate, result)
	}
}

func TestWithStaggerDelay(t *testing.T) {
	injector := NewPromptInjector()
	customDelay := 500 * time.Millisecond

	result := injector.WithStaggerDelay(customDelay)

	if result != injector {
		t.Error("WithStaggerDelay should return the same injector for chaining")
	}

	if injector.StaggerDelay != customDelay {
		t.Errorf("expected StaggerDelay of %v, got %v", customDelay, injector.StaggerDelay)
	}
}

func TestInjectionResult(t *testing.T) {
	now := time.Now()
	result := InjectionResult{
		SessionPane: "test:1.5",
		AgentType:   "cc",
		Success:     true,
		Duration:    100 * time.Millisecond,
		SentAt:      now,
	}

	if result.SessionPane != "test:1.5" {
		t.Errorf("unexpected SessionPane: %s", result.SessionPane)
	}
	if result.AgentType != "cc" {
		t.Errorf("unexpected AgentType: %s", result.AgentType)
	}
	if !result.Success {
		t.Error("expected Success to be true")
	}
	if result.Error != "" {
		t.Errorf("expected empty Error, got %q", result.Error)
	}
}

func TestInjectionTarget(t *testing.T) {
	target := InjectionTarget{
		SessionPane: "myproject:1.2",
		AgentType:   "cod",
	}

	if target.SessionPane != "myproject:1.2" {
		t.Errorf("unexpected SessionPane: %s", target.SessionPane)
	}
	if target.AgentType != "cod" {
		t.Errorf("unexpected AgentType: %s", target.AgentType)
	}
}

func TestBatchInjectionResult(t *testing.T) {
	result := BatchInjectionResult{
		TotalPanes: 5,
		Successful: 4,
		Failed:     1,
		Results: []InjectionResult{
			{SessionPane: "s:1.1", AgentType: "cc", Success: true},
			{SessionPane: "s:1.2", AgentType: "cc", Success: true},
			{SessionPane: "s:1.3", AgentType: "cod", Success: true},
			{SessionPane: "s:1.4", AgentType: "cod", Success: true},
			{SessionPane: "s:1.5", AgentType: "gmi", Success: false, Error: "test error"},
		},
		Duration: 2 * time.Second,
	}

	if result.TotalPanes != 5 {
		t.Errorf("expected TotalPanes of 5, got %d", result.TotalPanes)
	}
	if result.Successful != 4 {
		t.Errorf("expected Successful of 4, got %d", result.Successful)
	}
	if result.Failed != 1 {
		t.Errorf("expected Failed of 1, got %d", result.Failed)
	}
	if len(result.Results) != 5 {
		t.Errorf("expected 5 results, got %d", len(result.Results))
	}
}

func TestInjectSwarmNilPlan(t *testing.T) {
	injector := NewPromptInjector()
	result, err := injector.InjectSwarm(nil, "test prompt")

	if err == nil {
		t.Error("expected error for nil plan")
	}
	if result != nil {
		t.Error("expected nil result for nil plan")
	}
}

func TestInjectBatchEmpty(t *testing.T) {
	injector := NewPromptInjector()
	result, err := injector.InjectBatch([]InjectionTarget{}, "test prompt")

	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.TotalPanes != 0 {
		t.Errorf("expected TotalPanes of 0, got %d", result.TotalPanes)
	}
	if result.Successful != 0 {
		t.Errorf("expected Successful of 0, got %d", result.Successful)
	}
	if result.Failed != 0 {
		t.Errorf("expected Failed of 0, got %d", result.Failed)
	}
}

// GH#251 phase 2: grok prompt delivery is first-class — both the public
// InjectPrompt path and the direct sendToPane helper deliver to the (fake)
// tmux sender instead of refusing before any tmux call.
func TestPromptInjector_GrokDirectPathsDeliverViaTmux(t *testing.T) {
	client := &countingPromptInjectionTmuxClient{captureOutput: "ready>\n"}
	injector := NewPromptInjector()
	injector.tmuxClientOverride = client

	for _, tt := range []struct {
		name   string
		inject func() error
	}{
		{name: "public", inject: func() error {
			return injector.InjectPrompt("proj:1.2", "grok-build", "continue")
		}},
		{name: "direct helper", inject: func() error {
			return injector.sendToPane(context.Background(), "proj:1.2", "xai_grok_build", "continue")
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.inject(); err != nil {
				t.Fatalf("error = %v, want grok prompt delivered", err)
			}
		})
	}

	if client.submitCount != 2 {
		t.Fatalf("SubmitPrompt calls = %d, want 2 (one per grok delivery)", client.submitCount)
	}
	for _, send := range client.submitted {
		if send.target != "%2" {
			t.Fatalf("delivery pane = %q, want pinned physical pane %%2", send.target)
		}
	}
}

// GH#251 phase 2: a mixed batch containing grok now passes preflight and every
// pane — grok included — is delivered to, exactly like claude/codex.
func TestPromptInjector_MixedBatchDeliversToGrok(t *testing.T) {
	client := &countingPromptInjectionTmuxClient{captureOutput: "ready>\n"}
	injector := NewPromptInjector().WithStaggerDelay(0)
	injector.tmuxClientOverride = client
	targets := []InjectionTarget{
		{SessionPane: "proj:1.1", AgentType: "cc"},
		{SessionPane: "proj:1.2", AgentType: "grok"},
		{SessionPane: "proj:1.3", AgentType: "cod"},
	}

	result, err := injector.InjectBatchWithContext(context.Background(), targets, "continue")
	if err != nil {
		t.Fatalf("InjectBatchWithContext() error = %v, want mixed grok batch accepted", err)
	}
	if result == nil {
		t.Fatal("InjectBatchWithContext() returned nil result")
	}
	if result.TotalPanes != len(targets) || result.Successful != len(targets) || result.Failed != 0 || len(result.Results) != len(targets) {
		t.Fatalf("batch result = %+v, want all %d panes delivered", result, len(targets))
	}
	if client.submitCount != len(targets) {
		t.Fatalf("SubmitPrompt calls = %d, want %d (grok pane must be included)", client.submitCount, len(targets))
	}
	grokDelivered := false
	for _, send := range client.submitted {
		if send.target == "%2" {
			grokDelivered = true
		}
	}
	if !grokDelivered {
		t.Fatalf("grok pane proj:1.2 never received a send: %+v", client.submitted)
	}
}

func TestLoggerHelper(t *testing.T) {
	injector := NewPromptInjector()
	logger := injector.logger()

	if logger == nil {
		t.Error("expected non-nil logger from logger()")
	}
}

func TestDefaultMarchingOrdersNotEmpty(t *testing.T) {
	if DefaultMarchingOrders == "" {
		t.Error("DefaultMarchingOrders should not be empty")
	}

	// Check it contains key instructions
	if len(DefaultMarchingOrders) < 100 {
		t.Error("DefaultMarchingOrders seems too short")
	}
}

func TestPromptTemplateConstants(t *testing.T) {
	if ReviewTemplate == "" {
		t.Error("ReviewTemplate should not be empty")
	}
	if TestTemplate == "" {
		t.Error("TestTemplate should not be empty")
	}
}

func TestWithRateLimitTracker(t *testing.T) {
	injector := NewPromptInjector()
	tracker := ratelimit.NewRateLimitTracker("")

	result := injector.WithRateLimitTracker(tracker)

	if result != injector {
		t.Error("WithRateLimitTracker should return the same injector for chaining")
	}

	if injector.RateLimitTracker != tracker {
		t.Error("expected RateLimitTracker to be set")
	}
}

func TestWithAdaptiveDelay(t *testing.T) {
	injector := NewPromptInjector()

	result := injector.WithAdaptiveDelay(true)

	if result != injector {
		t.Error("WithAdaptiveDelay should return the same injector for chaining")
	}

	if !injector.UseAdaptiveDelay {
		t.Error("expected UseAdaptiveDelay to be true")
	}

	injector.WithAdaptiveDelay(false)
	if injector.UseAdaptiveDelay {
		t.Error("expected UseAdaptiveDelay to be false")
	}
}

func TestGetDelayForAgentFixed(t *testing.T) {
	injector := NewPromptInjector().WithStaggerDelay(500 * time.Millisecond)

	delay := injector.GetDelayForAgent("cc")

	if delay != 500*time.Millisecond {
		t.Errorf("expected delay of 500ms, got %v", delay)
	}
}

func TestGetDelayForAgentAdaptive(t *testing.T) {
	tracker := ratelimit.NewRateLimitTracker("")
	injector := NewPromptInjector().
		WithRateLimitTracker(tracker).
		WithAdaptiveDelay(true)

	// Default anthropic delay is 15s
	delay := injector.GetDelayForAgent("cc")

	// Should use tracker's optimal delay (defaults to 15s for anthropic)
	if delay != ratelimit.DefaultDelayAnthropic {
		t.Errorf("expected delay of %v for cc, got %v", ratelimit.DefaultDelayAnthropic, delay)
	}

	// Test with openai alias
	delay = injector.GetDelayForAgent("cod")
	if delay != ratelimit.DefaultDelayOpenAI {
		t.Errorf("expected delay of %v for cod, got %v", ratelimit.DefaultDelayOpenAI, delay)
	}
}

func TestRecordSuccess(t *testing.T) {
	tracker := ratelimit.NewRateLimitTracker("")
	injector := NewPromptInjector().
		WithRateLimitTracker(tracker).
		WithAdaptiveDelay(true)

	// Record multiple successes
	for i := 0; i < 10; i++ {
		injector.recordSuccess("cc")
	}

	// Check that tracker recorded the successes
	state := tracker.GetProviderState("anthropic")
	if state == nil {
		t.Fatal("expected provider state to exist")
	}

	if state.TotalSuccesses != 10 {
		t.Errorf("expected 10 successes, got %d", state.TotalSuccesses)
	}
}

func TestRecordSuccessNoOpWithoutTracker(t *testing.T) {
	injector := NewPromptInjector().WithAdaptiveDelay(true)

	// Should not panic when tracker is nil
	injector.recordSuccess("cc")
}

func TestRecordSuccessNoOpWithoutAdaptiveDelay(t *testing.T) {
	tracker := ratelimit.NewRateLimitTracker("")
	injector := NewPromptInjector().
		WithRateLimitTracker(tracker).
		WithAdaptiveDelay(false)

	injector.recordSuccess("cc")

	// Should not record anything when adaptive delay is disabled
	state := tracker.GetProviderState("anthropic")
	if state != nil {
		t.Error("expected no provider state when adaptive delay is disabled")
	}
}

func TestInjectBatchWithContextCancellation(t *testing.T) {
	injector := NewPromptInjector()

	// Create a cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	targets := []InjectionTarget{
		{SessionPane: "test:1.1", AgentType: "cc"},
		{SessionPane: "test:1.2", AgentType: "cc"},
	}

	result, err := injector.InjectBatchWithContext(ctx, targets, "test prompt")

	if err == nil {
		t.Error("expected error for cancelled context")
	}

	if result == nil {
		t.Fatal("expected non-nil result")
	}
}

func TestInjectSwarmWithContext(t *testing.T) {
	injector := NewPromptInjector()

	// Create a cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	plan := &SwarmPlan{
		Sessions: []SessionSpec{
			{
				Name:      "test",
				AgentType: "cc",
				PaneCount: 2,
				Panes: []PaneSpec{
					{Index: 1, AgentType: "cc"},
					{Index: 2, AgentType: "cc"},
				},
			},
		},
	}

	result, err := injector.InjectSwarmWithContext(ctx, plan, "test prompt")

	if err == nil {
		t.Error("expected error for cancelled context")
	}

	if result == nil {
		t.Fatal("expected non-nil result")
	}
}

// TestPromptInjector_StaggeredDeliveryTiming tests the timing of staggered delivery.
// It verifies that the stagger delay is actually applied between sends.
func TestPromptInjector_StaggeredDeliveryTiming(t *testing.T) {
	t.Log("[TEST] Starting staggered delivery timing test")

	injector := NewPromptInjector().WithStaggerDelay(50 * time.Millisecond)

	t.Logf("[TEST] Configured stagger delay: %v", injector.StaggerDelay)
	t.Logf("[TEST] Expected minimum total time for 3 panes with 2 gaps: %v", 2*50*time.Millisecond)

	// We can't test actual tmux sends, but we can verify the delay configuration
	if injector.StaggerDelay != 50*time.Millisecond {
		t.Errorf("[TEST] expected stagger delay of 50ms, got %v", injector.StaggerDelay)
	}

	// Verify delay is returned correctly for different agent types
	delays := map[string]time.Duration{}
	for _, agent := range []string{"cc", "cod", "gmi"} {
		delay := injector.GetDelayForAgent(agent)
		delays[agent] = delay
		t.Logf("[TEST] Delay for %s: %v", agent, delay)
	}

	// All should use the same fixed delay when adaptive is disabled
	for agent, delay := range delays {
		if delay != 50*time.Millisecond {
			t.Errorf("[TEST] expected delay of 50ms for %s, got %v", agent, delay)
		}
	}
}

// TestPromptInjector_ReviewModePrompts tests the review template content.
func TestPromptInjector_ReviewModePrompts(t *testing.T) {
	t.Log("[TEST] Testing review mode prompts")

	injector := NewPromptInjector()

	// Get the review template
	reviewPrompt := injector.GetTemplate("review")
	t.Logf("[TEST] Review template length: %d chars", len(reviewPrompt))
	t.Logf("[TEST] Review template first 50 chars: %q", reviewPrompt[:min(50, len(reviewPrompt))])

	// Verify review template contains expected content
	requiredPhrases := []string{
		"review",
		"git",
	}

	for _, phrase := range requiredPhrases {
		t.Logf("[TEST] Checking for phrase: %q", phrase)
		if !containsIgnoreCase(reviewPrompt, phrase) {
			t.Errorf("[TEST] Review template missing expected phrase: %q", phrase)
		}
	}

	// Test template contains actionable instructions
	if len(reviewPrompt) < 50 {
		t.Error("[TEST] Review template seems too short to be useful")
	}
}

// TestPromptInjector_TestModePrompts tests the test template content.
func TestPromptInjector_TestModePrompts(t *testing.T) {
	t.Log("[TEST] Testing test mode prompts")

	injector := NewPromptInjector()

	// Get the test template
	testPrompt := injector.GetTemplate("test")
	t.Logf("[TEST] Test template length: %d chars", len(testPrompt))

	// Verify test template contains expected content
	requiredPhrases := []string{
		"test",
	}

	for _, phrase := range requiredPhrases {
		t.Logf("[TEST] Checking for phrase: %q", phrase)
		if !containsIgnoreCase(testPrompt, phrase) {
			t.Errorf("[TEST] Test template missing expected phrase: %q", phrase)
		}
	}
}

// TestPromptInjector_DefaultMarchingOrdersContent tests the default marching orders.
func TestPromptInjector_DefaultMarchingOrdersContent(t *testing.T) {
	t.Log("[TEST] Testing default marching orders content")

	injector := NewPromptInjector()

	defaultPrompt := injector.GetTemplate("default")
	t.Logf("[TEST] Default template length: %d chars", len(defaultPrompt))

	// Verify default template contains expected content
	requiredPhrases := []string{
		"AGENTS.md",
		"bv",
		"br",
	}

	for _, phrase := range requiredPhrases {
		t.Logf("[TEST] Checking for phrase: %q", phrase)
		if !containsIgnoreCase(defaultPrompt, phrase) {
			t.Errorf("[TEST] Default template missing expected phrase: %q", phrase)
		}
	}

	// Verify it instructs agents to understand the codebase
	if !containsIgnoreCase(defaultPrompt, "understand") {
		t.Error("[TEST] Default template should instruct agents to understand the codebase")
	}
}

// TestPromptInjector_AdaptiveDelayWithDifferentProviders tests adaptive delay for different providers.
func TestPromptInjector_AdaptiveDelayWithDifferentProviders(t *testing.T) {
	t.Log("[TEST] Testing adaptive delay with different providers")

	tracker := ratelimit.NewRateLimitTracker("")
	injector := NewPromptInjector().
		WithRateLimitTracker(tracker).
		WithAdaptiveDelay(true)

	t.Log("[TEST] Adaptive delay enabled with rate limit tracker")

	// Test delays for each provider type
	testCases := []struct {
		agentType    string
		expectedFunc func() time.Duration
		providerName string
	}{
		{
			agentType:    "cc",
			expectedFunc: func() time.Duration { return ratelimit.DefaultDelayAnthropic },
			providerName: "Anthropic (Claude)",
		},
		{
			agentType:    "cod",
			expectedFunc: func() time.Duration { return ratelimit.DefaultDelayOpenAI },
			providerName: "OpenAI (Codex)",
		},
		{
			agentType:    "gmi",
			expectedFunc: func() time.Duration { return ratelimit.DefaultDelayGoogle },
			providerName: "Google (Gemini)",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.agentType, func(t *testing.T) {
			t.Logf("[TEST] Testing %s (%s)", tc.providerName, tc.agentType)

			delay := injector.GetDelayForAgent(tc.agentType)
			expected := tc.expectedFunc()

			t.Logf("[TEST] Delay for %s: %v (expected: %v)", tc.agentType, delay, expected)

			if delay != expected {
				t.Errorf("[TEST] Expected delay %v for %s, got %v", expected, tc.agentType, delay)
			}
		})
	}
}

// TestPromptInjector_InjectSwarmWithTemplate tests injection with named templates.
func TestPromptInjector_InjectSwarmWithTemplate(t *testing.T) {
	t.Log("[TEST] Testing InjectSwarmWithTemplate")

	injector := NewPromptInjector()

	// Create a cancelled context to avoid actual tmux calls
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	plan := &SwarmPlan{
		Sessions: []SessionSpec{
			{
				Name:      "test",
				AgentType: "cc",
				PaneCount: 1,
				Panes: []PaneSpec{
					{Index: 1, AgentType: "cc"},
				},
			},
		},
	}

	// Test with "review" template
	t.Log("[TEST] Testing with 'review' template")
	_, err := injector.InjectSwarmWithContext(ctx, plan, injector.GetTemplate("review"))
	if err == nil {
		t.Log("[TEST] Note: no error expected here would mean actual tmux calls succeeded")
	}

	// Test with "test" template
	t.Log("[TEST] Testing with 'test' template")
	_, err = injector.InjectSwarmWithContext(ctx, plan, injector.GetTemplate("test"))
	if err == nil {
		t.Log("[TEST] Note: no error expected here would mean actual tmux calls succeeded")
	}
}

// TestPromptInjector_BatchResultTracking tests batch injection result tracking.
func TestPromptInjector_BatchResultTracking(t *testing.T) {
	t.Log("[TEST] Testing batch injection result tracking")

	result := BatchInjectionResult{
		TotalPanes: 4,
		Successful: 3,
		Failed:     1,
		Results: []InjectionResult{
			{SessionPane: "proj:1.1", AgentType: "cc", Success: true, Duration: 100 * time.Millisecond},
			{SessionPane: "proj:1.2", AgentType: "cod", Success: true, Duration: 150 * time.Millisecond},
			{SessionPane: "proj:1.3", AgentType: "gmi", Success: true, Duration: 200 * time.Millisecond},
			{SessionPane: "proj:1.4", AgentType: "cc", Success: false, Error: "tmux: no such pane"},
		},
		Duration: 600 * time.Millisecond,
	}

	t.Logf("[TEST] Total panes: %d", result.TotalPanes)
	t.Logf("[TEST] Successful: %d", result.Successful)
	t.Logf("[TEST] Failed: %d", result.Failed)
	t.Logf("[TEST] Total duration: %v", result.Duration)

	// Verify counts
	if result.TotalPanes != result.Successful+result.Failed {
		t.Errorf("[TEST] TotalPanes (%d) != Successful (%d) + Failed (%d)",
			result.TotalPanes, result.Successful, result.Failed)
	}

	// Verify individual results
	successCount := 0
	failCount := 0
	for i, r := range result.Results {
		t.Logf("[TEST] Result[%d]: pane=%s, agent=%s, success=%v, duration=%v",
			i, r.SessionPane, r.AgentType, r.Success, r.Duration)
		if r.Success {
			successCount++
		} else {
			failCount++
			t.Logf("[TEST] Error for %s: %s", r.SessionPane, r.Error)
		}
	}

	if successCount != result.Successful {
		t.Errorf("[TEST] Counted successes (%d) != reported Successful (%d)", successCount, result.Successful)
	}
	if failCount != result.Failed {
		t.Errorf("[TEST] Counted failures (%d) != reported Failed (%d)", failCount, result.Failed)
	}
}

// containsIgnoreCase checks if s contains substr (case-insensitive).
func containsIgnoreCase(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr ||
		len(substr) == 0 ||
		(len(s) > 0 && containsLower(toLower(s), toLower(substr))))
}

func toLower(s string) string {
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}

func containsLower(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestPromptInjectorTmuxClient(t *testing.T) {
	injector := NewPromptInjector()
	client := injector.tmuxClient()

	if client == nil {
		t.Error("expected non-nil client from tmuxClient()")
	}
}

func TestWithLogger(t *testing.T) {
	injector := NewPromptInjector()
	result := injector.WithLogger(nil)

	if result != injector {
		t.Error("WithLogger should return the same injector for chaining")
	}
}

func TestPromptInjector_ChainedConfiguration(t *testing.T) {
	t.Log("[TEST] Testing chained configuration methods")

	tracker := ratelimit.NewRateLimitTracker("")

	injector := NewPromptInjector().
		WithStaggerDelay(250 * time.Millisecond).
		WithRateLimitTracker(tracker).
		WithAdaptiveDelay(true).
		WithLogger(nil)

	t.Log("[TEST] Created injector with chained configuration")
	t.Logf("[TEST] StaggerDelay: %v", injector.StaggerDelay)
	t.Logf("[TEST] UseAdaptiveDelay: %v", injector.UseAdaptiveDelay)
	t.Logf("[TEST] RateLimitTracker set: %v", injector.RateLimitTracker != nil)

	// Verify all configurations were applied
	if injector.StaggerDelay != 250*time.Millisecond {
		t.Errorf("[TEST] Expected StaggerDelay of 250ms, got %v", injector.StaggerDelay)
	}
	if !injector.UseAdaptiveDelay {
		t.Error("[TEST] Expected UseAdaptiveDelay to be true")
	}
	if injector.RateLimitTracker != tracker {
		t.Error("[TEST] Expected RateLimitTracker to be set")
	}
}
