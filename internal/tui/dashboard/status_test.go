package dashboard

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Dicklesworthstone/ntm/internal/agentmail"
	"github.com/Dicklesworthstone/ntm/internal/config"
	ctxmon "github.com/Dicklesworthstone/ntm/internal/context"
	pt "github.com/Dicklesworthstone/ntm/internal/integrations/pt"
	"github.com/Dicklesworthstone/ntm/internal/status"
	"github.com/Dicklesworthstone/ntm/internal/tmux"
	"github.com/Dicklesworthstone/ntm/internal/tui/layout"
)

func dashboardStatusObservation(observedAt time.Time, statuses ...status.AgentStatus) status.SessionObservation {
	panes := make([]status.PaneObservation, 0, len(statuses))
	for _, agentStatus := range statuses {
		current := status.StateObservation{
			Status:     agentStatus,
			ObservedAt: observedAt,
			Freshness:  status.FreshnessFresh,
			Confidence: 0.95,
		}
		lastKnown := current
		panes = append(panes, status.PaneObservation{
			Pane:      tmux.PaneRef{ID: agentStatus.PaneID},
			AgentType: agentStatus.AgentType,
			Current:   current,
			LastKnown: &lastKnown,
		})
	}
	return status.SessionObservation{
		Session:    "session",
		ObservedAt: observedAt,
		Complete:   true,
		Panes:      panes,
		Failures:   []status.ObservationFailure{},
	}
}

func TestStatusUpdateSetsPaneStateAndTimestamp(t *testing.T) {

	m := New("session", "")
	m.panes = []tmux.Pane{
		{ID: "%1", Index: 0, Title: "session__cod_1", Type: tmux.AgentCodex},
	}
	paneKey := paneStatusKey(m.panes[0])
	m.paneStatus[paneKey] = PaneStatus{}

	now := time.Now()
	// First update establishes the velocity baseline. With no prior sample, the
	// fresh-token rate is genuinely 0 (it is a delta over a window, not a
	// snapshot/age), so velocity history is recorded as 0 here.
	msg := StatusUpdateMsg{
		Observation: dashboardStatusObservation(now,
			status.AgentStatus{
				PaneID:     "%1",
				State:      status.StateIdle,
				LastActive: now.Add(-1 * time.Minute),
				LastOutput: strings.Repeat("token ", 24),
				TokensUsed: 10_000,
				UpdatedAt:  now,
			},
		),
		Time: now,
	}

	updated, _ := m.Update(msg)
	m2 := updated.(Model)

	if got := m2.paneStatus[paneKey].State; got != "idle" {
		t.Fatalf("expected pane state idle, got %q", got)
	}
	if m2.lastRefresh.IsZero() {
		t.Fatalf("expected lastRefresh to be set")
	}
	if len(m2.aggregateVelocityHistory) == 0 {
		t.Fatalf("expected aggregate velocity history to be recorded")
	}
	if got := m2.aggregateVelocityHistory[len(m2.aggregateVelocityHistory)-1]; got != 0 {
		t.Fatalf("expected first-sample aggregate velocity to be 0 (no prior window), got %v", got)
	}

	// Backfill the prior sample to a known earlier time so the next update has a
	// real wall-clock window to rate against, then feed genuine token growth
	// (+4000 tokens over ~2 minutes ≈ 2000 tok/min).
	m2.velocityByPaneID["%1"] = velocitySample{
		tokens:    10_000,
		sampledAt: now.Add(-2 * time.Minute),
	}
	now2 := time.Now()
	msg2 := StatusUpdateMsg{
		Observation: dashboardStatusObservation(now2,
			status.AgentStatus{
				PaneID:     "%1",
				State:      status.StateWorking,
				LastActive: now2,
				LastOutput: strings.Repeat("token ", 24),
				TokensUsed: 14_000,
				UpdatedAt:  now2,
			},
		),
		Time: now2,
	}
	updated2, _ := m2.Update(msg2)
	m3 := updated2.(Model)

	if got := m3.aggregateVelocityHistory[len(m3.aggregateVelocityHistory)-1]; got <= 0 {
		t.Fatalf("expected aggregate velocity sample > 0 after genuine growth, got %v", got)
	}
	if got := len(m3.velocityByType[string(tmux.AgentCodex)]); got == 0 {
		t.Fatalf("expected per-type velocity history to be recorded")
	}
}

func TestStatusUpdateBatchesOllamaRefreshCmd(t *testing.T) {

	m := New("session", "")
	m.panes = []tmux.Pane{
		{ID: "%1", Index: 0, Title: "session__ollama_1", Type: tmux.AgentOllama},
	}
	m.paneStatus[paneStatusKey(m.panes[0])] = PaneStatus{}

	now := time.Now()
	msg := StatusUpdateMsg{
		Observation: dashboardStatusObservation(now,
			status.AgentStatus{
				PaneID:    "%1",
				State:     status.StateIdle,
				UpdatedAt: now,
			},
		),
		Time: now,
	}

	updated, cmd := m.Update(msg)
	if cmd == nil {
		t.Fatal("expected batched follow-up command")
	}
	m2 := updated.(Model)
	if !m2.fetchingOllamaPS {
		t.Fatal("expected status update to schedule an Ollama refresh")
	}

	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatalf("expected tea.BatchMsg, got %T", cmd())
	}
	if len(batch) != 2 {
		t.Fatalf("expected health and Ollama refresh commands, got %d", len(batch))
	}
}

