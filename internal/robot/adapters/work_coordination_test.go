package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/agentmail"
	"github.com/Dicklesworthstone/ntm/internal/bv"
	"github.com/Dicklesworthstone/ntm/internal/handoff"
	"github.com/Dicklesworthstone/ntm/internal/tracker"
)

func TestNormalizeWork(t *testing.T) {
	t.Parallel()

	triage := &bv.TriageResponse{
		Triage: bv.TriageData{
			QuickRef: bv.TriageQuickRef{
				ActionableCount: 3,
			},
			Recommendations: []bv.TriageRecommendation{
				{
					ID:          "bd-1",
					Title:       "Fix adapter",
					Type:        "task",
					Priority:    1,
					Labels:      []string{"robot-redesign", "adapters"},
					Score:       8.5,
					Reasons:     []string{"high impact"},
					UnblocksIDs: []string{"bd-2", "bd-3"},
				},
			},
			QuickWins: []bv.TriageRecommendation{
				{ID: "bd-1"},
			},
			BlockersToClear: []bv.BlockerToClear{
				{ID: "bd-blocker", UnblocksCount: 4},
			},
			ProjectHealth: &bv.ProjectHealth{
				GraphMetrics: &bv.GraphMetrics{
					TotalNodes: 17,
					TotalEdges: 23,
					CycleCount: 1,
					MaxDepth:   5,
					Density:    0.2,
				},
			},
		},
	}

	section := NormalizeWork(WorkInputs{
		Summary: &bv.BeadsSummary{
			Available:  true,
			Total:      10,
			Open:       7,
			InProgress: 2,
			Closed:     3,
			Ready:      3,
			Blocked:    2,
			ReadyPreview: []bv.BeadPreview{
				{ID: "bd-1", Title: "Fix adapter", Priority: "P1"},
			},
			InProgressList: []bv.BeadInProgress{
				{ID: "bd-4", Title: "Existing work", Assignee: "BlueLake"},
			},
		},
		Ready: []bv.BeadPreview{
			{ID: "bd-1", Title: "Fix adapter", Priority: "P1"},
		},
		Blocked: []bv.BeadPreview{
			{ID: "bd-9", Title: "Blocked task", Priority: "P2"},
		},
		InProgress: []bv.BeadInProgress{
			{ID: "bd-4", Title: "Existing work", Assignee: "BlueLake"},
		},
		Triage: triage,
	})

	if !section.Available {
		t.Fatal("expected work section to be available")
	}
	if section.Summary == nil || section.Summary.Ready != 3 {
		t.Fatalf("unexpected summary: %+v", section.Summary)
	}
	if len(section.Ready) != 1 || section.Ready[0].Unblocks != 2 {
		t.Fatalf("unexpected ready items: %+v", section.Ready)
	}
	if section.Ready[0].TitleDisclosure == nil || section.Ready[0].TitleDisclosure.DisclosureState != "visible" {
		t.Fatalf("expected visible title disclosure, got %+v", section.Ready[0].TitleDisclosure)
	}
	if len(section.Blocked) != 1 || section.Blocked[0].ID != "bd-9" {
		t.Fatalf("unexpected blocked items: %+v", section.Blocked)
	}
	if len(section.InProgress) != 1 || section.InProgress[0].Assignee != "BlueLake" {
		t.Fatalf("unexpected in-progress items: %+v", section.InProgress)
	}
	if section.Triage == nil || section.Triage.TopRecommendation == nil || section.Triage.TopRecommendation.ID != "bd-1" {
		t.Fatalf("unexpected triage summary: %+v", section.Triage)
	}
	if section.Graph == nil || section.Graph.CycleCount != 1 {
		t.Fatalf("unexpected graph summary: %+v", section.Graph)
	}
}

func TestNormalizeWorkRedactsSensitiveTitles(t *testing.T) {
	t.Parallel()

	secret := "Bearer " + strings.Repeat("token", 10)
	section := NormalizeWork(WorkInputs{
		Ready: []bv.BeadPreview{
			{ID: "bd-secret", Title: "Rotate credential " + secret, Priority: "P1"},
		},
	})

	if !section.Available {
		t.Fatal("expected work section to be available")
	}
	if len(section.Ready) != 1 {
		t.Fatalf("expected 1 ready item, got %d", len(section.Ready))
	}
	item := section.Ready[0]
	if strings.Contains(item.Title, secret) {
		t.Fatalf("expected title to be redacted, got %q", item.Title)
	}
	if !strings.Contains(item.Title, "[REDACTED:") {
		t.Fatalf("expected redaction placeholder in title, got %q", item.Title)
	}
	if item.TitleDisclosure == nil {
		t.Fatal("expected title disclosure metadata")
	}
	if item.TitleDisclosure.DisclosureState != "redacted" {
		t.Fatalf("disclosure_state = %q, want redacted", item.TitleDisclosure.DisclosureState)
	}
	if item.TitleDisclosure.Findings == 0 {
		t.Fatalf("expected findings count > 0, got %+v", item.TitleDisclosure)
	}
	if strings.Contains(item.TitleDisclosure.Preview, secret) {
		t.Fatalf("expected safe preview to be redacted, got %q", item.TitleDisclosure.Preview)
	}
}

