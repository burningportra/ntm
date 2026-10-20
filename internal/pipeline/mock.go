package pipeline

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/dispatch"
	"github.com/Dicklesworthstone/ntm/internal/tmux"
)

// TmuxClient is the narrow tmux surface the pipeline executor needs.
// Production executors use realTmuxClient; tests can install MockTmuxClient
// with Executor.SetTmuxClient to avoid touching a live tmux server.
type TmuxClient interface {
	GetPanes(session string) ([]tmux.Pane, error)
	// SubmitPrompt types prompt into target and submits it the way a pane
	// of agentType needs.
	SubmitPrompt(ctx context.Context, target string, agentType tmux.AgentType, prompt string) error
	CapturePaneOutput(target string, lines int) (string, error)
}

type realTmuxClient struct{}

func (realTmuxClient) GetPanes(session string) ([]tmux.Pane, error) {
	return tmux.GetPanes(session)
}

// SubmitPrompt goes through dispatch.TMUXDeliverer.SubmitPrompt, the submit
// path ntm send uses: typed with send-keys and a 100 ms Enter, a multi-line
// prompt reached Claude and Codex as keystrokes and stayed an unsent draft in
// Muse.
func (realTmuxClient) SubmitPrompt(ctx context.Context, target string, agentType tmux.AgentType, prompt string) error {
	return dispatch.TMUXDeliverer{}.SubmitPrompt(ctx, target, agentType, prompt)
}

func (realTmuxClient) CapturePaneOutput(target string, lines int) (string, error) {
	return tmux.CapturePaneOutput(target, lines)
}

// MockTmuxPaste records one SubmitPrompt call made against the mock.
type MockTmuxPaste struct {
	Target    string
	Content   string
	AgentType tmux.AgentType
}

type mockTmuxPaneState struct {
	session string
	pane    tmux.Pane
	output  string
	pastes  []MockTmuxPaste
}

// MockTmuxClient is a deterministic in-memory tmux substitute for executor tests.
type MockTmuxClient struct {
	mu           sync.Mutex
	panes        map[string]*mockTmuxPaneState
	scripter     *AgentScripter
	deliveryGate <-chan struct{}
	deliveryWG   sync.WaitGroup
}

// NewMockTmuxClient creates a mock with optional global pane fixtures.
func NewMockTmuxClient(panes ...tmux.Pane) *MockTmuxClient {
	m := &MockTmuxClient{panes: make(map[string]*mockTmuxPaneState)}
	for _, pane := range panes {
		m.AddPane("", pane)
	}
	return m
}

// AddPane registers a pane fixture. An empty session makes the pane visible
// to every GetPanes call, which keeps single-session tests lightweight.
func (m *MockTmuxClient) AddPane(session string, pane tmux.Pane) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensure()
	pane = normalizeMockPane(pane, len(m.panes)+1)
	m.panes[pane.ID] = &mockTmuxPaneState{session: session, pane: pane}
}

// SetAgentScripter installs scripted responses for future SubmitPrompt calls.
func (m *MockTmuxClient) SetAgentScripter(scripter *AgentScripter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scripter = scripter
}

func (m *MockTmuxClient) setScriptedDeliveryGate(gate <-chan struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deliveryGate = gate
}

// Reset clears captured output and paste history while preserving pane fixtures.
func (m *MockTmuxClient) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, state := range m.panes {
		state.output = ""
		state.pastes = nil
	}
	if m.scripter != nil {
		m.scripter.Reset()
	}
}

// SetPaneOutput replaces a pane's captured output buffer.
func (m *MockTmuxClient) SetPaneOutput(target, output string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.panes[target]
	if !ok {
		return fmt.Errorf("mock tmux pane %q not found", target)
	}
	state.output = output
	return nil
}

// AppendPaneOutput appends content to a pane's captured output buffer.
func (m *MockTmuxClient) AppendPaneOutput(target, output string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.panes[target]
	if !ok {
		return fmt.Errorf("mock tmux pane %q not found", target)
	}
	state.output += output
	return nil
}

