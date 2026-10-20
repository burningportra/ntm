package serve

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Dicklesworthstone/ntm/internal/agentmail"
	"github.com/Dicklesworthstone/ntm/internal/robot/adapters"
)

// TestAgentMailRefusalsMapToHTTPStatus replays refusals the Rust (am 0.3.36)
// and Python (mcp_agent_mail 0.3.4) Agent Mail servers sent live, through the
// router, and checks the status and error code serve answers with. Before
// bd-m0gm2 every one of them answered 500 INTERNAL_ERROR.
func TestAgentMailRefusalsMapToHTTPStatus(t *testing.T) {
	toolError := func(text string) func(map[string]interface{}) (interface{}, *agentmail.JSONRPCError) {
		return func(map[string]interface{}) (interface{}, *agentmail.JSONRPCError) {
			envelope, err := json.Marshal(map[string]interface{}{
				"content": []map[string]string{{"type": "text", "text": text}},
				"isError": true,
			})
			if err != nil {
				t.Fatalf("marshal tool error: %v", err)
			}
			return json.RawMessage(envelope), nil
		}
	}
	const (
		rustReservationActive = `{"error":{"type":"RESERVATION_ACTIVE","message":"Reservation still shows recent activity; refusing forced release.","recoverable":true,"data":{"file_reservation_id":14598,"stale_reasons":["agent_recently_active","no_recent_mail_activity>900s","path_pattern_unmatched"]}}}`
		rustReservationAbsent = `{"error":{"type":"NOT_FOUND","message":"File reservation id=999999999 not found for project '/Volumes/1tb/Projects/ntm'.","recoverable":true,"data":{"file_reservation_id":999999999}}}`
		rustAgentAbsent       = `{"error":{"type":"AGENT_NOT_FOUND","message":"Agent 'NoSuchAgentZzq' not found in project 'volumes-1tb-projects-ntm'","recoverable":true,"data":{"agent_name":"NoSuchAgentZzq","project":"volumes-1tb-projects-ntm"}}}`
		rustProjectAbsent     = `{"error":{"type":"NOT_FOUND","message":"Project '/nonexistent/bd-m0gm2-probe' not found. Use ensure_project to create a new project, or check spelling.","recoverable":true}}`
		rustSenderRefused     = `{"error":{"type":"SENDER_TOKEN_MISMATCH","message":"sender_token does not match the registered token for agent 'RoseRiver'. Use the registration_token returned by register_agent.","recoverable":false}}`
		pyReservationActive   = `Error calling tool 'force_release_file_reservation': Reservation still shows recent activity; refusing forced release.`
		pyForceReleaseRefused = `Error calling tool 'force_release_file_reservation': force_release_file_reservation requires registration_token for agent 'RedStone', unless this MCP session has already authenticated as that agent.`
		pyWhoisRefused        = `Error calling tool 'whois': whois requires registration_token for agent 'GreenCastle', unless this MCP session has already authenticated as that agent.`
		pySearchAgent         = `Error calling tool 'search_messages': search_messages requires an authenticated agent for project '/p'. Provide agent_name plus registration_token (or agent_token for resources), or authenticate in this session first.`
	)

	tests := []struct {
		name     string
		method   string
		path     string
		body     string
		handlers map[string]func(map[string]interface{}) (interface{}, *agentmail.JSONRPCError)
		status   int
		code     string
	}{
		{
			name: "rust force-release of an active reservation", method: http.MethodPost,
			path: "/api/v1/reservations/14598/force-release", body: `{"agent_name":"RoseRiver"}`,
			handlers: map[string]func(map[string]interface{}) (interface{}, *agentmail.JSONRPCError){"force_release_file_reservation": toolError(rustReservationActive)},
			status:   http.StatusConflict, code: ErrCodeConflict,
		},
		{
			name: "python force-release of an active reservation", method: http.MethodPost,
			path: "/api/v1/reservations/2/force-release", body: `{"agent_name":"RedStone"}`,
			handlers: map[string]func(map[string]interface{}) (interface{}, *agentmail.JSONRPCError){"force_release_file_reservation": toolError(pyReservationActive)},
			status:   http.StatusConflict, code: ErrCodeConflict,
		},
		{
			name: "python force-release without a token", method: http.MethodPost,
			path: "/api/v1/reservations/2/force-release", body: `{"agent_name":"RedStone"}`,
			handlers: map[string]func(map[string]interface{}) (interface{}, *agentmail.JSONRPCError){"force_release_file_reservation": toolError(pyForceReleaseRefused)},
			status:   http.StatusUnauthorized, code: ErrCodeUnauthorized,
		},
		{
			name: "rust force-release of a missing id", method: http.MethodPost,
			path: "/api/v1/reservations/999999999/force-release", body: `{"agent_name":"RoseRiver"}`,
			handlers: map[string]func(map[string]interface{}) (interface{}, *agentmail.JSONRPCError){"force_release_file_reservation": toolError(rustReservationAbsent)},
			status:   http.StatusNotFound, code: ErrCodeNotFound,
		},
		{
			name: "rust whois of a missing agent", method: http.MethodGet,
			path:     "/api/v1/mail/agents/NoSuchAgentZzq",
			handlers: map[string]func(map[string]interface{}) (interface{}, *agentmail.JSONRPCError){"whois": toolError(rustAgentAbsent)},
			status:   http.StatusNotFound, code: ErrCodeAgentNotFound,
		},
		{
			name: "python whois without a token", method: http.MethodGet,
			path:     "/api/v1/mail/agents/GreenCastle",
			handlers: map[string]func(map[string]interface{}) (interface{}, *agentmail.JSONRPCError){"whois": toolError(pyWhoisRefused)},
			status:   http.StatusUnauthorized, code: ErrCodeUnauthorized,
		},
		{
			name: "python search without an agent", method: http.MethodGet,
			path:     "/api/v1/mail/search?q=probe",
			handlers: map[string]func(map[string]interface{}) (interface{}, *agentmail.JSONRPCError){"search_messages": toolError(pySearchAgent)},
			status:   http.StatusUnauthorized, code: ErrCodeUnauthorized,
		},
		{
			name: "rust search in a missing project", method: http.MethodGet,
			path:     "/api/v1/mail/search?q=probe",
			handlers: map[string]func(map[string]interface{}) (interface{}, *agentmail.JSONRPCError){"search_messages": toolError(rustProjectAbsent)},
			status:   http.StatusNotFound, code: ErrCodeNotFound,
		},
		{
			name: "rust send with a wrong sender_token", method: http.MethodPost,
			path: "/api/v1/mail/messages", body: `{"sender_name":"RoseRiver","to":["RoseRiver"],"subject":"s","body_md":"b"}`,
			handlers: map[string]func(map[string]interface{}) (interface{}, *agentmail.JSONRPCError){"send_message": toolError(rustSenderRefused)},
			status:   http.StatusUnauthorized, code: ErrCodeUnauthorized,
		},
		{
			name: "python message resource without a token", method: http.MethodGet,
			path: "/api/v1/mail/messages/3?agent_name=GreenCastle",
			handlers: map[string]func(map[string]interface{}) (interface{}, *agentmail.JSONRPCError){
				"resource:resource://message/3": func(map[string]interface{}) (interface{}, *agentmail.JSONRPCError) {
					return nil, &agentmail.JSONRPCError{Code: 0, Message: "Error reading resource from template 'resource://message/3?agent=GreenCastle&format=json&project=%2Fp': resource://message/{message_id} requires agent_token for agent 'GreenCastle', unless this MCP session has already authenticated as that agent."}
				},
			},
			status: http.StatusUnauthorized, code: ErrCodeUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := setupTestServer(t)
			srv.mailClient = newMockAgentMailMCPClient(t, tt.handlers)

			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			if tt.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			rec := httptest.NewRecorder()
			srv.Router().ServeHTTP(rec, req)

			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, tt.status, rec.Body.String())
			}
			var resp APIError
			if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if resp.ErrorCode != tt.code {
				t.Fatalf("error_code = %q, want %q (error: %s)", resp.ErrorCode, tt.code, resp.Error)
			}
		})
	}
}