func TestNormalizeWorkMarksLongSafeTitlesPreviewOnly(t *testing.T) {
	t.Parallel()

	longTitle := strings.Repeat("harmless coordination text ", 8)
	section := NormalizeWork(WorkInputs{
		Ready: []bv.BeadPreview{
			{ID: "bd-preview", Title: longTitle, Priority: "P2"},
		},
	})

	if len(section.Ready) != 1 {
		t.Fatalf("expected 1 ready item, got %d", len(section.Ready))
	}
	item := section.Ready[0]
	if item.TitleDisclosure == nil {
		t.Fatal("expected title disclosure metadata")
	}
	if item.TitleDisclosure.DisclosureState != "preview_only" {
		t.Fatalf("disclosure_state = %q, want preview_only", item.TitleDisclosure.DisclosureState)
	}
	if item.Title != item.TitleDisclosure.Preview {
		t.Fatalf("expected preview-only title to match preview, got title=%q preview=%q", item.Title, item.TitleDisclosure.Preview)
	}
	if item.Title == strings.TrimSpace(longTitle) {
		t.Fatalf("expected preview-only title to truncate long content, got %q", item.Title)
	}
	if !strings.HasSuffix(item.TitleDisclosure.Preview, "...") {
		t.Fatalf("expected ellipsis in preview, got %q", item.TitleDisclosure.Preview)
	}
}

func TestNormalizeWorkUnavailable(t *testing.T) {
	t.Parallel()

	section := NormalizeWork(WorkInputs{
		Summary: &bv.BeadsSummary{
			Available: false,
			Reason:    "beads unavailable",
		},
	})

	if section.Available {
		t.Fatal("expected unavailable work section")
	}
	if section.Reason != "beads unavailable" {
		t.Fatalf("reason = %q, want beads unavailable", section.Reason)
	}
}

func TestNormalizeCoordination(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 22, 4, 0, 0, 0, time.UTC)
	secret := strings.Repeat("s", 20)
	expiringSoon := agentmail.FlexTime{Time: now.Add(30 * time.Minute)}
	later := agentmail.FlexTime{Time: now.Add(3 * time.Hour)}

	section := NormalizeCoordination(CoordinationInputs{
		InboxByAgent: map[string][]agentmail.InboxMessage{
			"BlueLake": {
				{
					ID:          1,
					Subject:     "Claim update",
					From:        "GreenStone",
					ThreadID:    stringPtr("bd-j9jo3.3.2"),
					CreatedTS:   agentmail.FlexTime{Time: now.Add(-10 * time.Minute)},
					Importance:  "urgent",
					AckRequired: true,
					BodyMD:      "Need restart token=" + secret + " before merge",
				},
			},
			"RedHill": {
				{
					ID:        2,
					Subject:   "Coordination",
					ThreadID:  stringPtr("bd-j9jo3.3.2"),
					CreatedTS: agentmail.FlexTime{Time: now.Add(-2 * time.Hour)},
				},
			},
		},
		Reservations: []agentmail.FileReservation{
			{
				ID:          1,
				PathPattern: "internal/robot/adapters/*.go",
				AgentName:   "BlueLake",
				Exclusive:   true,
				Reason:      "bd-j9jo3.3.2",
				CreatedTS:   agentmail.FlexTime{Time: now.Add(-20 * time.Minute)},
				ExpiresTS:   expiringSoon,
			},
			{
				ID:          2,
				PathPattern: "internal/robot/adapters/work_coordination.go",
				AgentName:   "GreenStone",
				Exclusive:   true,
				Reason:      "bd-j9jo3.3.5",
				CreatedTS:   agentmail.FlexTime{Time: now.Add(-5 * time.Minute)},
				ExpiresTS:   later,
			},
		},
		ReservationConflicts: []ReservationConflict{
			{
				Pattern: "internal/robot/adapters/*.go <-> internal/robot/adapters/work_coordination.go",
				Holders: []ReservationHolder{
					{AgentName: "BlueLake"},
					{AgentName: "GreenStone"},
				},
			},
		},
		FileConflicts: []tracker.Conflict{
			{
				Path:     "internal/robot/adapters/work_coordination.go",
				Severity: "critical",
				Agents:   []string{"BlueLake", "GreenStone"},
			},
		},
		Handoff: (&handoff.Handoff{
			Session:          "ntm--robot-redesign",
			Status:           "blocked",
			Goal:             "finish adapter",
			Now:              "resolve reservation conflict",
			UpdatedAt:        now,
			Blockers:         []string{"reservation conflict"},
			ActiveBeads:      []string{"bd-j9jo3.3.2"},
			AgentMailThreads: []string{"bd-j9jo3.3.2"},
			Files: handoff.FileChanges{
				Modified: []string{"internal/robot/adapters/work_coordination.go"},
			},
		}),
		Now:                       now,
		ThreadStaleAfter:          time.Hour,
		ReservationExpiringWithin: time.Hour,
		MailBacklogThreshold:      1,
	})

	if !section.Available {
		t.Fatal("expected coordination section to be available")
	}
	if section.Mail == nil || section.Mail.TotalUnread != 2 || section.Mail.UrgentUnread != 1 || section.Mail.PendingAck != 1 {
		t.Fatalf("unexpected mail summary: %+v", section.Mail)
	}
	blue := section.Mail.ByAgent["BlueLake"]
	if blue.LatestSubject != "Claim update" {
		t.Fatalf("latest subject = %q, want %q", blue.LatestSubject, "Claim update")
	}
	if !strings.Contains(blue.LatestPreview, "[REDACTED:GENERIC_SECRET:") {
		t.Fatalf("latest preview = %q", blue.LatestPreview)
	}
	if blue.LatestSubjectDisclosure == nil || blue.LatestSubjectDisclosure.DisclosureState != "visible" {
		t.Fatalf("expected visible latest subject disclosure, got %+v", blue.LatestSubjectDisclosure)
	}
	if blue.LatestPreviewDisclosure == nil || blue.LatestPreviewDisclosure.DisclosureState != "redacted" || blue.LatestPreviewDisclosure.Findings != 1 {
		t.Fatalf("expected redacted latest preview disclosure, got %+v", blue.LatestPreviewDisclosure)
	}
	if section.Threads == nil || section.Threads.Active != 1 || section.Threads.TopThreads[0] != "bd-j9jo3.3.2" {
		t.Fatalf("unexpected threads summary: %+v", section.Threads)
	}
	if section.Reservations == nil || section.Reservations.Active != 2 || section.Reservations.Expiring != 1 || section.Reservations.Conflicts != 1 {
		t.Fatalf("unexpected reservations summary: %+v", section.Reservations)
	}
	if section.Handoff == nil || section.Handoff.Status != "blocked" {
		t.Fatalf("unexpected handoff summary: %+v", section.Handoff)
	}
	if section.Handoff.GoalDisclosure == nil || section.Handoff.GoalDisclosure.DisclosureState != "visible" {
		t.Fatalf("expected visible goal disclosure, got %+v", section.Handoff.GoalDisclosure)
	}
	if section.Handoff.NowDisclosure == nil || section.Handoff.NowDisclosure.DisclosureState != "visible" {
		t.Fatalf("expected visible now disclosure, got %+v", section.Handoff.NowDisclosure)
	}
	if len(section.Handoff.BlockerDisclosures) != len(section.Handoff.Blockers) {
		t.Fatalf("expected blocker disclosure metadata for each blocker, got %+v", section.Handoff.BlockerDisclosures)
	}
	if len(section.Problems) < 4 {
		t.Fatalf("expected multiple coordination problems, got %+v", section.Problems)
	}
}

