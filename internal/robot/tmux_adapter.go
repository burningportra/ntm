// Package robot provides machine-readable output for AI agents.
// tmux_adapter.go normalizes tmux session/pane data into canonical projection shapes.
//
// This adapter transforms raw tmux data into RuntimeSession and RuntimeAgent
// structures suitable for robot surfaces. It insulates the rest of the robot
// stack from tmux-specific mechanics while preserving ntm's tmux-first nature.
//
// Bead: bd-j9jo3.3.1
package robot

import (
	"fmt"
	"strings"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/state"
	"github.com/Dicklesworthstone/ntm/internal/tmux"
)

// =============================================================================
// Configuration
// =============================================================================

// TmuxAdapterConfig controls the normalization behavior.
type TmuxAdapterConfig struct {
	// SessionStaleness is how long session projections are considered fresh.
	// Default: 5 seconds
	SessionStaleness time.Duration

	// AgentStaleness is how long agent projections are considered fresh.
	// Default: 5 seconds
	AgentStaleness time.Duration
}

// DefaultTmuxAdapterConfig returns sensible defaults.
func DefaultTmuxAdapterConfig() TmuxAdapterConfig {
	return TmuxAdapterConfig{
		SessionStaleness: 5 * time.Second,
		AgentStaleness:   5 * time.Second,
	}
}

// =============================================================================
// Adapter
// =============================================================================

// TmuxAdapter normalizes tmux data into canonical projection shapes.
type TmuxAdapter struct {
	config TmuxAdapterConfig
}

// NewTmuxAdapter creates a new adapter with the given configuration.
func NewTmuxAdapter(config TmuxAdapterConfig) *TmuxAdapter {
	return &TmuxAdapter{config: config}
}

// =============================================================================
// Session Normalization
// =============================================================================

// NormalizeSession transforms a tmux.Session into a RuntimeSession.
func (a *TmuxAdapter) NormalizeSession(sess *tmux.Session, agents []Agent) *state.RuntimeSession {
	now := time.Now()

	// Parse creation time
	var createdAt *time.Time
	if sess.Created != "" {
		if t, err := time.Parse(time.RFC3339, sess.Created); err == nil {
			createdAt = &t
		}
	}

	// Count agent states
	var activeAgents, idleAgents, errorAgents int
	var lastActivity time.Time
	for _, agent := range agents {
		agentState := a.classifyAgentState(&agent)
		switch agentState {
		case state.AgentStateBusy:
			activeAgents++
		case state.AgentStateIdle:
			idleAgents++
		case state.AgentStateError:
			errorAgents++
		}
		if !agent.LastOutputTS.IsZero() && agent.LastOutputTS.After(lastActivity) {
			lastActivity = agent.LastOutputTS
		}
	}

	// Compute health status
	healthStatus, healthReason := a.computeSessionHealth(sess, agents, errorAgents)

	// Track last attached time (approximation: if attached, it's now)
	var lastAttachedAt *time.Time
	if sess.Attached {
		lastAttachedAt = &now
	}

	var lastActivityAt *time.Time
	if !lastActivity.IsZero() {
		lastActivityAt = &lastActivity
	}

	return &state.RuntimeSession{
		Name:           sess.Name,
		Label:          extractSessionLabel(sess.Name),
		ProjectPath:    sess.Directory,
		Attached:       sess.Attached,
		WindowCount:    sess.Windows,
		PaneCount:      len(sess.Panes),
		AgentCount:     len(agents),
		ActiveAgents:   activeAgents,
		IdleAgents:     idleAgents,
		ErrorAgents:    errorAgents,
		HealthStatus:   healthStatus,
		HealthReason:   healthReason,
		CreatedAt:      createdAt,
		LastAttachedAt: lastAttachedAt,
		LastActivityAt: lastActivityAt,
		CollectedAt:    now,
		StaleAfter:     now.Add(a.config.SessionStaleness),
	}
}

// NormalizeSessions transforms multiple tmux sessions.
func (a *TmuxAdapter) NormalizeSessions(sessions []tmux.Session, agentsBySession map[string][]Agent) []state.RuntimeSession {
	result := make([]state.RuntimeSession, 0, len(sessions))
	for i := range sessions {
		agents := agentsBySession[sessions[i].Name]
		rs := a.NormalizeSession(&sessions[i], agents)
		result = append(result, *rs)
	}
	return result
}