// PasteHistory returns a copy of the recorded SubmitPrompt calls.
func (m *MockTmuxClient) PasteHistory(target string) ([]MockTmuxPaste, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.panes[target]
	if !ok {
		return nil, fmt.Errorf("mock tmux pane %q not found", target)
	}
	history := make([]MockTmuxPaste, len(state.pastes))
	copy(history, state.pastes)
	return history, nil
}

// GetPanes returns the configured panes for a session.
func (m *MockTmuxClient) GetPanes(session string) ([]tmux.Pane, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensure()

	panes := make([]tmux.Pane, 0, len(m.panes))
	for _, state := range m.panes {
		if state.session == "" || state.session == session {
			panes = append(panes, state.pane)
		}
	}
	sort.Slice(panes, func(i, j int) bool {
		if panes[i].Index == panes[j].Index {
			return panes[i].ID < panes[j].ID
		}
		return panes[i].Index < panes[j].Index
	})
	return panes, nil
}

// SubmitPrompt records the prompt with its agent type and appends it, with
// the submitting newline, to the target pane's output.
func (m *MockTmuxClient) SubmitPrompt(_ context.Context, target string, agentType tmux.AgentType, content string) error {
	m.mu.Lock()
	_, ok := m.panes[target]
	scripter := m.scripter
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("mock tmux pane %q not found", target)
	}

	var response string
	var delay time.Duration
	var generation int64
	hasScriptedResponse := false
	if scripter != nil {
		var err error
		response, delay, generation, err = scripter.nextResponse(content)
		if err != nil {
			return err
		}
		hasScriptedResponse = true
	}

	m.mu.Lock()
	state, ok := m.panes[target]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("mock tmux pane %q not found", target)
	}
	state.pastes = append(state.pastes, MockTmuxPaste{
		Target:    target,
		Content:   content,
		AgentType: agentType,
	})
	state.output += content + "\n"
	m.mu.Unlock()

	if hasScriptedResponse {
		m.deliverScriptedResponse(target, response, delay, scripter, generation)
	}
	return nil
}

// CapturePaneOutput returns the target pane's output, optionally tailed by line count.
func (m *MockTmuxClient) CapturePaneOutput(target string, lines int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.panes[target]
	if !ok {
		return "", fmt.Errorf("mock tmux pane %q not found", target)
	}
	return tailMockLines(state.output, lines), nil
}

func (m *MockTmuxClient) deliverScriptedResponse(target, response string, delay time.Duration, scripter *AgentScripter, generation int64) {
	m.mu.Lock()
	deliveryGate := m.deliveryGate
	m.mu.Unlock()

	deliver := func() {
		if deliveryGate != nil {
			<-deliveryGate
		}
		// Drop stale deliveries (bd-05x7b): if Reset bumped the scripter
		// generation since this response was scheduled, the goroutine
		// belongs to a previous test phase and must not contaminate the
		// current pane state or produced count.
		m.mu.Lock()
		defer m.mu.Unlock()
		if state, ok := m.panes[target]; ok {
			scripter.deliverIfCurrent(generation, func() {
				state.output += response
			})
		}
	}
	if delay <= 0 {
		deliver()
		return
	}
	m.deliveryWG.Add(1)
	go func() {
		defer m.deliveryWG.Done()
		timer := time.NewTimer(delay)
		defer timer.Stop()
		<-timer.C
		deliver()
	}()
}

func (m *MockTmuxClient) waitForScriptedDeliveries() {
	m.deliveryWG.Wait()
}

func (m *MockTmuxClient) ensure() {
	if m.panes == nil {
		m.panes = make(map[string]*mockTmuxPaneState)
	}
}

func normalizeMockPane(pane tmux.Pane, ordinal int) tmux.Pane {
	if pane.ID == "" {
		pane.ID = fmt.Sprintf("%%%d", ordinal)
	}
	if pane.Index == 0 {
		pane.Index = ordinal
	}
	return pane
}

func tailMockLines(output string, lines int) string {
	if lines <= 0 || output == "" {
		return output
	}
	parts := strings.SplitAfter(output, "\n")
	if parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	if lines >= len(parts) {
		return output
	}
	return strings.Join(parts[len(parts)-lines:], "")
}

type agentScriptMatchKind int