func TestNormalizeCoordinationRedactsHandoffFreeText(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 22, 4, 0, 0, 0, time.UTC)
	goalSecret := "Bearer " + strings.Repeat("token", 10)
	section := NormalizeCoordination(CoordinationInputs{
		Handoff: (&handoff.Handoff{
			Session:   "ntm--robot-redesign",
			Status:    "blocked",
			Goal:      "Rotate bearer token " + goalSecret,
			Now:       "Tell ops password=secretpass123 before restart",
			UpdatedAt: now,
			Blockers: []string{
				"Waiting on postgres://user:pass123@localhost:5432/db",
			},
		}),
		Now: now,
	})

	if section.Handoff == nil {
		t.Fatal("expected handoff summary")
	}
	if strings.Contains(section.Handoff.Goal, goalSecret) {
		t.Fatalf("expected redacted goal, got %q", section.Handoff.Goal)
	}
	if strings.Contains(section.Handoff.Now, "secretpass123") {
		t.Fatalf("expected redacted now text, got %q", section.Handoff.Now)
	}
	if len(section.Handoff.Blockers) != 1 {
		t.Fatalf("expected 1 blocker, got %+v", section.Handoff.Blockers)
	}
	if strings.Contains(section.Handoff.Blockers[0], "pass123") {
		t.Fatalf("expected redacted blocker, got %q", section.Handoff.Blockers[0])
	}
	if section.Handoff.GoalDisclosure == nil || section.Handoff.GoalDisclosure.DisclosureState != "redacted" {
		t.Fatalf("expected redacted goal disclosure, got %+v", section.Handoff.GoalDisclosure)
	}
	if section.Handoff.NowDisclosure == nil || section.Handoff.NowDisclosure.DisclosureState != "redacted" {
		t.Fatalf("expected redacted now disclosure, got %+v", section.Handoff.NowDisclosure)
	}
	if len(section.Handoff.BlockerDisclosures) != 1 || section.Handoff.BlockerDisclosures[0].DisclosureState != "redacted" {
		t.Fatalf("expected redacted blocker disclosure, got %+v", section.Handoff.BlockerDisclosures)
	}
	if strings.Contains(section.Handoff.GoalDisclosure.Preview, goalSecret) {
		t.Fatalf("expected redacted goal preview, got %q", section.Handoff.GoalDisclosure.Preview)
	}
}