func TestStatusUpdateErrorClearsStaleLiveStatus(t *testing.T) {

	m := New("session", "")
	m.panes = []tmux.Pane{
		{ID: "%1", Index: 0, Title: "session__cod_1", Type: tmux.AgentCodex},
	}
	paneKey := paneStatusKey(m.panes[0])
	m.paneStatus[paneKey] = PaneStatus{
		State:                "working",
		TokenVelocity:        120,
		LocalTokensPerSecond: 4.5,
		LocalTotalTokens:     300,
		LocalLastLatency:     150 * time.Millisecond,
		LocalAvgLatency:      200 * time.Millisecond,
		LocalMemoryBytes:     1 << 20,
		LocalTPSHistory:      []float64{2.5, 4.5},
	}
	m.agentStatuses["%1"] = status.AgentStatus{
		PaneID:     "%1",
		State:      status.StateWorking,
		LastOutput: "stale output",
	}

	updated, _ := m.Update(StatusUpdateMsg{
		Err:  errors.New("status refresh failed"),
		Time: time.Now(),
	})
	m2 := updated.(Model)

	if m2.statusFetchErr == nil {
		t.Fatal("expected status fetch error to be recorded")
	}
	ps := m2.paneStatus[paneKey]
	if ps.State != "" {
		t.Fatalf("expected stale pane state to clear, got %q", ps.State)
	}
	if ps.TokenVelocity != 0 {
		t.Fatalf("expected stale token velocity to clear, got %v", ps.TokenVelocity)
	}
	if ps.LocalTokensPerSecond != 0 || ps.LocalTotalTokens != 0 || ps.LocalLastLatency != 0 || ps.LocalAvgLatency != 0 || ps.LocalMemoryBytes != 0 || len(ps.LocalTPSHistory) != 0 {
		t.Fatalf("expected stale local perf snapshot to clear, got %+v", ps)
	}
	if _, ok := m2.agentStatuses["%1"]; ok {
		t.Fatal("expected stale agent status snapshot to clear")
	}
	rows := BuildPaneTableRows(m2.panes, m2.agentStatuses, m2.paneStatus, nil, nil, nil, 0, m2.theme)
	if got := rows[0].Status; got != "unknown" {
		t.Fatalf("expected stale row status to clear, got %q", got)
	}
	if len(m2.aggregateVelocityHistory) == 0 {
		t.Fatal("expected aggregate velocity history sample to be recorded")
	}
	if got := m2.aggregateVelocityHistory[len(m2.aggregateVelocityHistory)-1]; got != 0 {
		t.Fatalf("expected cleared aggregate velocity sample to be 0, got %v", got)
	}
}

func TestStatusUpdateFailureShowsUnknownAndKeepsLastKnownSeparate(t *testing.T) {
	t.Parallel()

	m := New("session", "")
	m.panes = []tmux.Pane{{ID: "%1", Index: 0, Title: "session__cc_1", Type: tmux.AgentClaude}}
	paneKey := paneStatusKey(m.panes[0])
	m.paneStatus[paneKey] = PaneStatus{State: "working", TokenVelocity: 120}
	previousAt := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	failureAt := previousAt.Add(time.Minute)
	lastKnown := status.StateObservation{
		Status: status.AgentStatus{
			PaneID:    "%1",
			PaneName:  "session__cc_1",
			AgentType: "cc",
			State:     status.StateWorking,
			UpdatedAt: previousAt,
		},
		ObservedAt: previousAt,
		Freshness:  status.FreshnessStale,
		Confidence: 0.95,
	}
	observation := status.PaneObservation{
		Pane:      tmux.PaneRef{ID: "%1", PaneIndex: 0},
		PaneName:  "session__cc_1",
		AgentType: "cc",
		Current: status.StateObservation{
			Status: status.AgentStatus{
				PaneID:    "%1",
				PaneName:  "session__cc_1",
				AgentType: "cc",
				State:     status.StateUnknown,
				UpdatedAt: failureAt,
			},
			ObservedAt: failureAt,
			Freshness:  status.FreshnessUnavailable,
			Confidence: 0,
			Error:      "capture failed",
		},
		LastKnown: &lastKnown,
	}

	updated, _ := m.Update(StatusUpdateMsg{
		Observation: status.SessionObservation{
			Session:    "session",
			ObservedAt: failureAt,
			Complete:   false,
			Panes:      []status.PaneObservation{observation},
			Failures:   []status.ObservationFailure{{PaneID: "%1", Stage: "capture", Error: "capture failed"}},
		},
		Time: failureAt,
	})
	m2 := updated.(Model)
	if m2.paneStatus[paneKey].State != "unknown" || m2.paneStatus[paneKey].TokenVelocity != 0 {
		t.Fatalf("failed current state displayed as %+v", m2.paneStatus[paneKey])
	}
	if got := m2.agentStatuses["%1"]; got.State != status.StateUnknown || got.UpdatedAt != failureAt {
		t.Fatalf("current status = %+v", got)
	}
	stored, ok := m2.paneObservations["%1"]
	if !ok || stored.LastKnown == nil || stored.LastKnown.Status.State != status.StateWorking || stored.LastKnown.ObservedAt != previousAt || stored.SafeToDispatch() {
		t.Fatalf("stored observation = %+v", stored)
	}
}