// =============================================================================
// Agent Normalization
// =============================================================================

// NormalizeAgent transforms a robot.Agent into a RuntimeAgent.
func (a *TmuxAdapter) NormalizeAgent(sessionName string, agent *Agent) *state.RuntimeAgent {
	now := time.Now()

	// Compute agent ID
	agentID := fmt.Sprintf("%s:%s", sessionName, agent.Pane)

	agentState := a.classifyAgentState(agent)
	stateReason := a.classifyStateReason(agent, agentState)

	// Compute health
	healthStatus, healthReason := a.computeAgentHealth(agent, agentState)

	// Track last output
	var lastOutputAt *time.Time
	lastOutputAgeSec := -1
	if !agent.LastOutputTS.IsZero() {
		lastOutputAt = &agent.LastOutputTS
		lastOutputAgeSec = agent.SecondsSinceOutput
	}

	// Type confidence based on detection method
	typeConfidence, typeMethod := a.computeTypeConfidence(agent)

	return &state.RuntimeAgent{
		ID:               agentID,
		SessionName:      sessionName,
		Pane:             agent.Pane,
		AgentType:        normalizeAgentType(agent.Type),
		Variant:          agent.Variant,
		TypeConfidence:   typeConfidence,
		TypeMethod:       typeMethod,
		State:            agentState,
		StateReason:      stateReason,
		PreviousState:    "", // Would need state tracking
		StateChangedAt:   nil,
		LastOutputAt:     lastOutputAt,
		LastOutputAgeSec: lastOutputAgeSec,
		OutputTailLines:  agent.OutputLinesSinceLast,
		CurrentBead:      "", // Would need external enrichment
		PendingMail:      0,  // Would need Agent Mail integration
		AgentMailName:    agent.Name,
		CaptureError:     agent.CaptureError,
		HealthStatus:     healthStatus,
		HealthReason:     healthReason,
		CollectedAt:      now,
		StaleAfter:       now.Add(a.config.AgentStaleness),
	}
}

// NormalizeAgents transforms multiple robot.Agent structs.
func (a *TmuxAdapter) NormalizeAgents(sessionName string, agents []Agent) []state.RuntimeAgent {
	result := make([]state.RuntimeAgent, 0, len(agents))
	for i := range agents {
		ra := a.NormalizeAgent(sessionName, &agents[i])
		result = append(result, *ra)
	}
	return result
}

// =============================================================================
// State Classification
// =============================================================================

// classifyAgentState determines the agent's state. A zombie or stopped agent
// process is an error whatever its pane shows. Otherwise the state is the
// --robot-is-working verdict the collector attached from the pane's one
// capture (bd-p7es6). The adapter used to guess from output timing, and since
// a process's first look at a pane counted as fresh output, every pane in a
// one-shot CLI call came out busy.
func (a *TmuxAdapter) classifyAgentState(agent *Agent) state.AgentState {
	agentState := runtimeAgentState(agent.Type, agent.work)
	if agentState == state.AgentStateUser {
		return agentState
	}
	if processStoppedOrZombie(agent.ProcessState) {
		return state.AgentStateError
	}
	return agentState
}

// classifyStateReason explains the state in a short phrase.
func (a *TmuxAdapter) classifyStateReason(agent *Agent, agentState state.AgentState) string {
	switch {
	case agentState == state.AgentStateUser:
		return "user pane"
	case agentState == state.AgentStateError && agent.ProcessState == "Z":
		return "process zombie"
	case agentState == state.AgentStateError && agent.ProcessState == "T":
		return "process stopped"
	case agent.work != nil:
		return fmt.Sprintf("%s (%s)", agent.work.RecommendationReason, agent.work.IndicatorBasis)
	case agent.CaptureError != "":
		return "pane output unavailable: " + agent.CaptureError
	default:
		return "insufficient signals"
	}
}

// processStoppedOrZombie reports whether a ps state code means the process can
// no longer run: Z is a zombie, T is stopped or traced.
func processStoppedOrZombie(processState string) bool {
	return processState == "Z" || processState == "T"
}