func TestNormalizeCoordinationMailIgnoresReadMessagesForUnreadCounts(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 22, 4, 0, 0, 0, time.UTC)
	readAt := &agentmail.FlexTime{Time: now.Add(-time.Minute)}

	section := NormalizeCoordination(CoordinationInputs{
		InboxByAgent: map[string][]agentmail.InboxMessage{
			"BlueLake": {
				{ID: 1, Subject: "read urgent", Importance: "urgent", ReadAt: readAt},
				{ID: 2, Subject: "unread urgent", Importance: "urgent", AckRequired: true, CreatedTS: agentmail.FlexTime{Time: now}},
				{ID: 3, Subject: "unread normal", Importance: "normal", CreatedTS: agentmail.FlexTime{Time: now.Add(-time.Minute)}},
			},
		},
		Now:              now,
		ThreadStaleAfter: time.Hour,
	})

	if section.Mail == nil {
		t.Fatal("expected mail summary")
	}
	if section.Mail.TotalUnread != 2 || section.Mail.UrgentUnread != 1 || section.Mail.PendingAck != 1 {
		t.Fatalf("unexpected mail summary: %+v", section.Mail)
	}
	blue := section.Mail.ByAgent["BlueLake"]
	if blue.Unread != 2 || blue.Urgent != 1 || blue.Pending != 1 {
		t.Fatalf("unexpected agent mail stats: %+v", blue)
	}
}

func TestNormalizeCoordinationMarksLongSafeHandoffPreviewOnly(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 22, 4, 0, 0, 0, time.UTC)
	longNow := strings.Repeat("normal handoff detail ", 8)
	section := NormalizeCoordination(CoordinationInputs{
		Handoff: (&handoff.Handoff{
			Session:   "ntm--robot-redesign",
			Status:    "in_progress",
			Goal:      "complete queue wiring",
			Now:       longNow,
			UpdatedAt: now,
		}),
		Now: now,
	})

	if section.Handoff == nil {
		t.Fatal("expected handoff summary")
	}
	if section.Handoff.NowDisclosure == nil {
		t.Fatal("expected now disclosure metadata")
	}
	if section.Handoff.NowDisclosure.DisclosureState != "preview_only" {
		t.Fatalf("disclosure_state = %q, want preview_only", section.Handoff.NowDisclosure.DisclosureState)
	}
	if section.Handoff.Now != section.Handoff.NowDisclosure.Preview {
		t.Fatalf("expected preview-only handoff text to match preview, got now=%q preview=%q", section.Handoff.Now, section.Handoff.NowDisclosure.Preview)
	}
	if section.Handoff.Now == strings.TrimSpace(longNow) {
		t.Fatalf("expected preview-only handoff text to truncate long content, got %q", section.Handoff.Now)
	}
	if !strings.HasSuffix(section.Handoff.NowDisclosure.Preview, "...") {
		t.Fatalf("expected ellipsis in preview, got %q", section.Handoff.NowDisclosure.Preview)
	}
}

func TestNormalizeCoordinationDerivesReservationConflicts(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 22, 4, 0, 0, 0, time.UTC)
	section := NormalizeCoordination(CoordinationInputs{
		Reservations: []agentmail.FileReservation{
			{
				ID:          1,
				PathPattern: "internal/robot/*.go",
				AgentName:   "BlueLake",
				Exclusive:   true,
				CreatedTS:   agentmail.FlexTime{Time: now.Add(-5 * time.Minute)},
				ExpiresTS:   agentmail.FlexTime{Time: now.Add(time.Hour)},
			},
			{
				ID:          2,
				PathPattern: "internal/robot/work_coordination.go",
				AgentName:   "GreenStone",
				Exclusive:   true,
				CreatedTS:   agentmail.FlexTime{Time: now.Add(-3 * time.Minute)},
				ExpiresTS:   agentmail.FlexTime{Time: now.Add(time.Hour)},
			},
		},
		Now: now,
	})

	if section.Reservations == nil {
		t.Fatal("expected reservations summary")
	}
	if section.Reservations.Conflicts != 1 {
		t.Fatalf("conflicts = %d, want 1", section.Reservations.Conflicts)
	}
	found := false
	for _, problem := range section.Problems {
		if problem.Kind == "reservation_conflict" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected derived reservation conflict problem, got %+v", section.Problems)
	}
}

func TestCollectCoordinationContinuesWhenAgentListingFails(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 22, 4, 0, 0, 0, time.UTC)
	projectDir := "/data/projects/ntm"
	reservations := []agentmail.FileReservation{
		{
			ID:          1,
			PathPattern: "internal/robot/*.go",
			AgentName:   "BlueLake",
			Exclusive:   true,
			CreatedTS:   agentmail.FlexTime{Time: now.Add(-10 * time.Minute)},
			ExpiresTS:   agentmail.FlexTime{Time: now.Add(time.Hour)},
		},
		{
			ID:          2,
			PathPattern: "internal/robot/attention_feed.go",
			AgentName:   "GreenStone",
			Exclusive:   true,
			CreatedTS:   agentmail.FlexTime{Time: now.Add(-5 * time.Minute)},
			ExpiresTS:   agentmail.FlexTime{Time: now.Add(2 * time.Hour)},
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req agentmail.JSONRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}

		switch req.Method {
		case "ping":
			writeJSONRPCResult(t, w, req.ID, map[string]any{})
		case "tools/call":
			params, _ := req.Params.(map[string]interface{})
			name, _ := params["name"].(string)
			switch name {
			case "fetch_inbox":
				t.Fatalf("fetch_inbox should not be called when list_agents fails")
			default:
				t.Fatalf("unexpected tool call %q", name)
			}
		case "resources/read":
			params, _ := req.Params.(map[string]interface{})
			uri, _ := params["uri"].(string)
			switch {
			case strings.HasPrefix(uri, "resource://agents/"):
				writeJSONRPCError(t, w, req.ID, -32602, "agents unavailable")
			case strings.HasPrefix(uri, "resource://file_reservations/"):
				writeJSONRPCResult(t, w, req.ID, map[string]any{
					"contents": []map[string]string{{
						"text": mustJSONText(t, reservations),
					}},
				})
			default:
				t.Fatalf("unexpected resource read %q", uri)
			}
		default:
			t.Fatalf("unexpected method %q", req.Method)
		}
	}))
	defer server.Close()

	client := agentmail.NewClient(agentmail.WithBaseURL(server.URL + "/"))
	adapter := NewCoordinationAdapter(WorkCoordinationAdapterConfig{
		ProjectDir:      projectDir,
		AgentMailClient: client,
	})

	section := adapter.collectCoordination(context.Background(), now)
	if section == nil {
		t.Fatal("expected coordination section")
	}
	if !section.Available {
		t.Fatalf("expected coordination to remain available, got reason %q", section.Reason)
	}
	if section.Reservations == nil {
		t.Fatal("expected reservations summary even when list_agents fails")
	}
	if section.Reservations.Active != 2 {
		t.Fatalf("active reservations = %d, want 2", section.Reservations.Active)
	}
	if section.Reservations.Conflicts != 1 {
		t.Fatalf("reservation conflicts = %d, want 1", section.Reservations.Conflicts)
	}
	if !strings.Contains(section.Reason, "list_agents failed") {
		t.Fatalf("reason = %q, want list_agents failure context", section.Reason)
	}
}