func TestSessionDataCaptureFailureCannotPreserveIdleAsCurrent(t *testing.T) {
	t.Parallel()

	m := New("session", "")
	m.startupWarmupDone = true
	m.panes = []tmux.Pane{{ID: "%1", Index: 0, Title: "session__cc_1", Type: tmux.AgentClaude}}
	paneKey := paneStatusKey(m.panes[0])
	m.paneStatus[paneKey] = PaneStatus{State: "idle"}
	m.agentStatuses["%1"] = status.AgentStatus{PaneID: "%1", State: status.StateIdle}
	m.paneOutputCache["%1"] = "known prior output"
	previousAt := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	failed := status.PaneObservation{
		Pane:      tmux.PaneRef{ID: "%1", PaneIndex: 0},
		PaneName:  "session__cc_1",
		AgentType: "cc",
		Current: status.StateObservation{
			Status:     status.AgentStatus{PaneID: "%1", PaneName: "session__cc_1", AgentType: "cc", State: status.StateUnknown, UpdatedAt: previousAt.Add(time.Minute)},
			ObservedAt: previousAt.Add(time.Minute),
			Freshness:  status.FreshnessUnavailable,
			Error:      "capture failed",
		},
		LastKnown: &status.StateObservation{
			Status:     status.AgentStatus{PaneID: "%1", State: status.StateIdle, UpdatedAt: previousAt},
			ObservedAt: previousAt,
			Freshness:  status.FreshnessStale,
			Confidence: 0.95,
		},
	}
	updated, _ := m.Update(SessionDataWithOutputMsg{
		Panes: m.panes,
		Outputs: []PaneOutputData{{
			PaneID:       "%1",
			PaneIndex:    0,
			LastActivity: previousAt,
			AgentType:    "cc",
			Observation:  failed,
		}},
	})
	m2 := updated.(Model)
	if m2.paneStatus[paneKey].State != "unknown" || m2.agentStatuses["%1"].State != status.StateUnknown {
		t.Fatalf("failed budgeted capture preserved idle: pane=%+v status=%+v", m2.paneStatus[paneKey], m2.agentStatuses["%1"])
	}
	if m2.paneOutputCache["%1"] != "known prior output" {
		t.Fatalf("failed capture overwrote output cache: %q", m2.paneOutputCache["%1"])
	}
	if stored := m2.paneObservations["%1"]; stored.LastKnown == nil || stored.LastKnown.Status.State != status.StateIdle || stored.SafeToDispatch() {
		t.Fatalf("failed capture observation = %+v", stored)
	}
}

func TestStatusUpdateSuccessClearsMissingPaneSnapshots(t *testing.T) {

	m := New("session", "")
	m.panes = []tmux.Pane{
		{ID: "%1", Index: 0, Title: "session__cc_1", Type: tmux.AgentClaude},
		{ID: "%2", Index: 1, Title: "session__cod_1", Type: tmux.AgentCodex},
	}
	firstKey := paneStatusKey(m.panes[0])
	secondKey := paneStatusKey(m.panes[1])
	m.paneStatus[firstKey] = PaneStatus{State: "working", TokenVelocity: 100}
	m.paneStatus[secondKey] = PaneStatus{
		State:                "idle",
		TokenVelocity:        80,
		LocalTokensPerSecond: 3.5,
		LocalTotalTokens:     200,
		LocalTPSHistory:      []float64{3.5},
	}
	m.agentStatuses["%1"] = status.AgentStatus{PaneID: "%1", State: status.StateWorking}
	m.agentStatuses["%2"] = status.AgentStatus{PaneID: "%2", State: status.StateIdle, LastOutput: "stale"}

	now := time.Now()
	updated, _ := m.Update(StatusUpdateMsg{
		Observation: dashboardStatusObservation(now,
			status.AgentStatus{
				PaneID:    "%1",
				State:     status.StateIdle,
				UpdatedAt: now,
			},
		),
		Time: now,
	})
	m2 := updated.(Model)

	if got := m2.paneStatus[firstKey].State; got != "idle" {
		t.Fatalf("expected pane 0 state to refresh, got %q", got)
	}
	if _, ok := m2.agentStatuses["%1"]; !ok {
		t.Fatal("expected pane 0 status snapshot to remain present")
	}
	if ps := m2.paneStatus[secondKey]; ps.State != "" || ps.TokenVelocity != 0 || ps.LocalTokensPerSecond != 0 || ps.LocalTotalTokens != 0 || len(ps.LocalTPSHistory) != 0 {
		t.Fatalf("expected missing pane status snapshot to clear, got %+v", ps)
	}
	if _, ok := m2.agentStatuses["%2"]; ok {
		t.Fatal("expected missing pane agent status snapshot to clear")
	}
	rows := BuildPaneTableRows(m2.panes, m2.agentStatuses, m2.paneStatus, nil, nil, nil, 0, m2.theme)
	if got := rows[1].Status; got != "unknown" {
		t.Fatalf("expected missing pane row status to clear, got %q", got)
	}
}

func TestSessionDataUpdateBatchesOllamaRefreshCmd(t *testing.T) {

	m := New("session", "")
	m.startupWarmupDone = true
	m.panes = []tmux.Pane{
		{ID: "%1", Index: 0, Title: "session__ollama_1_mistral", Type: tmux.AgentOllama, Variant: "mistral:7b"},
	}
	m.paneStatus[paneStatusKey(m.panes[0])] = PaneStatus{}

	now := time.Now()
	msg := SessionDataWithOutputMsg{
		Panes: []tmux.Pane{
			{ID: "%1", Index: 0, Title: "session__ollama_1_mistral", Type: tmux.AgentOllama, Variant: "mistral:7b"},
		},
		Outputs: []PaneOutputData{
			{
				PaneID:       "%1",
				PaneIndex:    0,
				LastActivity: now,
				Output:       "hello from ollama",
				AgentType:    string(tmux.AgentOllama),
				Observation: dashboardStatusObservation(now, status.AgentStatus{
					PaneID:     "%1",
					PaneName:   "session__ollama_1_mistral",
					AgentType:  string(tmux.AgentOllama),
					State:      status.StateUnknown,
					LastActive: now,
					UpdatedAt:  now,
				}).Panes[0],
			},
		},
		Duration: 10 * time.Millisecond,
	}

	updated, cmd := m.Update(msg)
	if cmd == nil {
		t.Fatal("expected batched follow-up command")
	}
	m2 := updated.(Model)
	if !m2.fetchingOllamaPS {
		t.Fatal("expected session data update to schedule an Ollama refresh")
	}

	switch result := cmd().(type) {
	case tea.BatchMsg:
		if len(result) != 1 {
			t.Fatalf("expected Ollama refresh command to survive warmup batching, got %d", len(result))
		}
	case OllamaPSResultMsg:
		// A single-command batch may collapse to the direct command result.
	default:
		t.Fatalf("expected Ollama refresh result, got %T", result)
	}
}