const (
	agentScriptSubstring agentScriptMatchKind = iota
	agentScriptRegex
)

type agentScriptRule struct {
	kind     agentScriptMatchKind
	pattern  string
	regex    *regexp.Regexp
	response string
	used     bool
}

// AgentScripter provides scriptable mock-agent responses for MockTmuxClient.
//
// generation is bumped on every Reset so delayed responses scheduled before
// the reset can detect that they belong to a stale generation and silently
// drop their delivery (bd-05x7b). Delivery rechecks it under the rule mutex so
// generation validation, output append, and produced-count updates are atomic
// with Reset.
type AgentScripter struct {
	mu              sync.Mutex
	rules           []agentScriptRule
	defaultResponse string
	hasDefault      bool
	delay           time.Duration
	produced        int
	generation      int64
}

// NewAgentScripter creates an empty script table.
func NewAgentScripter() *AgentScripter {
	return &AgentScripter{}
}

// Match adds a one-shot substring response rule.
func (s *AgentScripter) Match(pattern, response string) *AgentScripter {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = append(s.rules, agentScriptRule{
		kind:     agentScriptSubstring,
		pattern:  pattern,
		response: response,
	})
	return s
}

// MatchRegex adds a one-shot regular-expression response rule.
func (s *AgentScripter) MatchRegex(pattern, response string) error {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = append(s.rules, agentScriptRule{
		kind:     agentScriptRegex,
		pattern:  pattern,
		regex:    re,
		response: response,
	})
	return nil
}

// Default sets the fallback response used when no one-shot rule matches.
func (s *AgentScripter) Default(response string) *AgentScripter {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.defaultResponse = response
	s.hasDefault = true
	return s
}

// Delay sets the delay before future scripted responses are appended.
func (s *AgentScripter) Delay(delay time.Duration) *AgentScripter {
	if delay < 0 {
		delay = 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delay = delay
	return s
}

// Reset marks all one-shot rules unused and resets the produced-response
// count. Bumps the generation counter so any in-flight delayed response
// goroutines drop their delivery instead of contaminating the fresh state.
func (s *AgentScripter) Reset() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	atomic.AddInt64(&s.generation, 1)
	for i := range s.rules {
		s.rules[i].used = false
	}
	s.produced = 0
}

// deliverIfCurrent commits a scripted response only if captured still belongs
// to the live generation. The output append and produced counter update share
// the rule mutex with Reset, so a direct AgentScripter.Reset call is an atomic
// barrier against in-flight delivery.
func (s *AgentScripter) deliverIfCurrent(captured int64, deliver func()) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if atomic.LoadInt64(&s.generation) != captured {
		return false
	}
	deliver()
	s.produced++
	return true
}

// Wait blocks until at least count scripted responses have been appended.
func (s *AgentScripter) Wait(ctx context.Context, count int) error {
	if count <= 0 {
		return nil
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		s.mu.Lock()
		produced := s.produced
		s.mu.Unlock()
		if produced >= count {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %d scripted responses: produced %d: %w", count, produced, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (s *AgentScripter) producedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.produced
}

// nextResponse returns the next scripted response, the configured delay,
// and the generation number that was current when the response was matched.
// The generation is captured here and re-checked under the same mutex by
// deliverIfCurrent so a Reset between match and delivery invalidates the
// response atomically with its output and produced-count updates.
func (s *AgentScripter) nextResponse(prompt string) (string, time.Duration, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	generation := atomic.LoadInt64(&s.generation)
	for i := range s.rules {
		rule := &s.rules[i]
		if rule.used {
			continue
		}
		if rule.matches(prompt) {
			rule.used = true
			return rule.response, s.delay, generation, nil
		}
	}
	if s.hasDefault {
		return s.defaultResponse, s.delay, generation, nil
	}
	return "", 0, 0, fmt.Errorf("mock agent script has no response for prompt %q", truncatePrompt(prompt, 120))
}

func (r agentScriptRule) matches(prompt string) bool {
	switch r.kind {
	case agentScriptRegex:
		return r.regex != nil && r.regex.MatchString(prompt)
	default:
		return strings.Contains(prompt, r.pattern)
	}
}