// TestCollectCoordinationPeeksAtInboxes guards bd-izmjq: the projection refresh
// observes every agent's inbox, and fetch_inbox marks messages read unless told
// otherwise, so a status call would consume other agents' mail.
func TestCollectCoordinationPeeksAtInboxes(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	markReadByAgent := map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req agentmail.JSONRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		params, _ := req.Params.(map[string]interface{})
		switch req.Method {
		case "ping":
			writeJSONRPCResult(t, w, req.ID, map[string]any{})
		case "resources/read":
			uri, _ := params["uri"].(string)
			text := "[]"
			if strings.HasPrefix(uri, "resource://agents/") {
				text = mustJSONText(t, []agentmail.Agent{{Name: "BlueLake"}, {Name: "GreenStone"}})
			}
			writeJSONRPCResult(t, w, req.ID, map[string]any{"contents": []map[string]string{{"text": text}}})
		case "tools/call":
			name, _ := params["name"].(string)
			args, _ := params["arguments"].(map[string]interface{})
			if name == "fetch_inbox" {
				mu.Lock()
				markRead, set := args["mark_read"]
				if !set {
					markRead = "<unset>"
				}
				markReadByAgent[args["agent_name"].(string)] = markRead
				mu.Unlock()
			}
			writeJSONRPCResult(t, w, req.ID, []any{})
		default:
			t.Errorf("unexpected method %q", req.Method)
		}
	}))
	defer server.Close()

	adapter := NewCoordinationAdapter(WorkCoordinationAdapterConfig{
		ProjectDir:      t.TempDir(),
		AgentMailClient: agentmail.NewClient(agentmail.WithBaseURL(server.URL + "/")),
	})
	section := adapter.collectCoordination(context.Background(), time.Now())
	if section == nil || !section.Available {
		t.Fatalf("coordination section = %+v, want available", section)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(markReadByAgent) != 2 {
		t.Fatalf("fetched inboxes for %v, want BlueLake and GreenStone", markReadByAgent)
	}
	for agent, markRead := range markReadByAgent {
		if markRead != false {
			t.Fatalf("fetch_inbox for %s sent mark_read=%v, want false", agent, markRead)
		}
	}
}