func TestPendingRotationsUpdateErrorClearsStalePendingData(t *testing.T) {

	m := New("session", "")
	stale := []*ctxmon.PendingRotation{
		{AgentID: "agent1", TimeoutAt: time.Now().Add(time.Minute)},
	}
	m.pendingRotations = stale
	m.rotationConfirmPanel.SetData(stale, nil)

	updated, _ := m.Update(PendingRotationsUpdateMsg{
		Err: errors.New("pending rotations fetch failed"),
	})
	m2 := updated.(Model)

	if m2.pendingRotationsErr == nil {
		t.Fatal("expected pending rotations error to be recorded")
	}
	if len(m2.pendingRotations) != 0 {
		t.Fatalf("expected stale pending rotations to be cleared, got %d", len(m2.pendingRotations))
	}
	if !m2.rotationConfirmPanel.HasError() {
		t.Fatal("expected rotation confirm panel to surface the error")
	}
	if m2.rotationConfirmPanel.HasPending() {
		t.Fatal("expected rotation confirm panel pending state to be cleared on error")
	}
	if pending := m2.rotationConfirmPanel.SelectedPending(); pending != nil {
		t.Fatalf("expected no selected pending rotation on error, got %v", pending.AgentID)
	}
}

func TestHealthUpdateErrorClearsStalePaneHealthDetails(t *testing.T) {

	m := New("session", "")
	m.panes = []tmux.Pane{
		{ID: "%1", Index: 0, Title: "session__cc_1", Type: tmux.AgentClaude},
		{ID: "%2", Index: 1, Title: "session__cod_1", Type: tmux.AgentCodex},
	}
	firstKey := paneStatusKey(m.panes[0])
	secondKey := paneStatusKey(m.panes[1])
	m.paneStatus[firstKey] = PaneStatus{
		HealthStatus:  "error",
		HealthIssues:  []string{"rate limit"},
		RestartCount:  3,
		UptimeSeconds: 120,
	}
	m.paneStatus[secondKey] = PaneStatus{
		HealthStatus:  "warning",
		HealthIssues:  []string{"slow response"},
		RestartCount:  1,
		UptimeSeconds: 60,
	}

	updated, _ := m.Update(HealthUpdateMsg{Err: errors.New("health refresh failed")})
	m2 := updated.(Model)

	for idx, key := range []string{firstKey, secondKey} {
		ps := m2.paneStatus[key]
		if ps.HealthStatus != "" {
			t.Fatalf("expected pane %d health status to clear, got %q", idx, ps.HealthStatus)
		}
		if len(ps.HealthIssues) != 0 {
			t.Fatalf("expected pane %d health issues to clear, got %v", idx, ps.HealthIssues)
		}
		if ps.RestartCount != 0 {
			t.Fatalf("expected pane %d restart count to clear, got %d", idx, ps.RestartCount)
		}
		if ps.UptimeSeconds != 0 {
			t.Fatalf("expected pane %d uptime to clear, got %d", idx, ps.UptimeSeconds)
		}
	}
}

func TestHealthUpdateSuccessClearsMissingPaneHealthDetails(t *testing.T) {

	m := New("session", "")
	m.panes = []tmux.Pane{
		{ID: "%1", Index: 0, Title: "session__cc_1", Type: tmux.AgentClaude},
		{ID: "%2", Index: 1, Title: "session__cod_1", Type: tmux.AgentCodex},
	}
	firstKey := paneStatusKey(m.panes[0])
	secondKey := paneStatusKey(m.panes[1])
	m.paneStatus[firstKey] = PaneStatus{
		HealthStatus:  "warning",
		HealthIssues:  []string{"old warning"},
		RestartCount:  2,
		UptimeSeconds: 45,
	}
	m.paneStatus[secondKey] = PaneStatus{
		HealthStatus:  "error",
		HealthIssues:  []string{"old error"},
		RestartCount:  5,
		UptimeSeconds: 90,
	}

	updated, _ := m.Update(HealthUpdateMsg{
		Health: map[string]PaneHealthInfo{
			"%1": {
				Status:       "ok",
				Issues:       []string{"fresh"},
				RestartCount: 1,
				Uptime:       300,
			},
		},
	})
	m2 := updated.(Model)

	if got := m2.paneStatus[firstKey].HealthStatus; got != "ok" {
		t.Fatalf("expected pane 0 health status to update, got %q", got)
	}
	if got := m2.paneStatus[firstKey].HealthIssues; len(got) != 1 || got[0] != "fresh" {
		t.Fatalf("expected pane 0 health issues to update, got %v", got)
	}
	if got := m2.paneStatus[firstKey].RestartCount; got != 1 {
		t.Fatalf("expected pane 0 restart count to update, got %d", got)
	}
	if got := m2.paneStatus[firstKey].UptimeSeconds; got != 300 {
		t.Fatalf("expected pane 0 uptime to update, got %d", got)
	}

	if ps := m2.paneStatus[secondKey]; ps.HealthStatus != "" || len(ps.HealthIssues) != 0 || ps.RestartCount != 0 || ps.UptimeSeconds != 0 {
		t.Fatalf("expected pane 1 stale health state to clear, got %+v", ps)
	}
}