// =============================================================================
// Health Computation
// =============================================================================

// computeSessionHealth determines overall session health.
func (a *TmuxAdapter) computeSessionHealth(sess *tmux.Session, agents []Agent, errorCount int) (state.HealthStatus, string) {
	if len(agents) == 0 {
		return state.HealthStatusUnknown, "no agents"
	}

	if errorCount > 0 {
		if errorCount == len(agents) {
			return state.HealthStatusCritical, fmt.Sprintf("all %d agents in error state", errorCount)
		}
		return state.HealthStatusWarning, fmt.Sprintf("%d/%d agents in error state", errorCount, len(agents))
	}

	// Check for rate limiting
	rateLimited := 0
	for _, agent := range agents {
		if agent.RateLimitDetected {
			rateLimited++
		}
	}
	if rateLimited > 0 {
		return state.HealthStatusWarning, fmt.Sprintf("%d agents rate limited", rateLimited)
	}

	return state.HealthStatusHealthy, ""
}

// computeAgentHealth determines individual agent health.
func (a *TmuxAdapter) computeAgentHealth(agent *Agent, agentState state.AgentState) (state.HealthStatus, string) {
	switch agentState {
	case state.AgentStateError:
		if agent.RateLimitDetected {
			return state.HealthStatusWarning, "rate limited"
		}
		return state.HealthStatusCritical, "error state"

	case state.AgentStateCompacting:
		return state.HealthStatusWarning, "context compacting"

	case state.AgentStateUnknown:
		return state.HealthStatusUnknown, "state unknown"

	default:
		// Check context usage
		if agent.ContextPercent > 80 {
			return state.HealthStatusWarning, fmt.Sprintf("context at %.0f%%", agent.ContextPercent)
		}
		return state.HealthStatusHealthy, ""
	}
}

// =============================================================================
// Type Detection
// =============================================================================

// computeTypeConfidence returns confidence and detection method.
func (a *TmuxAdapter) computeTypeConfidence(agent *Agent) (float64, string) {
	// If we have child PID, high confidence from process inspection
	if agent.ChildPID > 0 {
		return 0.95, "process"
	}

	// If we have a pane title with type, medium-high confidence
	if agent.Type != "" && agent.Type != "user" && agent.Type != "unknown" {
		return 0.85, "title"
	}

	// Low confidence fallback
	return 0.5, "heuristic"
}

// Note: normalizeAgentType is already defined in robot.go

// =============================================================================
// Helpers
// =============================================================================

// extractSessionLabel extracts a human-friendly label from session name.
// For example: "myproject" from "myproject" or "myproject" from "myproject__cc_1"
func extractSessionLabel(name string) string {
	// Strip NTM pane suffix if present
	if idx := strings.Index(name, "__"); idx > 0 {
		return name[:idx]
	}
	return name
}

// =============================================================================
// Batch Normalization
// =============================================================================

// NormalizedSnapshot holds normalized data for a full tmux snapshot.
type NormalizedSnapshot struct {
	Sessions    []state.RuntimeSession `json:"sessions"`
	Agents      []state.RuntimeAgent   `json:"agents"`
	CollectedAt time.Time              `json:"collected_at"`
}

// NormalizeSnapshot transforms a complete tmux state into normalized projections.
func (a *TmuxAdapter) NormalizeSnapshot(
	sessions []tmux.Session,
	agentsBySession map[string][]Agent,
) *NormalizedSnapshot {
	now := time.Now()

	snapshot := &NormalizedSnapshot{
		Sessions:    make([]state.RuntimeSession, 0, len(sessions)),
		Agents:      make([]state.RuntimeAgent, 0),
		CollectedAt: now,
	}

	for i := range sessions {
		sess := &sessions[i]
		agents := agentsBySession[sess.Name]

		// Normalize session
		rs := a.NormalizeSession(sess, agents)
		snapshot.Sessions = append(snapshot.Sessions, *rs)

		// Normalize agents
		normalizedAgents := a.NormalizeAgents(sess.Name, agents)
		snapshot.Agents = append(snapshot.Agents, normalizedAgents...)
	}

	return snapshot
}