func TestCoordinationAdapterAgentMailAvailabilityHonorsCanceledContext(t *testing.T) {
	requests := make(chan struct{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- struct{}{}
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer server.Close()

	adapter := NewCoordinationAdapter(WorkCoordinationAdapterConfig{
		ProjectDir:      t.TempDir(),
		AgentMailClient: agentmail.NewClient(agentmail.WithBaseURL(server.URL + "/")),
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if adapter.Available(ctx) {
		t.Fatal("adapter reported available for canceled context")
	}
	section := adapter.collectCoordination(ctx, time.Now())
	if section == nil || section.Available || !strings.Contains(section.Reason, "agent mail unavailable") {
		t.Fatalf("canceled coordination section = %+v", section)
	}
	select {
	case <-requests:
		t.Fatal("canceled adapter availability reached Agent Mail")
	default:
	}
}

func TestWorkAdapterCollectCancelsBlockedBVWork(t *testing.T) {
	projectDir := t.TempDir()
	for _, name := range []string{".git", ".beads"} {
		if err := os.Mkdir(filepath.Join(projectDir, name), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
	}
	binDir := t.TempDir()
	startedPath := filepath.Join(t.TempDir(), "bv-started")
	script := "#!/bin/sh\n: > " + startedPath + "\nexec /bin/sleep 30\n"
	if err := os.WriteFile(filepath.Join(binDir, "bv"), []byte(script), 0o700); err != nil {
		t.Fatalf("write blocking bv: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	adapter := NewWorkAdapter(DefaultWorkCoordinationAdapterConfig(projectDir))
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := adapter.Collect(ctx)
		result <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(startedPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("blocking bv collection did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	started := time.Now()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Collect error=%v, want context canceled", err)
		}
		if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
			t.Fatalf("blocked BV collection ignored cancellation for %v", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked BV collection did not join after cancellation")
	}
}

// fakeBRScript answers the four br reads the work source makes.
const fakeBRScript = `#!/bin/sh
for arg in "$@"; do
  case "$arg" in
    stats) echo '{"summary":{"total_issues":3,"open_issues":2,"in_progress_issues":1,"blocked_issues":1,"ready_issues":1,"closed_issues":0}}'; exit 0 ;;
    ready) echo '[{"id":"bd-r","title":"Ready bead","priority":1}]'; exit 0 ;;
    list) echo '[{"id":"bd-p","title":"Bead in progress","assignee":"BlueLake","updated_at":"2026-10-19T00:00:00Z"}]'; exit 0 ;;
    blocked) echo '[{"id":"bd-b","title":"Blocked bead"}]'; exit 0 ;;
  esac
done
exit 1
`

// installFakeBeadsTools puts fake br and bv scripts first on PATH and returns
// a project directory with an empty .beads.
func installFakeBeadsTools(t *testing.T, brScript, bvScript string) string {
	t.Helper()
	projectDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(projectDir, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	binDir := t.TempDir()
	for name, script := range map[string]string{"br": brScript, "bv": bvScript} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o700); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return projectDir
}

// newFakeAgentMail serves one agent whose inbox holds an urgent unread message.
func newFakeAgentMail(t *testing.T) *agentmail.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req agentmail.JSONRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		params, _ := req.Params.(map[string]interface{})
		switch req.Method {
		case "ping":
			writeJSONRPCResult(t, w, req.ID, map[string]any{})
		case "resources/read":
			uri, _ := params["uri"].(string)
			text := "[]"
			if strings.HasPrefix(uri, "resource://agents/") {
				text = mustJSONText(t, []agentmail.Agent{{Name: "BlueLake"}})
			}
			writeJSONRPCResult(t, w, req.ID, map[string]any{"contents": []map[string]string{{"text": text}}})
		case "tools/call":
			writeJSONRPCResult(t, w, req.ID, []map[string]any{{
				"id":         1,
				"subject":    "Need review",
				"from":       "GreenStone",
				"importance": "urgent",
				"created_ts": "2026-10-19T00:00:00Z",
			}})
		default:
			t.Errorf("unexpected method %q", req.Method)
		}
	}))
	t.Cleanup(server.Close)
	return agentmail.NewClient(agentmail.WithBaseURL(server.URL + "/"))
}

// bd-jv2dq: work and coordination were one source, and the aggregator drops
// the batch of a source whose Collect fails, so a slow br or bv also threw
// away the coordination section Agent Mail had answered in time.
func TestWorkAndCoordinationSourcesDegradeIndependently(t *testing.T) {
	blocking := "#!/bin/sh\nexec /bin/sleep 30\n"
	projectDir := installFakeBeadsTools(t, blocking, blocking)
	config := DefaultWorkCoordinationAdapterConfig(projectDir)
	config.AgentMailClient = newFakeAgentMail(t)

	aggregator := NewSignalAggregator(0)
	aggregator.RegisterAdapter(NewWorkAdapter(config))
	aggregator.RegisterAdapter(NewCoordinationAdapter(config))
	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	started := time.Now()
	signals, err := aggregator.Collect(ctx)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("collection took %v with a 750ms budget", elapsed)
	}

	if signals.Coordination == nil || !signals.Coordination.Available || signals.Coordination.Mail == nil {
		t.Fatalf("coordination = %+v, want the mail Agent Mail answered", signals.Coordination)
	}
	if got := signals.Coordination.Mail.UrgentUnread; got != 1 {
		t.Fatalf("urgent unread = %d, want 1", got)
	}
	if source := signals.Health.Sources["coordination"]; !source.Available || source.Degraded {
		t.Fatalf("coordination health = %+v, want available", source)
	}
	work := signals.Health.Sources["work"]
	if work.Available || !work.Degraded || !strings.Contains(work.LastError, "deadline") {
		t.Fatalf("work health = %+v, want degraded by the deadline", work)
	}
}

// bd-jv2dq: triage took longer than the whole source budget, so the work
// section was lost even though br had answered. Triage is enrichment: the
// section now carries br's data and returns before the deadline.
func TestWorkAdapterReturnsBRDataWhenTriageOverrunsTheBudget(t *testing.T) {
	projectDir := installFakeBeadsTools(t, fakeBRScript, "#!/bin/sh\nexec /bin/sleep 30\n")

	aggregator := NewSignalAggregator(0)
	aggregator.RegisterAdapter(NewWorkAdapter(DefaultWorkCoordinationAdapterConfig(projectDir)))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	signals, err := aggregator.Collect(ctx)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("work collection ran into its deadline")
	}

	work := signals.Work
	if work == nil || !work.Available || work.Summary == nil || work.Summary.Total != 3 {
		t.Fatalf("work = %+v, want br's summary", work)
	}
	if work.Triage != nil {
		t.Fatalf("triage = %+v, want none from a bv that never answered", work.Triage)
	}
	if len(work.Ready) != 1 || len(work.Blocked) != 1 || len(work.InProgress) != 1 {
		t.Fatalf("work items ready=%v blocked=%v in_progress=%v, want one of each", work.Ready, work.Blocked, work.InProgress)
	}
	if source := signals.Health.Sources["work"]; !source.Available || source.Degraded {
		t.Fatalf("work health = %+v, want available", source)
	}
}

func TestWorkAdapterUsesTriageThatIsReadyInTime(t *testing.T) {
	triage := `{"data_hash":"abc","triage":{"quick_ref":{"actionable_count":1},"recommendations":[{"id":"bd-r","title":"Ready bead","type":"task","priority":1,"score":0.5}]}}`
	projectDir := installFakeBeadsTools(t, fakeBRScript, "#!/bin/sh\nprintf '%s\\n' '"+triage+"'\n")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	batch, err := NewWorkAdapter(DefaultWorkCoordinationAdapterConfig(projectDir)).Collect(ctx)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	work := batch.Work
	if work == nil || work.Triage == nil || work.Triage.TopRecommendation == nil || work.Triage.TopRecommendation.ID != "bd-r" {
		t.Fatalf("work = %+v, want bv's top recommendation", work)
	}
	if len(work.Ready) != 1 || work.Ready[0].Score == nil {
		t.Fatalf("ready items = %+v, want the triage score on bd-r", work.Ready)
	}
}

// bd-jv2dq: inboxes were read one at a time, 0.5-1.2 s each on the shared
// server, so a few dozen agents never fit the source budget.
func TestFetchInboxesReadsConcurrentlyWithinTheBound(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0
	markRead := map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req agentmail.JSONRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		params, _ := req.Params.(map[string]interface{})
		args, _ := params["arguments"].(map[string]interface{})
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		agentName, _ := args["agent_name"].(string)
		value, set := args["mark_read"]
		if !set {
			value = "<unset>"
		}
		markRead[agentName] = value
		mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		writeJSONRPCResult(t, w, req.ID, []any{})
	}))
	defer server.Close()

	agents := []agentmail.Agent{{Name: "HumanOverseer"}}
	for i := 0; i < 3*inboxFetchConcurrency; i++ {
		agents = append(agents, agentmail.Agent{Name: "Agent" + strconv.Itoa(i)})
	}
	client := agentmail.NewClient(agentmail.WithBaseURL(server.URL + "/"))
	started := time.Now()
	inboxes := fetchInboxes(context.Background(), client, "/data/projects/ntm", agents)
	elapsed := time.Since(started)

	if len(inboxes) != 3*inboxFetchConcurrency {
		t.Fatalf("fetched %d inboxes, want %d", len(inboxes), 3*inboxFetchConcurrency)
	}
	if _, ok := inboxes["HumanOverseer"]; ok {
		t.Fatal("fetched the HumanOverseer inbox")
	}
	mu.Lock()
	defer mu.Unlock()
	if maxInFlight > inboxFetchConcurrency || maxInFlight < 2 {
		t.Fatalf("max concurrent inbox reads = %d, want 2..%d", maxInFlight, inboxFetchConcurrency)
	}
	if serial := time.Duration(len(inboxes)) * 50 * time.Millisecond; elapsed >= serial {
		t.Fatalf("fan-out took %v, no faster than reading serially (%v)", elapsed, serial)
	}
	for agent, value := range markRead {
		if value != false {
			t.Fatalf("fetch_inbox for %s sent mark_read=%v, want false", agent, value)
		}
	}
}