func TestPTHealthStatesNilClearsStaleClassification(t *testing.T) {

	m := New("session", "")
	m.panes = []tmux.Pane{
		{ID: "%1", Index: 0, Title: "session__cc_1", Type: tmux.AgentClaude},
	}
	m.healthStates = map[string]*pt.AgentState{
		"session__cc_1": {
			Pane:           "session__cc_1",
			Classification: pt.ClassStuck,
			Since:          time.Now().Add(-10 * time.Minute),
		},
	}

	before := BuildPaneTableRows(m.panes, nil, m.paneStatus, nil, nil, m.healthStates, 0, m.theme)
	if got := before[0].HealthClass; got != pt.ClassStuck {
		t.Fatalf("expected initial health class stuck, got %q", got)
	}

	updated, _ := m.Update(PTHealthStatesMsg{States: nil})
	m2 := updated.(Model)
	if m2.healthStates != nil {
		t.Fatalf("expected PT health states to clear, got %#v", m2.healthStates)
	}

	after := BuildPaneTableRows(m2.panes, nil, m2.paneStatus, nil, nil, m2.healthStates, 0, m2.theme)
	if got := after[0].HealthClass; got != pt.ClassUnknown {
		t.Fatalf("expected stale PT classification to clear, got %q", got)
	}
}

func TestViewShowsLoadingWhenSessionFetchInFlight(t *testing.T) {

	m := New("session", "")
	m.width = 80
	m.height = 30
	m.tier = layout.TierForWidth(m.width)

	// Simulate initial load: no panes yet, fetch in flight.
	m.panes = nil
	m.err = nil
	m.fetchingSession = true
	m.lastPaneFetch = time.Now().Add(-750 * time.Millisecond)

	plain := status.StripANSI(m.View())
	if strings.Contains(plain, "No panes found") {
		t.Fatalf("expected loading state, got empty state")
	}
	if !strings.Contains(plain, "Fetching panes") {
		t.Fatalf("expected loading copy to mention fetching panes")
	}
}

func TestViewRendersPanesEvenWhenLastSessionFetchErrored(t *testing.T) {

	m := New("session", "")
	m.width = 120
	m.height = 30
	m.tier = layout.TierForWidth(m.width)

	m.panes = []tmux.Pane{
		{ID: "%1", Index: 0, Title: "session__cod_1", Type: tmux.AgentCodex},
	}
	m.err = errors.New("tmux refresh failed")

	plain := status.StripANSI(m.View())
	if !strings.Contains(plain, "tmux refresh failed") {
		t.Fatalf("expected error to be surfaced in view")
	}
	if !strings.Contains(plain, "session__cod_1") {
		t.Fatalf("expected panes to still render when session fetch errors")
	}
}

func TestPlanPaneCaptures_PrioritizesSelectedAndNewActivity(t *testing.T) {

	now := time.Now()
	panes := []tmux.PaneActivity{
		{Pane: tmux.Pane{ID: "%0", Index: 0, Type: tmux.AgentUser}, LastActivity: now},
		{Pane: tmux.Pane{ID: "%1", Index: 1, Type: tmux.AgentCodex}, LastActivity: now.Add(-10 * time.Second)},
		{Pane: tmux.Pane{ID: "%2", Index: 2, Type: tmux.AgentClaude}, LastActivity: now.Add(-1 * time.Second)},
		{Pane: tmux.Pane{ID: "%3", Index: 3, Type: tmux.AgentGemini}, LastActivity: now.Add(-5 * time.Second)},
	}

	lastCaptured := map[string]time.Time{
		"%1": now,                        // no new activity
		"%2": now,                        // selected, no new activity
		"%3": now.Add(-20 * time.Second), // new activity since last capture
	}

	plan := planPaneCaptures(panes, "%2", lastCaptured, 2, 0)
	if len(plan.Targets) != 2 {
		t.Fatalf("expected 2 capture targets, got %d", len(plan.Targets))
	}
	if plan.Targets[0].Pane.ID != "%2" {
		t.Fatalf("expected selected pane first, got %s", plan.Targets[0].Pane.ID)
	}
	if plan.Targets[1].Pane.ID != "%3" {
		t.Fatalf("expected pane with new activity second, got %s", plan.Targets[1].Pane.ID)
	}
}

func TestPlanPaneCaptures_RoundRobinAdvancesCursor(t *testing.T) {

	now := time.Now()
	panes := []tmux.PaneActivity{
		{Pane: tmux.Pane{ID: "%1", Index: 1, Type: tmux.AgentCodex}, LastActivity: now.Add(-10 * time.Second)},
		{Pane: tmux.Pane{ID: "%2", Index: 2, Type: tmux.AgentClaude}, LastActivity: now.Add(-10 * time.Second)},
		{Pane: tmux.Pane{ID: "%3", Index: 3, Type: tmux.AgentGemini}, LastActivity: now.Add(-10 * time.Second)},
	}

	lastCaptured := map[string]time.Time{
		"%1": now,
		"%2": now,
		"%3": now,
	}

	plan := planPaneCaptures(panes, "", lastCaptured, 2, 1)
	if len(plan.Targets) != 2 {
		t.Fatalf("expected 2 capture targets, got %d", len(plan.Targets))
	}
	if plan.Targets[0].Pane.ID != "%2" || plan.Targets[1].Pane.ID != "%3" {
		t.Fatalf("unexpected round-robin targets: %s, %s", plan.Targets[0].Pane.ID, plan.Targets[1].Pane.ID)
	}
	if plan.NextCursor != 0 {
		t.Fatalf("expected NextCursor=0, got %d", plan.NextCursor)
	}
}