// bd-kar03: redaction must not depend on which read path a caller uses.
// sanitizeInboxMessage/sanitizeMessage routed subject+body through
// redaction.ScanAndRedact, but thread-summary, reply, and search returned raw
// content under the SAME mail:read permission — so one secret came back
// REDACTED from GET /mail/inbox?include_bodies=true and VERBATIM from
// GET /mail/threads/{id}/summary?include_examples=true.
func TestMailReadPathsRedactUniformly(t *testing.T) {
	const secret = "sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

	// Establish that the shared redactor actually removes this token, so the
	// assertions below are meaningful rather than vacuous.
	redactedProbe, disclosure := adapters.NormalizeDisclosureText("key " + secret)
	if strings.Contains(redactedProbe, secret) || disclosure == nil || disclosure.DisclosureState != "redacted" {
		t.Skipf("shared redactor does not treat the probe token as a secret (state=%v); nothing to assert", disclosure)
	}

	t.Run("thread summary examples", func(t *testing.T) {
		examples := sanitizeInboxMessages([]agentmail.InboxMessage{{
			ID:      1,
			Subject: "leak " + secret,
			BodyMD:  "body " + secret,
		}}, true)
		if len(examples) != 1 {
			t.Fatalf("got %d examples, want 1", len(examples))
		}
		if strings.Contains(examples[0].Subject, secret) {
			t.Errorf("thread-summary example subject leaked the secret: %q", examples[0].Subject)
		}
		if examples[0].BodyMD != nil && strings.Contains(*examples[0].BodyMD, secret) {
			t.Errorf("thread-summary example body leaked the secret: %q", *examples[0].BodyMD)
		}
	})

	t.Run("thread summary text", func(t *testing.T) {
		safe := sanitizeThreadSummary(agentmail.ThreadSummary{
			ThreadID:     "br-1",
			Participants: []string{"BlueLake"},
			KeyPoints:    []string{"the key is " + secret},
			ActionItems:  []string{"rotate " + secret},
		})
		for _, line := range append(append([]string{}, safe.KeyPoints...), safe.ActionItems...) {
			if strings.Contains(line, secret) {
				t.Errorf("thread summary text leaked the secret: %q", line)
			}
		}
		if safe.ThreadID != "br-1" || len(safe.Participants) != 1 {
			t.Errorf("sanitization dropped non-sensitive fields: %+v", safe)
		}
	})

	t.Run("search results", func(t *testing.T) {
		safe := sanitizeSearchResults([]agentmail.SearchResult{{
			ID:      7,
			Subject: "found " + secret,
			From:    "BlueLake",
		}})
		if len(safe) != 1 {
			t.Fatalf("got %d results, want 1", len(safe))
		}
		if strings.Contains(safe[0].Subject, secret) {
			t.Errorf("search result subject leaked the secret: %q", safe[0].Subject)
		}
		if safe[0].ID != 7 || safe[0].From != "BlueLake" {
			t.Errorf("sanitization dropped non-sensitive fields: %+v", safe[0])
		}
	})

	t.Run("reply message", func(t *testing.T) {
		safe := sanitizeMessage(&agentmail.Message{
			ID:      9,
			Subject: "re: " + secret,
			BodyMD:  "body " + secret,
		})
		if safe == nil {
			t.Fatal("sanitizeMessage returned nil")
		}
		if strings.Contains(safe.Subject, secret) {
			t.Errorf("reply subject leaked the secret: %q", safe.Subject)
		}
		if safe.BodyMD != nil && strings.Contains(*safe.BodyMD, secret) {
			t.Errorf("reply body leaked the secret: %q", *safe.BodyMD)
		}
	})
}
