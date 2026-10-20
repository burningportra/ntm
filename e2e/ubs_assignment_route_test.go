//go:build e2e
// +build e2e

// Package e2e contains end-to-end tests for NTM UBS routing.
// ubs_assignment_route_test.go validates assignment-aware routing via Agent Mail.
//
// Bead: bd-3rxri - Route UBS findings to assigned agents
package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/agentmail"
	"github.com/Dicklesworthstone/ntm/internal/assignment"
	"github.com/Dicklesworthstone/ntm/internal/scanner"
)

func TestE2E_UBSAssignmentRouting(t *testing.T) {
	CommonE2EPrerequisites(t)

	ctx := context.Background()
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	projectKey := tmpDir + "/e2e_ubs_route"
	session := "e2e_ubs_route"
	agentName := session + "_claude_1"

	client := agentmail.NewClient(agentmail.WithProjectKey(projectKey))
	if !client.IsAvailable() {
		t.Skip("Agent Mail not available; skipping routing test")
	}

	// Ensure project exists before registering agent
	if _, err := client.EnsureProject(ctx, projectKey); err != nil {
		t.Fatalf("[E2E-UBS-ROUTE] ensure project failed: %v", err)
	}

	if _, err := client.RegisterAgent(ctx, agentmail.RegisterAgentOptions{
		ProjectKey:      projectKey,
		Program:         "ntm",
		Model:           "scanner",
		Name:            agentName,
		TaskDescription: "e2e routing target",
	}); err != nil {
		t.Fatalf("[E2E-UBS-ROUTE] register agent failed: %v", err)
	}

	store := assignment.NewStore(session)
	if _, err := store.Assign("bd-1", "Fix internal/scanner/scanner.go", 1, "claude", agentName, "Work on internal/scanner/scanner.go"); err != nil {
		t.Fatalf("[E2E-UBS-ROUTE] assign failed: %v", err)
	}

	result := &scanner.ScanResult{
		Project: projectKey,
		Totals: scanner.ScanTotals{
			Warning: 1,
		},
		Findings: []scanner.Finding{
			{
				File:     "internal/scanner/scanner.go",
				Line:     10,
				Severity: scanner.SeverityWarning,
				Message:  "test warning",
				RuleID:   "rule-1",
			},
		},
	}

	if err := scanner.NotifyScanResults(ctx, client, result, projectKey); err != nil {
		t.Fatalf("[E2E-UBS-ROUTE] notify failed: %v", err)
	}

	var inbox []agentmail.InboxMessage
	var fetchErr error
	for i := 0; i < 5; i++ {
		inbox, fetchErr = client.FetchInbox(ctx, agentmail.FetchInboxOptions{
			ProjectKey:    projectKey,
			AgentName:     agentName,
			IncludeBodies: true,
			Limit:         5,
		})
		if fetchErr == nil && len(inbox) > 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if fetchErr != nil {
		t.Fatalf("[E2E-UBS-ROUTE] fetch inbox failed: %v", fetchErr)
	}
	if len(inbox) == 0 {
		t.Fatalf("[E2E-UBS-ROUTE] expected inbox message for %s", agentName)
	}

	found := false
	for _, msg := range inbox {
		if strings.Contains(msg.Subject, "[Scan]") && strings.Contains(msg.BodyMD, "bd-1") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("[E2E-UBS-ROUTE] expected scan message mentioning bd-1, got subjects: %s", inboxSubjects(inbox))
	}
}

func inboxSubjects(msgs []agentmail.InboxMessage) string {
	if len(msgs) == 0 {
		return ""
	}
	subjects := make([]string, 0, len(msgs))
	for _, msg := range msgs {
		subjects = append(subjects, msg.Subject)
	}
	return fmt.Sprintf("%v", subjects)
}