func TestSessionDataUpdate_SortsPanesAndKeepsSelection(t *testing.T) {

	m := New("session", "")
	m.panes = []tmux.Pane{
		{ID: "%0", Index: 0, Title: "session__user_0", Type: tmux.AgentUser},
		{ID: "%1", Index: 1, Title: "session__cod_1", Type: tmux.AgentCodex},
		{ID: "%2", Index: 2, Title: "session__cc_1", Type: tmux.AgentClaude},
	}
	m.cursor = 2

	msg := SessionDataWithOutputMsg{
		Panes: []tmux.Pane{
			{ID: "%2", Index: 2, Title: "session__cc_1", Type: tmux.AgentClaude},
			{ID: "%0", Index: 0, Title: "session__user_0", Type: tmux.AgentUser},
			{ID: "%1", Index: 1, Title: "session__cod_1", Type: tmux.AgentCodex},
		},
		Duration:          10 * time.Millisecond,
		NextCaptureCursor: 0,
	}

	updated, _ := m.Update(msg)
	m2 := updated.(Model)

	if len(m2.panes) != 3 {
		t.Fatalf("expected 3 panes, got %d", len(m2.panes))
	}
	if m2.panes[0].ID != "%0" || m2.panes[1].ID != "%1" || m2.panes[2].ID != "%2" {
		t.Fatalf("expected panes sorted by index, got %s %s %s", m2.panes[0].ID, m2.panes[1].ID, m2.panes[2].ID)
	}
	if m2.panes[m2.cursor].ID != "%2" {
		t.Fatalf("expected selection to remain on %%2, got %s", m2.panes[m2.cursor].ID)
	}
}

func TestSeedInitialPanes_SortsAndMarksSnapshotComplete(t *testing.T) {

	m := New("session", "")
	m.seedInitialPanes([]tmux.PaneActivity{
		{Pane: tmux.Pane{ID: "%2", Index: 2, Title: "session__cc_1", Type: tmux.AgentClaude}},
		{Pane: tmux.Pane{ID: "%0", Index: 0, Title: "session__user_0", Type: tmux.AgentUser}},
		{Pane: tmux.Pane{ID: "%1", Index: 1, Title: "session__cod_1", Type: tmux.AgentCodex}},
	})

	if !m.initialPaneSnapshotDone {
		t.Fatalf("expected initialPaneSnapshotDone to be true")
	}
	if len(m.panes) != 3 {
		t.Fatalf("expected 3 panes, got %d", len(m.panes))
	}
	if m.panes[0].ID != "%0" || m.panes[1].ID != "%1" || m.panes[2].ID != "%2" {
		t.Fatalf("expected seeded panes sorted by index, got %s %s %s", m.panes[0].ID, m.panes[1].ID, m.panes[2].ID)
	}
	if m.agentCounts[tmux.AgentClaude] != 1 || m.agentCounts[tmux.AgentCodex] != 1 || m.agentCounts[tmux.AgentUser] != 1 {
		t.Fatalf("expected seeded counts Claude=1 Codex=1 User=1, got %v", m.agentCounts)
	}
}