func mustJSONText(t *testing.T, value any) string {
	t.Helper()

	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JSON text: %v", err)
	}
	return string(data)
}

func writeJSONRPCResult(t *testing.T, w http.ResponseWriter, id interface{}, result any) {
	t.Helper()

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal JSON-RPC result: %v", err)
	}
	writeJSONRPCResponse(t, w, agentmail.JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  data,
	})
}

func writeJSONRPCError(t *testing.T, w http.ResponseWriter, id interface{}, code int, message string) {
	t.Helper()

	writeJSONRPCResponse(t, w, agentmail.JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &agentmail.JSONRPCError{Code: code, Message: message},
	})
}

func writeJSONRPCResponse(t *testing.T, w http.ResponseWriter, resp agentmail.JSONRPCResponse) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		t.Fatalf("encode JSON-RPC response: %v", err)
	}
}

func stringPtr(v string) *string {
	return &v
}

func TestComputeSourceHealth(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 22, 4, 0, 0, 0, time.UTC)
	config := SourceHealthConfig{
		StaleAfter:    30 * time.Second,
		DegradedAfter: 60 * time.Second,
	}

	tests := []struct {
		name          string
		results       []AdapterResult
		wantAllFresh  bool
		wantDegraded  []string
		wantAvailable map[string]bool
	}{
		{
			name: "all_healthy",
			results: []AdapterResult{
				{Name: "work_coordination", Available: true, CollectedAt: now.Add(-5 * time.Second)},
				{Name: "tmux", Available: true, CollectedAt: now.Add(-10 * time.Second)},
			},
			wantAllFresh:  true,
			wantDegraded:  nil,
			wantAvailable: map[string]bool{"work_coordination": true, "tmux": true},
		},
		{
			name: "one_stale",
			results: []AdapterResult{
				{Name: "work_coordination", Available: true, CollectedAt: now.Add(-5 * time.Second)},
				{Name: "tmux", Available: true, CollectedAt: now.Add(-60 * time.Second)},
			},
			wantAllFresh:  false,
			wantDegraded:  []string{"tmux"},
			wantAvailable: map[string]bool{"work_coordination": true, "tmux": true},
		},
		{
			name: "one_unavailable",
			results: []AdapterResult{
				{Name: "work_coordination", Available: true, CollectedAt: now.Add(-5 * time.Second)},
				{Name: "beads", Available: false},
			},
			wantAllFresh:  false,
			wantDegraded:  []string{"beads"},
			wantAvailable: map[string]bool{"work_coordination": true, "beads": false},
		},
		{
			name: "collection_error",
			results: []AdapterResult{
				{Name: "work_coordination", Available: true, CollectedAt: now.Add(-5 * time.Second)},
				{Name: "caut", Available: true, Error: errTest},
			},
			wantAllFresh:  false,
			wantDegraded:  []string{"caut"},
			wantAvailable: map[string]bool{"work_coordination": true, "caut": false},
		},
		{
			name: "awaiting_first_collection",
			results: []AdapterResult{
				{Name: "mail", Available: true}, // No CollectedAt
			},
			wantAllFresh:  false,
			wantDegraded:  []string{"mail"},
			wantAvailable: map[string]bool{"mail": true},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			health := ComputeSourceHealth(tc.results, config, now)

			if health.AllFresh != tc.wantAllFresh {
				t.Errorf("AllFresh = %v, want %v", health.AllFresh, tc.wantAllFresh)
			}

			if len(health.Degraded) != len(tc.wantDegraded) {
				t.Errorf("Degraded = %v, want %v", health.Degraded, tc.wantDegraded)
			}

			for name, wantAvail := range tc.wantAvailable {
				if info, ok := health.Sources[name]; !ok {
					t.Errorf("missing source %q", name)
				} else if info.Available != wantAvail {
					t.Errorf("source %q: Available = %v, want %v", name, info.Available, wantAvail)
				}
			}
		})
	}
}