func TestMultiWindowSameLocalIndexKeepsAllPaneStateIndependent(t *testing.T) {
	lockAttentionFeedForTest(t)

	now := time.Now()
	panes := []tmux.Pane{
		{ID: "%10", WindowIndex: 0, Index: 0, Title: "session__cod_1", Type: tmux.AgentCodex},
		{ID: "%20", WindowIndex: 1, Index: 0, Title: "session__cc_1", Type: tmux.AgentClaude},
	}
	m := New("session", "")
	m.startupWarmupDone = true
	m.width = 140
	m.tier = layout.TierForWidth(m.width)
	m.panes = panes
	m.paneStatus["%10"] = PaneStatus{MailUnread: 9, HealthStatus: "stale-first"}
	m.paneStatus["%20"] = PaneStatus{MailUnread: 8, HealthStatus: "stale-second"}
	m.velocityByPaneID["%10"] = velocitySample{tokens: 1_000, sampledAt: now.Add(-time.Minute)}
	m.velocityByPaneID["%20"] = velocitySample{tokens: 2_000, sampledAt: now.Add(-time.Minute)}

	firstStatus := status.AgentStatus{
		PaneID:     "%10",
		PaneName:   panes[0].Title,
		AgentType:  string(tmux.AgentCodex),
		State:      status.StateError,
		ErrorType:  status.ErrorRateLimit,
		TokensUsed: 2_000,
		UpdatedAt:  now,
	}
	secondStatus := status.AgentStatus{
		PaneID:     "%20",
		PaneName:   panes[1].Title,
		AgentType:  string(tmux.AgentClaude),
		State:      status.StateIdle,
		TokensUsed: 5_000,
		UpdatedAt:  now,
	}
	firstObservation := dashboardStatusObservation(now, firstStatus).Panes[0]
	firstObservation.Pane = panes[0].Ref()
	secondObservation := dashboardStatusObservation(now, secondStatus).Panes[0]
	secondObservation.Pane = panes[1].Ref()

	updated, _ := m.Update(SessionDataWithOutputMsg{
		Panes: panes,
		Outputs: []PaneOutputData{
			{
				PaneID:       "%10",
				PaneIndex:    0,
				LastActivity: now,
				Output:       strings.Repeat("codex output ", 1_000),
				AgentType:    string(tmux.AgentCodex),
				Observation:  firstObservation,
			},
			{
				PaneID:       "%20",
				PaneIndex:    0,
				LastActivity: now,
				Output:       strings.Repeat("claude output ", 4_000),
				AgentType:    string(tmux.AgentClaude),
				Observation:  secondObservation,
			},
		},
	})
	m = updated.(Model)

	first := m.paneStatus["%10"]
	second := m.paneStatus["%20"]
	if first.State != "rate_limited" || second.State != "idle" {
		t.Fatalf("same-index live states aliased: first=%+v second=%+v", first, second)
	}
	if first.TokenVelocity <= 0 || second.TokenVelocity <= 0 {
		t.Fatalf("same-index token velocities were not tracked independently: first=%v second=%v", first.TokenVelocity, second.TokenVelocity)
	}
	if first.ContextTokens == 0 || second.ContextTokens == 0 || first.ContextModel == second.ContextModel {
		t.Fatalf("same-index context state aliased: first=%+v second=%+v", first, second)
	}
	if first.MailUnread != 9 || second.MailUnread != 8 || first.HealthStatus != "stale-first" || second.HealthStatus != "stale-second" {
		t.Fatalf("output refresh crossed unrelated pane state: first=%+v second=%+v", first, second)
	}

	updated, _ = m.Update(HealthUpdateMsg{Health: map[string]PaneHealthInfo{
		"%10": {Status: "critical", Issues: []string{"first-only"}, RestartCount: 3, Uptime: 10},
		"%20": {Status: "ok", Issues: []string{"second-only"}, RestartCount: 1, Uptime: 20},
	}})
	m = updated.(Model)
	if got := m.paneStatus["%10"]; got.HealthStatus != "critical" || len(got.HealthIssues) != 1 || got.HealthIssues[0] != "first-only" || got.RestartCount != 3 {
		t.Fatalf("first pane health = %+v", got)
	}
	if got := m.paneStatus["%20"]; got.HealthStatus != "ok" || len(got.HealthIssues) != 1 || got.HealthIssues[0] != "second-only" || got.RestartCount != 1 {
		t.Fatalf("second pane health = %+v", got)
	}

	updated, _ = m.Update(AgentMailInboxSummaryMsg{
		ProjectKey: "project",
		Inboxes: map[string][]agentmail.InboxMessage{
			"%10": {{ID: 101, Subject: "first urgent", Importance: "urgent"}},
			"%20": {
				{ID: 201, Subject: "second one", Importance: "normal"},
				{ID: 202, Subject: "second two", Importance: "normal"},
			},
		},
		AgentMap: map[string]string{"%10": "FirstAgent", "%20": "SecondAgent"},
		Gen:      1,
	})
	m = updated.(Model)
	if got := m.paneStatus["%10"]; got.MailUnread != 1 || got.MailUrgent != 1 {
		t.Fatalf("first pane mail = %+v", got)
	}
	if got := m.paneStatus["%20"]; got.MailUnread != 2 || got.MailUrgent != 0 {
		t.Fatalf("second pane mail = %+v", got)
	}

	rows := BuildPaneTableRows(m.panes, m.agentStatuses, m.paneStatus, nil, nil, nil, 0, m.theme)
	if len(rows) != 2 || rows[0].Address != "0.0" || rows[1].Address != "1.0" {
		t.Fatalf("multi-window row addresses = %+v", rows)
	}
	if rows[0].Status != "rate_limited" || rows[1].Status != "idle" || rows[0].ContextPct != m.paneStatus["%10"].ContextPercent || rows[1].ContextPct != m.paneStatus["%20"].ContextPercent {
		t.Fatalf("multi-window rows crossed state: %+v", rows)
	}
	ranks := m.computeContextRanks()
	if _, ok := ranks["%10"]; !ok {
		t.Fatalf("context ranks omitted first physical pane: %#v", ranks)
	}
	if _, ok := ranks["%20"]; !ok {
		t.Fatalf("context ranks omitted second physical pane: %#v", ranks)
	}

	grid := status.StripANSI(m.renderPaneGrid())
	if !strings.Contains(grid, "#0.0") || !strings.Contains(grid, "#1.0") || !strings.Contains(grid, "RATE") || !strings.Contains(grid, "IDLE") {
		t.Fatalf("multi-window grid did not render independent panes:\n%s", grid)
	}
	alert := status.StripANSI(m.renderRateLimitAlert())
	if !strings.Contains(alert, "pane 0.0") || strings.Contains(alert, "pane 1.0") {
		t.Fatalf("rate-limit rendering attributed the wrong physical pane: %q", alert)
	}
}