func TestComputeDegradedFeatures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		health       *SourceHealthSection
		wantFeatures int
	}{
		{
			name: "all_fresh_no_degraded",
			health: &SourceHealthSection{
				AllFresh: true,
				Sources:  map[string]SourceInfo{},
				Degraded: nil,
			},
			wantFeatures: 0,
		},
		{
			name: "one_degraded_source",
			health: &SourceHealthSection{
				AllFresh: false,
				Sources: map[string]SourceInfo{
					"work": {Available: false, Degraded: true},
				},
				Degraded: []string{"work"},
			},
			wantFeatures: 2, // work_section, bead_triage
		},
		{
			name: "work_and_coordination_degraded",
			health: &SourceHealthSection{
				AllFresh: false,
				Sources: map[string]SourceInfo{
					"work":         {Available: false, Degraded: true},
					"coordination": {Available: false, Degraded: true},
				},
				Degraded: []string{"work", "coordination"},
			},
			wantFeatures: 3, // work_section, bead_triage, coordination_section
		},
		{
			name: "multiple_degraded_sources",
			health: &SourceHealthSection{
				AllFresh: false,
				Sources: map[string]SourceInfo{
					"tmux":  {Available: false, Degraded: true},
					"beads": {Available: false, Degraded: true},
				},
				Degraded: []string{"tmux", "beads"},
			},
			wantFeatures: 6, // session_list, agent_detection, pane_output + work_section, bead_triage, dependency_graph
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			features := ComputeDegradedFeatures(tc.health)
			if len(features) != tc.wantFeatures {
				t.Errorf("got %d features, want %d: %+v", len(features), tc.wantFeatures, features)
			}
		})
	}
}

func TestSourceHealthReasonCodes(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 22, 4, 0, 0, 0, time.UTC)
	config := DefaultSourceHealthConfig()

	// Test unavailable source gets correct reason code
	health := ComputeSourceHealth([]AdapterResult{
		{Name: "test", Available: false},
	}, config, now)

	if info, ok := health.Sources["test"]; !ok {
		t.Fatal("missing test source")
	} else if info.ReasonCode != ReasonHealthSourceUnavailable {
		t.Errorf("ReasonCode = %q, want %q", info.ReasonCode, ReasonHealthSourceUnavailable)
	}

	// Test stale source gets correct reason code
	health = ComputeSourceHealth([]AdapterResult{
		{Name: "stale", Available: true, CollectedAt: now.Add(-2 * time.Minute)},
	}, config, now)

	if info, ok := health.Sources["stale"]; !ok {
		t.Fatal("missing stale source")
	} else if info.ReasonCode != ReasonHealthSourceStale {
		t.Errorf("ReasonCode = %q, want %q", info.ReasonCode, ReasonHealthSourceStale)
	}

	// Test healthy source gets OK reason code
	health = ComputeSourceHealth([]AdapterResult{
		{Name: "healthy", Available: true, CollectedAt: now.Add(-5 * time.Second)},
	}, config, now)

	if info, ok := health.Sources["healthy"]; !ok {
		t.Fatal("missing healthy source")
	} else if info.ReasonCode != ReasonHealthOK {
		t.Errorf("ReasonCode = %q, want %q", info.ReasonCode, ReasonHealthOK)
	}
}

var errTest = &testError{}

type testError struct{}

func (e *testError) Error() string { return "test error" }