// Muse Code panes are titled with the alias they were spawned with plus an
// optional @effort ("spark", "wonder-full@xhigh"). Passed to the context
// registry raw, neither matched and the pane showed the 128K fallback window,
// and a bare Muse pane had no model at all. Every Muse pane must resolve to a
// Muse model and its 1,007,997-token window, including an operator alias.
func TestMusePaneContextLimitResolvesAliasAndEffort(t *testing.T) {
	lockAttentionFeedForTest(t)

	now := time.Now()
	panes := []tmux.Pane{
		{ID: "%41", WindowIndex: 0, Index: 1, Title: "session__muse_1", Type: tmux.AgentMuse},
		{ID: "%42", WindowIndex: 0, Index: 2, Title: "session__muse_2_spark", Type: tmux.AgentMuse, Variant: "spark"},
		{ID: "%43", WindowIndex: 0, Index: 3, Title: "session__muse_3_wonder-full@xhigh", Type: tmux.AgentMuse, Variant: "wonder-full@xhigh"},
		{ID: "%44", WindowIndex: 0, Index: 4, Title: "session__muse_4_quick", Type: tmux.AgentMuse, Variant: "quick"},
	}
	wantModels := map[string]string{
		"%41": "muse-wonder-1.0",
		"%42": "muse-spark-1.3",
		"%43": "muse-wonder-full-1.0",
		"%44": "muse-spark-1.2",
	}

	m := New("session", "")
	m.startupWarmupDone = true
	m.width = 140
	m.tier = layout.TierForWidth(m.width)
	m.panes = panes
	m.cfg = config.Default()
	m.cfg.Models.Muse["quick"] = "muse-spark-1.2"

	var outputs []PaneOutputData
	for _, pane := range panes {
		observation := dashboardStatusObservation(now, status.AgentStatus{
			PaneID:    pane.ID,
			PaneName:  pane.Title,
			AgentType: string(tmux.AgentMuse),
			State:     status.StateIdle,
			UpdatedAt: now,
		}).Panes[0]
		observation.Pane = pane.Ref()
		outputs = append(outputs, PaneOutputData{
			PaneID:       pane.ID,
			PaneIndex:    pane.Index,
			LastActivity: now,
			Output:       strings.Repeat("muse output ", 500),
			AgentType:    string(pane.Type),
			Observation:  observation,
		})
	}

	updated, _ := m.Update(SessionDataWithOutputMsg{Panes: panes, Outputs: outputs})
	m = updated.(Model)

	for _, pane := range panes {
		ps := m.paneStatus[paneStatusKey(pane)]
		if ps.ContextModel != wantModels[pane.ID] || ps.ContextLimit != 1007997 {
			t.Errorf("Muse pane %s (variant %q) context = model %q limit %d, want model %q limit 1007997",
				pane.ID, pane.Variant, ps.ContextModel, ps.ContextLimit, wantModels[pane.ID])
		}
		if got := m.resolveCostModelForPane(pane); got != wantModels[pane.ID] {
			t.Errorf("resolveCostModelForPane(%s, variant %q) = %q, want %q", pane.ID, pane.Variant, got, wantModels[pane.ID])
		}
	}

	// Before a config load the dashboard has no cfg; the built-in Muse
	// aliases and default model still apply.
	unconfigured := New("session", "")
	if got := unconfigured.resolveCostModelForPane(panes[2]); got != "muse-wonder-full-1.0" {
		t.Errorf("resolveCostModelForPane(wonder-full@xhigh) without cfg = %q, want muse-wonder-full-1.0", got)
	}
	if got := unconfigured.resolveCostModelForPane(panes[0]); got != "muse-wonder-1.0" {
		t.Errorf("resolveCostModelForPane(bare muse) without cfg = %q, want muse-wonder-1.0", got)
	}
}

func TestPaneStatusSurvivesSingleToMultiWindowTopologyTransitionByPaneID(t *testing.T) {
	t.Parallel()

	m := New("session", "")
	m.startupWarmupDone = true
	m.panes = []tmux.Pane{
		{ID: "%a", WindowIndex: 0, Index: 0, Title: "session__cc_1", Type: tmux.AgentClaude},
		{ID: "%b", WindowIndex: 0, Index: 1, Title: "session__cod_1", Type: tmux.AgentCodex},
	}
	m.paneStatus["%a"] = PaneStatus{State: "working", ContextPercent: 73, MailUnread: 4, HealthStatus: "warning"}
	m.paneStatus["%b"] = PaneStatus{State: "idle", ContextPercent: 19, MailUnread: 1, HealthStatus: "ok"}
	m.cursor = 1

	updated, _ := m.Update(SessionDataWithOutputMsg{
		Panes: []tmux.Pane{
			{ID: "%a", WindowIndex: 1, Index: 0, Title: "session__cc_1", Type: tmux.AgentClaude},
			{ID: "%c", WindowIndex: 0, Index: 1, Title: "session__agy_1", Type: tmux.AgentAntigravity},
			{ID: "%b", WindowIndex: 0, Index: 0, Title: "session__cod_1", Type: tmux.AgentCodex},
		},
		MetadataOnly: true,
	})
	m = updated.(Model)

	if got := []string{m.panes[0].ID, m.panes[1].ID, m.panes[2].ID}; strings.Join(got, ",") != "%b,%c,%a" {
		t.Fatalf("panes not sorted by physical topology: %v", got)
	}
	if selected := m.panes[m.cursor].ID; selected != "%b" {
		t.Fatalf("selection moved from physical pane %%b to %s", selected)
	}
	if got := m.paneStatus["%a"]; got.State != "working" || got.ContextPercent != 73 || got.MailUnread != 4 || got.HealthStatus != "warning" {
		t.Fatalf("pane %%a state was not retained across window move: %+v", got)
	}
	if got := m.paneStatus["%b"]; got.State != "idle" || got.ContextPercent != 19 || got.MailUnread != 1 || got.HealthStatus != "ok" {
		t.Fatalf("pane %%b state was not retained across local-index change: %+v", got)
	}
	if _, exists := m.paneStatus["%c"]; exists {
		t.Fatalf("new pane %%c inherited another pane's state: %+v", m.paneStatus["%c"])
	}

	updated, _ = m.Update(SessionDataWithOutputMsg{
		Panes:        []tmux.Pane{m.panes[2]},
		MetadataOnly: true,
	})
	m = updated.(Model)
	if len(m.paneStatus) != 1 || m.paneStatus["%a"].State != "working" {
		t.Fatalf("removed pane state was not pruned without disturbing survivor: %#v", m.paneStatus)
	}
}
