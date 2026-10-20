package agentmail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// mockMCPHandler returns an http.Handler that dispatches JSON-RPC tools/call
// requests to tool-specific handlers. Each handler receives the raw arguments
// map and returns a JSON-encodable result or an error string.
func mockMCPHandler(t *testing.T, handlers map[string]func(args map[string]interface{}) (interface{}, *JSONRPCError)) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
			return
		}

		var req JSONRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")

		if req.Method == "tools/call" {
			params, _ := req.Params.(map[string]interface{})
			toolName, _ := params["name"].(string)
			args, _ := params["arguments"].(map[string]interface{})

			handler, ok := handlers[toolName]
			if !ok {
				resp := JSONRPCResponse{
					JSONRPC: "2.0",
					ID:      req.ID,
					Error:   &JSONRPCError{Code: -32601, Message: "unknown tool: " + toolName},
				}
				json.NewEncoder(w).Encode(resp)
				return
			}

			result, rpcErr := handler(args)
			if rpcErr != nil {
				resp := JSONRPCResponse{
					JSONRPC: "2.0",
					ID:      req.ID,
					Error:   rpcErr,
				}
				json.NewEncoder(w).Encode(resp)
				return
			}

			resultJSON, _ := json.Marshal(result)
			resp := JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result:  json.RawMessage(resultJSON),
			}
			json.NewEncoder(w).Encode(resp)
			return
		}

		if req.Method == "resources/read" {
			params, _ := req.Params.(map[string]interface{})
			uri, _ := params["uri"].(string)

			handler, ok := handlers["resource:"+uri]
			if !ok {
				// Default: return not-found style error
				resp := JSONRPCResponse{
					JSONRPC: "2.0",
					ID:      req.ID,
					Error:   &JSONRPCError{Code: -32602, Message: "resource not found: " + uri},
				}
				json.NewEncoder(w).Encode(resp)
				return
			}

			result, rpcErr := handler(nil)
			if rpcErr != nil {
				resp := JSONRPCResponse{
					JSONRPC: "2.0",
					ID:      req.ID,
					Error:   rpcErr,
				}
				json.NewEncoder(w).Encode(resp)
				return
			}

			resultJSON, _ := json.Marshal(result)
			resp := JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result:  json.RawMessage(resultJSON),
			}
			json.NewEncoder(w).Encode(resp)
			return
		}

		resp := JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &JSONRPCError{Code: -32601, Message: "unknown method: " + req.Method},
		}
		json.NewEncoder(w).Encode(resp)
	})
}

func TestEnsureProject(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(mockMCPHandler(t, map[string]func(args map[string]interface{}) (interface{}, *JSONRPCError){
		"ensure_project": func(args map[string]interface{}) (interface{}, *JSONRPCError) {
			key, _ := args["human_key"].(string)
			return Project{
				ID:       1,
				Slug:     "ntm",
				HumanKey: key,
			}, nil
		},
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	project, err := c.EnsureProject(context.Background(), "/data/projects/ntm")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if project.Slug != "ntm" {
		t.Errorf("slug = %q, want ntm", project.Slug)
	}
	if project.HumanKey != "/data/projects/ntm" {
		t.Errorf("human_key = %q, want /data/projects/ntm", project.HumanKey)
	}
}

func TestEnsureProject_Error(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(mockMCPHandler(t, map[string]func(args map[string]interface{}) (interface{}, *JSONRPCError){
		"ensure_project": func(args map[string]interface{}) (interface{}, *JSONRPCError) {
			return nil, &JSONRPCError{Code: -32000, Message: "project creation failed"}
		},
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	_, err := c.EnsureProject(context.Background(), "/bad")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestCreateAgentIdentity(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(mockMCPHandler(t, map[string]func(args map[string]interface{}) (interface{}, *JSONRPCError){
		"create_agent_identity": func(args map[string]interface{}) (interface{}, *JSONRPCError) {
			return Agent{
				ID:      42,
				Name:    "GreenCastle",
				Program: args["program"].(string),
				Model:   args["model"].(string),
			}, nil
		},
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	agent, err := c.CreateAgentIdentity(context.Background(), RegisterAgentOptions{
		ProjectKey:      "/test",
		Program:         "claude-code",
		Model:           "opus-4.5",
		Name:            "GreenCastle",
		TaskDescription: "testing",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if agent.Name != "GreenCastle" {
		t.Errorf("name = %q, want GreenCastle", agent.Name)
	}
	if agent.Program != "claude-code" {
		t.Errorf("program = %q, want claude-code", agent.Program)
	}
}

func TestWhois(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(mockMCPHandler(t, map[string]func(args map[string]interface{}) (interface{}, *JSONRPCError){
		"whois": func(args map[string]interface{}) (interface{}, *JSONRPCError) {
			return Agent{
				ID:              10,
				Name:            args["agent_name"].(string),
				Program:         "claude-code",
				Model:           "opus-4.5",
				TaskDescription: "test coverage",
			}, nil
		},
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	agent, err := c.Whois(context.Background(), "/test", "BlueLake", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if agent.Name != "BlueLake" {
		t.Errorf("name = %q, want BlueLake", agent.Name)
	}
}

func TestSendMessage(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(mockMCPHandler(t, map[string]func(args map[string]interface{}) (interface{}, *JSONRPCError){
		"send_message": func(args map[string]interface{}) (interface{}, *JSONRPCError) {
			return SendResult{
				Deliveries: []MessageDelivery{
					{Project: "ntm", Payload: &Message{ID: 100, Subject: args["subject"].(string)}},
				},
				Count: 1,
			}, nil
		},
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	convertImages := true
	result, err := c.SendMessage(context.Background(), SendMessageOptions{
		ProjectKey:    "/test",
		SenderName:    "TestAgent",
		To:            []string{"BlueLake"},
		Subject:       "Hello",
		BodyMD:        "Test body",
		CC:            []string{"RedStone"},
		BCC:           []string{"GreenCastle"},
		Importance:    "high",
		AckRequired:   true,
		ThreadID:      "thread-1",
		ConvertImages: &convertImages,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Count != 1 {
		t.Errorf("count = %d, want 1", result.Count)
	}
	if result.Deliveries[0].Payload.Subject != "Hello" {
		t.Errorf("subject = %q, want Hello", result.Deliveries[0].Payload.Subject)
	}
}

func TestReplyMessage(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(mockMCPHandler(t, map[string]func(args map[string]interface{}) (interface{}, *JSONRPCError){
		"reply_message": func(args map[string]interface{}) (interface{}, *JSONRPCError) {
			return Message{
				ID:      200,
				Subject: "Re: Hello",
				BodyMD:  args["body_md"].(string),
			}, nil
		},
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	msg, err := c.ReplyMessage(context.Background(), ReplyMessageOptions{
		ProjectKey:    "/test",
		MessageID:     100,
		SenderName:    "TestAgent",
		BodyMD:        "Reply body",
		To:            []string{"BlueLake"},
		CC:            []string{"RedStone"},
		BCC:           []string{"GreenCastle"},
		SubjectPrefix: "RE:",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.ID != 200 {
		t.Errorf("msg.ID = %d, want 200", msg.ID)
	}
	if msg.BodyMD != "Reply body" {
		t.Errorf("body = %q, want Reply body", msg.BodyMD)
	}
}

func TestMarkMessageRead(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(mockMCPHandler(t, map[string]func(args map[string]interface{}) (interface{}, *JSONRPCError){
		"mark_message_read": func(args map[string]interface{}) (interface{}, *JSONRPCError) {
			return map[string]interface{}{
				"message_id": args["message_id"],
				"read":       true,
				"read_at":    "2026-01-02T03:04:05Z",
			}, nil
		},
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	result, err := c.MarkMessageRead(context.Background(), "/test", "BlueLake", 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if !result.Read {
		t.Fatal("expected read=true")
	}
	if result.MessageID != 42 {
		t.Fatalf("message_id = %d, want 42", result.MessageID)
	}
	if result.ReadAt == nil || result.ReadAt.Time.IsZero() {
		t.Fatal("expected read_at timestamp")
	}
}

func TestAcknowledgeMessage(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(mockMCPHandler(t, map[string]func(args map[string]interface{}) (interface{}, *JSONRPCError){
		"acknowledge_message": func(args map[string]interface{}) (interface{}, *JSONRPCError) {
			return map[string]interface{}{
				"message_id":      args["message_id"],
				"acknowledged":    true,
				"acknowledged_at": "2026-01-02T03:04:05Z",
				"read_at":         "2026-01-02T03:00:00Z",
			}, nil
		},
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	result, err := c.AcknowledgeMessage(context.Background(), "/test", "BlueLake", 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if !result.Acknowledged {
		t.Fatal("expected acknowledged=true")
	}
	if result.MessageID != 42 {
		t.Fatalf("message_id = %d, want 42", result.MessageID)
	}
	if result.AcknowledgedAt == nil || result.AcknowledgedAt.Time.IsZero() {
		t.Fatal("expected acknowledged_at timestamp")
	}
}

func TestRequestContact(t *testing.T) {
	t.Parallel()

	expiresAt := "2026-03-22T12:34:56Z"

	server := httptest.NewServer(mockMCPHandler(t, map[string]func(args map[string]interface{}) (interface{}, *JSONRPCError){
		"request_contact": func(args map[string]interface{}) (interface{}, *JSONRPCError) {
			// Both servers reply with the link's fields at the top level.
			return map[string]interface{}{
				"from":         "TestAgent",
				"from_project": "/test",
				"to":           "BlueLake",
				"to_project":   "/other",
				"status":       "pending",
				"expires_ts":   expiresAt,
			}, nil
		},
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	result, err := c.RequestContact(context.Background(), RequestContactOptions{
		ProjectKey: "/test",
		FromAgent:  "TestAgent",
		ToAgent:    "BlueLake",
		ToProject:  "/other",
		Reason:     "coordination",
		TTLSeconds: 3600,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != "pending" {
		t.Errorf("status = %q, want pending", result.Status)
	}
	if result.From != "TestAgent" || result.To != "BlueLake" || result.ToProject != "/other" {
		t.Fatalf("link fields = %+v", *result)
	}
	if result.ExpiresTS == nil || *result.ExpiresTS != expiresAt {
		t.Fatalf("expires_ts = %#v, want %q", result.ExpiresTS, expiresAt)
	}
}

func TestRespondContact(t *testing.T) {
	t.Parallel()

	expiresAt := "2026-03-23T00:00:00Z"

	server := httptest.NewServer(mockMCPHandler(t, map[string]func(args map[string]interface{}) (interface{}, *JSONRPCError){
		"respond_contact": func(args map[string]interface{}) (interface{}, *JSONRPCError) {
			// Both servers reply {from, to, approved, expires_ts, updated}.
			return map[string]interface{}{
				"from":       "TestAgent",
				"to":         "BlueLake",
				"approved":   true,
				"expires_ts": expiresAt,
				"updated":    1,
			}, nil
		},
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	result, err := c.RespondContact(context.Background(), RespondContactOptions{
		ProjectKey: "/test",
		ToAgent:    "BlueLake",
		FromAgent:  "TestAgent",
		Accept:     true,
		TTLSeconds: 7200,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Approved || result.Updated != 1 || result.From != "TestAgent" || result.To != "BlueLake" {
		t.Fatalf("result = %+v, want TestAgent->BlueLake approved with 1 link updated", *result)
	}
	if result.ExpiresTS == nil || *result.ExpiresTS != expiresAt {
		t.Fatalf("expires_ts = %#v, want %q", result.ExpiresTS, expiresAt)
	}
}

// TestListContacts replays list_contacts replies both servers sent live. The
// counterparty is always in "to". The Rust server (am 0.3.36) returns the list
// as the result text; the Python server (mcp_agent_mail 0.3.4) also returns it
// as structured content wrapped in {"result": [...]}, the form the client
// reads first.
func TestListContacts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		body     string
		wantTo   []string
		wantLast ContactLink
	}{
		{
			name:   "rust",
			body:   mcpTextReply(`[{"to":"OliveSpire","status":"approved","reason":"auto-handshake by send_message","updated_ts":"2026-10-20T02:24:41.114375Z","expires_ts":"2026-10-21T02:24:41.114375Z"},{"to":"CrimsonDune","status":"approved","reason":"auto-handshake by send_message","updated_ts":"2026-10-20T02:32:19.964488Z","expires_ts":"2026-10-21T02:32:19.964488Z"}]`),
			wantTo: []string{"OliveSpire", "CrimsonDune"},
			wantLast: ContactLink{To: "CrimsonDune", Status: "approved", Reason: "auto-handshake by send_message",
				ExpiresTS: &FlexTime{Time: time.Date(2026, 10, 21, 2, 32, 19, 964488000, time.UTC)}},
		},
		{
			name:   "rust, none",
			body:   mcpTextReply(`[]`),
			wantTo: []string{},
		},
		{
			name:   "python",
			body:   `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"[{\"to\":\"GreenCastle\",\"to_project\":\"/p\",\"status\":\"approved\",\"is_expired\":false,\"allows_messaging\":true,\"reason\":\"seed\",\"updated_ts\":\"2026-10-20T05:07:25.637121+00:00\",\"expires_ts\":\"2026-11-19T05:07:25.637121+00:00\"}]"}],"structuredContent":{"result":[{"to":"GreenCastle","to_project":"/p","status":"approved","is_expired":false,"allows_messaging":true,"reason":"seed","updated_ts":"2026-10-20T05:07:25.637121+00:00","expires_ts":"2026-11-19T05:07:25.637121+00:00"}]},"isError":false}}`,
			wantTo: []string{"GreenCastle"},
			wantLast: ContactLink{To: "GreenCastle", ToProject: "/p", Status: "approved", Reason: "seed",
				ExpiresTS: &FlexTime{Time: time.Date(2026, 11, 19, 5, 7, 25, 637121000, time.UTC)}},
		},
		{
			name:   "python, none",
			body:   `{"jsonrpc":"2.0","id":1,"result":{"content":[],"structuredContent":{"result":[]},"isError":false}}`,
			wantTo: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			c := NewClient(WithBaseURL(server.URL + "/"))
			contacts, err := c.ListContacts(context.Background(), "/p", "BlueLake")
			if err != nil {
				t.Fatalf("ListContacts: %v", err)
			}
			gotTo := []string{}
			for _, contact := range contacts {
				gotTo = append(gotTo, contact.To)
			}
			if strings.Join(gotTo, ",") != strings.Join(tt.wantTo, ",") {
				t.Fatalf("contacts to %v, want %v", gotTo, tt.wantTo)
			}
			if len(contacts) == 0 {
				return
			}
			last := contacts[len(contacts)-1]
			if last.To != tt.wantLast.To || last.ToProject != tt.wantLast.ToProject || last.Status != tt.wantLast.Status ||
				last.Reason != tt.wantLast.Reason || last.ExpiresTS == nil || !last.ExpiresTS.Equal(tt.wantLast.ExpiresTS.Time) {
				t.Errorf("last contact = %+v, want %+v", last, tt.wantLast)
			}
		})
	}
}

// mcpTextReply wraps a tool's result text in the JSON-RPC envelope the Rust
// server sends: one text content block and no structured content.
func mcpTextReply(text string) string {
	body, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"result":  map[string]interface{}{"content": []map[string]string{{"type": "text", "text": text}}},
	})
	return string(body)
}

// capturedRustSearchMessagesReply is the tool text of a real search_messages
// reply from the Rust Agent Mail server (am 0.3.36). ack_required arrives as
// the 0/1 integer stored in SQLite, not a JSON boolean; the Python server's
// FTS query returns the same integer.
const capturedRustSearchMessagesReply = `{"result":[{"id":54667,"subject":"[bd-0clbc] fixture B: normal, stays unread","importance":"normal","ack_required":0,"created_ts":"2026-10-20T02:21:16.648945Z","thread_id":"bd-0clbc","from":"SapphireSquirrel","to":["SapphireSquirrel"],"cc":[]},{"id":54666,"subject":"[bd-0clbc] fixture A: urgent, will be marked read","importance":"urgent","ack_required":1,"created_ts":"2026-10-20T02:21:15.751360Z","thread_id":"bd-0clbc","from":"SapphireSquirrel","to":["SapphireSquirrel"],"cc":[]}],"next_cursor":"s402d076a40000000:i54666"}`

func TestSearchMessages(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(mockMCPHandler(t, map[string]func(args map[string]interface{}) (interface{}, *JSONRPCError){
		"search_messages": func(args map[string]interface{}) (interface{}, *JSONRPCError) {
			return json.RawMessage(capturedRustSearchMessagesReply), nil
		},
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	results, err := c.SearchMessages(context.Background(), SearchOptions{
		ProjectKey: "/test",
		Query:      "fixture",
		Limit:      10,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	if results[0].ID != 54667 || results[0].AckRequired {
		t.Errorf("results[0] = %+v, want id 54667 with ack_required false", results[0])
	}
	if results[1].ID != 54666 || !results[1].AckRequired || results[1].Importance != "urgent" {
		t.Errorf("results[1] = %+v, want urgent id 54666 with ack_required true", results[1])
	}
	if results[1].ThreadID == nil || *results[1].ThreadID != "bd-0clbc" || results[1].From != "SapphireSquirrel" {
		t.Errorf("results[1] thread/from = %v/%q", results[1].ThreadID, results[1].From)
	}
}

func TestSummarizeThread(t *testing.T) {
	t.Parallel()

	var receivedArgs map[string]interface{}
	server := httptest.NewServer(mockMCPHandler(t, map[string]func(args map[string]interface{}) (interface{}, *JSONRPCError){
		"summarize_thread": func(args map[string]interface{}) (interface{}, *JSONRPCError) {
			receivedArgs = args
			return ThreadSummaryResponse{
				ThreadID: args["thread_id"].(string),
				Summary: ThreadSummary{
					Participants: []string{"BlueLake", "RedStone"},
					KeyPoints:    []string{"Discussed build plan"},
					ActionItems:  []string{"Write tests"},
				},
				Examples: []InboxMessage{
					{ID: 1, Subject: "Build plan"},
				},
			}, nil
		},
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	trueVal := true
	result, err := c.SummarizeThread(context.Background(), SummarizeThreadOptions{
		ProjectKey:      "/test",
		ThreadID:        "TKT-123",
		IncludeExamples: &trueVal,
		LLMMode:         &trueVal,
		LLMModel:        "opus-4.5",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ThreadID != "TKT-123" {
		t.Errorf("thread_id = %q, want TKT-123", result.ThreadID)
	}
	if len(result.Summary.Participants) != 2 {
		t.Errorf("len(participants) = %d, want 2", len(result.Summary.Participants))
	}
	if len(result.Examples) != 1 {
		t.Fatalf("len(examples) = %d, want 1", len(result.Examples))
	}
	if got, ok := receivedArgs["include_examples"]; !ok {
		t.Fatal("expected include_examples argument to be forwarded")
	} else if got != true {
		t.Fatalf("include_examples = %#v, want true", got)
	}
}

func TestSummarizeThread_ForwardsExplicitFalseLLMMode(t *testing.T) {
	t.Parallel()

	var receivedArgs map[string]interface{}
	server := httptest.NewServer(mockMCPHandler(t, map[string]func(args map[string]interface{}) (interface{}, *JSONRPCError){
		"summarize_thread": func(args map[string]interface{}) (interface{}, *JSONRPCError) {
			receivedArgs = args
			return ThreadSummaryResponse{
				ThreadID: args["thread_id"].(string),
				Summary: ThreadSummary{
					Participants: []string{"BlueLake"},
				},
			}, nil
		},
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	falseVal := false
	_, err := c.SummarizeThread(context.Background(), SummarizeThreadOptions{
		ProjectKey:      "/test",
		ThreadID:        "TKT-456",
		IncludeExamples: &falseVal,
		LLMMode:         &falseVal,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, ok := receivedArgs["llm_mode"]; !ok {
		t.Fatal("expected llm_mode argument to be forwarded")
	} else if got != false {
		t.Fatalf("llm_mode = %#v, want false", got)
	}
}

func TestSummarizeThread_OmitsIncludeExamplesWhenUnset(t *testing.T) {
	t.Parallel()

	var receivedArgs map[string]interface{}
	server := httptest.NewServer(mockMCPHandler(t, map[string]func(args map[string]interface{}) (interface{}, *JSONRPCError){
		"summarize_thread": func(args map[string]interface{}) (interface{}, *JSONRPCError) {
			receivedArgs = args
			return ThreadSummaryResponse{
				ThreadID: args["thread_id"].(string),
				Summary: ThreadSummary{
					Participants: []string{"BlueLake"},
				},
			}, nil
		},
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	_, err := c.SummarizeThread(context.Background(), SummarizeThreadOptions{
		ProjectKey: "/test",
		ThreadID:   "TKT-789",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := receivedArgs["include_examples"]; ok {
		t.Fatal("did not expect include_examples argument when unset")
	}
}

func TestReservePaths(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(mockMCPHandler(t, map[string]func(args map[string]interface{}) (interface{}, *JSONRPCError){
		"file_reservation_paths": func(args map[string]interface{}) (interface{}, *JSONRPCError) {
			return ReservationResult{
				Granted: []FileReservation{
					{ID: 1, PathPattern: "internal/agentmail/*", AgentName: "TestAgent", Exclusive: true},
				},
				Conflicts: nil,
			}, nil
		},
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	result, err := c.ReservePaths(context.Background(), FileReservationOptions{
		ProjectKey: "/test",
		AgentName:  "TestAgent",
		Paths:      []string{"internal/agentmail/*"},
		TTLSeconds: 3600,
		Exclusive:  true,
		Reason:     "testing",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Granted) != 1 {
		t.Fatalf("len(granted) = %d, want 1", len(result.Granted))
	}
	if result.Granted[0].PathPattern != "internal/agentmail/*" {
		t.Errorf("path = %q, want internal/agentmail/*", result.Granted[0].PathPattern)
	}
}

func TestReservePaths_Conflict(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(mockMCPHandler(t, map[string]func(args map[string]interface{}) (interface{}, *JSONRPCError){
		"file_reservation_paths": func(args map[string]interface{}) (interface{}, *JSONRPCError) {
			return ReservationResult{
				Granted: nil,
				Conflicts: []ReservationConflict{
					{Path: "internal/agentmail/*", Holders: []string{"BlueLake"}},
				},
			}, nil
		},
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	result, err := c.ReservePaths(context.Background(), FileReservationOptions{
		ProjectKey: "/test",
		AgentName:  "TestAgent",
		Paths:      []string{"internal/agentmail/*"},
	})
	if err == nil {
		t.Fatal("expected conflict error, got nil")
	}
	if !IsReservationConflict(err) {
		t.Errorf("expected reservation conflict error, got: %v", err)
	}
	if result == nil {
		t.Fatal("expected result even with conflict")
	}
	if len(result.Conflicts) != 1 {
		t.Errorf("len(conflicts) = %d, want 1", len(result.Conflicts))
	}
}

func TestReleaseReservations(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(mockMCPHandler(t, map[string]func(args map[string]interface{}) (interface{}, *JSONRPCError){
		"release_file_reservations": func(args map[string]interface{}) (interface{}, *JSONRPCError) {
			return map[string]interface{}{"released": 2}, nil
		},
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	result, err := c.ReleaseReservations(context.Background(), "/test", "TestAgent", []string{"a/*"}, []int{1, 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Released != 2 {
		t.Fatalf("released = %d, want 2", result.Released)
	}
}

func TestRenewReservations(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(mockMCPHandler(t, map[string]func(args map[string]interface{}) (interface{}, *JSONRPCError){
		"renew_file_reservations": func(args map[string]interface{}) (interface{}, *JSONRPCError) {
			return RenewReservationsResult{
				Renewed: 1,
				Reservations: []RenewedReservation{
					{ID: 1, PathPattern: "a/*"},
				},
			}, nil
		},
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	result, err := c.RenewReservations(context.Background(), RenewReservationsOptions{
		ProjectKey:     "/test",
		AgentName:      "TestAgent",
		ExtendSeconds:  1800,
		ReservationIDs: []int{1},
		Paths:          []string{"a/*"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Renewed != 1 {
		t.Errorf("renewed = %d, want 1", result.Renewed)
	}
	if len(result.Reservations) != 1 {
		t.Fatalf("len(reservations) = %d, want 1", len(result.Reservations))
	}
	if result.Reservations[0].PathPattern != "a/*" {
		t.Errorf("path_pattern = %q, want %q", result.Reservations[0].PathPattern, "a/*")
	}
}

func TestStartSession(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(mockMCPHandler(t, map[string]func(args map[string]interface{}) (interface{}, *JSONRPCError){
		"macro_start_session": func(args map[string]interface{}) (interface{}, *JSONRPCError) {
			return SessionStartResult{
				Project: &Project{ID: 1, Slug: "ntm"},
				Agent:   &Agent{ID: 5, Name: "BlueLake"},
				Inbox:   []InboxMessage{},
			}, nil
		},
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	result, err := c.StartSession(context.Background(), "/test", "claude-code", "opus-4.5", "testing")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Project.Slug != "ntm" {
		t.Errorf("project slug = %q, want ntm", result.Project.Slug)
	}
	if result.Agent.Name != "BlueLake" {
		t.Errorf("agent name = %q, want BlueLake", result.Agent.Name)
	}
}

// capturedRustPrepareThreadReply is a real macro_prepare_thread reply from the
// Rust Agent Mail server (am 0.3.36, llm_mode=false), with the two key_points
// shortened. The summary and examples sit under "thread", not at the top level.
const capturedRustPrepareThreadReply = `{"project":{"id":65,"slug":"volumes-1tb-projects-ntm","human_key":"/Volumes/1tb/Projects/ntm","created_at":"2026-10-19T19:40:42.553833Z"},"agent":{"id":2109,"name":"SapphireSquirrel","program":"claude-code","model":"claude-opus-5-5","task_description":"bd-0clbc: decode Agent Mail read_ts (not read_at) in ntm agentmail types","inception_ts":"2026-10-20T02:16:43.412357Z","last_active_ts":"2026-10-20T02:16:43.412357Z","project_id":65,"attachments_policy":"auto","reaper_exempt":false,"capabilities":["send_message","fetch_inbox","file_reservation_paths","acknowledge_message"]},"thread":{"thread_id":"bd-0clbc","summary":{"participants":["SapphireSquirrel"],"key_points":["**server_test.go, force-release mocks**","**mail.go contact handlers**"],"action_items":[],"total_messages":4,"open_actions":0,"done_actions":0,"mentions":[]},"examples":[{"id":54667,"from":"SapphireSquirrel","subject":"[bd-0clbc] fixture B: normal, stays unread","created_ts":"2026-10-20T02:21:16.648945Z"},{"id":54677,"from":"SapphireSquirrel","subject":"[bd-0clbc] Start: decode Agent Mail read_ts","created_ts":"2026-10-20T02:24:42.068766Z"},{"id":54688,"from":"SapphireSquirrel","subject":"[bd-0clbc] need small hunks in internal/serve/mail.go + server_test.go","created_ts":"2026-10-20T02:32:20.655288Z"}],"total_messages":4},"inbox":[{"id":54667,"project_id":65,"sender_id":2109,"thread_id":"bd-0clbc","topic":null,"subject":"[bd-0clbc] fixture B: normal, stays unread","importance":"normal","ack_required":false,"from":"SapphireSquirrel","created_ts":"2026-10-20T02:21:16.648945Z","read_ts":"2026-10-20T02:33:43.631560Z","kind":"to","attachments":[]}]}`

func TestPrepareThread(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(mockMCPHandler(t, map[string]func(args map[string]interface{}) (interface{}, *JSONRPCError){
		"macro_prepare_thread": func(args map[string]interface{}) (interface{}, *JSONRPCError) {
			return json.RawMessage(capturedRustPrepareThreadReply), nil
		},
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	trueVal := true
	falseVal := false
	result, err := c.PrepareThread(context.Background(), PrepareThreadOptions{
		ProjectKey:         "/test",
		ThreadID:           "TKT-99",
		Program:            "claude-code",
		Model:              "opus-4.5",
		AgentName:          "BlueLake",
		TaskDescription:    "testing",
		LLMModel:           "opus-4.5",
		InboxLimit:         5,
		IncludeExamples:    &trueVal,
		IncludeInboxBodies: &falseVal,
		LLMMode:            &trueVal,
		RegisterIfMissing:  &trueVal,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Thread.ThreadID != "bd-0clbc" || result.Thread.TotalMessages != 4 {
		t.Errorf("thread = %q with %d messages, want bd-0clbc with 4", result.Thread.ThreadID, result.Thread.TotalMessages)
	}
	if got := result.Thread.Summary.KeyPoints; len(got) != 2 || got[1] != "**mail.go contact handlers**" {
		t.Errorf("summary key_points = %q", got)
	}
	if len(result.Thread.Examples) != 3 || result.Thread.Examples[2].ID != 54688 {
		t.Errorf("examples = %+v, want 3 ending with id 54688", result.Thread.Examples)
	}
	if result.Agent == nil || result.Agent.Name != "SapphireSquirrel" || result.Project == nil || result.Project.Slug != "volumes-1tb-projects-ntm" {
		t.Errorf("agent/project = %+v / %+v", result.Agent, result.Project)
	}
	if len(result.Inbox) != 1 || result.Inbox[0].ReadAt == nil {
		t.Errorf("inbox = %+v, want one row with read_ts decoded", result.Inbox)
	}
}

func TestContactHandshake(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(mockMCPHandler(t, map[string]func(args map[string]interface{}) (interface{}, *JSONRPCError){
		"macro_contact_handshake": func(args map[string]interface{}) (interface{}, *JSONRPCError) {
			// Both servers nest the request_contact, respond_contact and
			// send_message replies (Rust macros.rs HandshakeResponse,
			// Python app.py macro_contact_handshake).
			return json.RawMessage(`{
				"request":{"from":"TestAgent","from_project":"/test","to":"BlueLake","to_project":"/other","status":"pending","expires_ts":"2026-10-20T04:30:00Z"},
				"response":{"from":"TestAgent","to":"BlueLake","approved":true,"expires_ts":"2026-10-20T04:30:00Z","updated":1},
				"welcome_message":{"deliveries":[{"project":"/test","payload":{"id":321,"project_id":1,"sender_id":2,"thread_id":null,"topic":null,"subject":"Hello!","body_md":"Hi there","importance":"normal","ack_required":false,"created_ts":"2026-10-20T02:30:00.000000Z","attachments":[],"from":"TestAgent","to":["BlueLake"],"cc":[],"bcc":[]}}],"count":1,"attachments":[],"verified_sender":false}
			}`), nil
		},
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	result, err := c.ContactHandshake(context.Background(), ContactHandshakeOptions{
		ProjectKey:      "/test",
		AgentName:       "TestAgent",
		ToAgent:         "BlueLake",
		ToProject:       "/other",
		Reason:          "coordination",
		Program:         "claude-code",
		Model:           "opus-4.5",
		TaskDescription: "testing",
		AutoAccept:      true,
		WelcomeSubject:  "Hello!",
		WelcomeBody:     "Hi there",
		TTLSeconds:      7200,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Request.Status != "pending" || result.Request.ExpiresTS == nil {
		t.Errorf("request = %+v, want pending with expires_ts", result.Request)
	}
	if result.Response == nil || !result.Response.Approved || result.Response.ExpiresTS == nil {
		t.Errorf("response = %+v, want the approving respond_contact reply", result.Response)
	}
	if result.WelcomeMessage == nil || len(result.WelcomeMessage.Deliveries) != 1 ||
		result.WelcomeMessage.Deliveries[0].Payload == nil || result.WelcomeMessage.Deliveries[0].Payload.Subject != "Hello!" {
		t.Errorf("welcome_message = %+v, want one delivery with subject Hello!", result.WelcomeMessage)
	}
}

func TestSendOverseerMessage(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/overseer/send") {
			// Handle MCP requests normally (not used here)
			w.WriteHeader(http.StatusNotFound)
			return
		}

		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(OverseerSendResult{
			Success:    true,
			MessageID:  300,
			Recipients: []string{"BlueLake"},
		})
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/mcp/"))
	result, err := c.SendOverseerMessage(context.Background(), OverseerMessageOptions{
		ProjectSlug: "ntm",
		Recipients:  []string{"BlueLake"},
		Subject:     "Urgent task",
		BodyMD:      "Please do this",
		ThreadID:    "thread-1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Error("expected success=true")
	}
	if result.MessageID != 300 {
		t.Errorf("messageID = %d, want 300", result.MessageID)
	}
}

func TestSendOverseerMessage_Unauthorized(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/mcp/"))
	_, err := c.SendOverseerMessage(context.Background(), OverseerMessageOptions{
		ProjectSlug: "ntm",
		Recipients:  []string{"BlueLake"},
		Subject:     "test",
		BodyMD:      "test",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !IsUnauthorized(err) {
		t.Errorf("expected unauthorized error, got: %v", err)
	}
}

func TestSendOverseerMessage_BadRequest(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"detail": "invalid recipients"})
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/mcp/"))
	_, err := c.SendOverseerMessage(context.Background(), OverseerMessageOptions{
		ProjectSlug: "ntm",
		Recipients:  []string{},
		Subject:     "test",
		BodyMD:      "test",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid recipients") {
		t.Errorf("expected 'invalid recipients' in error, got: %v", err)
	}
}

func TestSendOverseerMessage_BadRequestNoDetail(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("not json"))
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/mcp/"))
	_, err := c.SendOverseerMessage(context.Background(), OverseerMessageOptions{
		ProjectSlug: "ntm",
		Recipients:  []string{"x"},
		Subject:     "test",
		BodyMD:      "test",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "bad request") {
		t.Errorf("expected 'bad request' in error, got: %v", err)
	}
}

func TestSendOverseerMessage_ServerError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/mcp/"))
	_, err := c.SendOverseerMessage(context.Background(), OverseerMessageOptions{
		ProjectSlug: "ntm",
		Recipients:  []string{"x"},
		Subject:     "test",
		BodyMD:      "test",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestSendOverseerMessage_Timeout(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	c := NewClient(WithBaseURL(server.URL + "/mcp/"))
	_, err := c.SendOverseerMessage(ctx, OverseerMessageOptions{
		ProjectSlug: "ntm",
		Recipients:  []string{"x"},
		Subject:     "test",
		BodyMD:      "test",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestInstallPrecommitGuard(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(mockMCPHandler(t, map[string]func(args map[string]interface{}) (interface{}, *JSONRPCError){
		"install_precommit_guard": func(args map[string]interface{}) (interface{}, *JSONRPCError) {
			return map[string]interface{}{"installed": true}, nil
		},
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	err := c.InstallPrecommitGuard(context.Background(), "/test", "/data/projects/ntm")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestUninstallPrecommitGuard(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(mockMCPHandler(t, map[string]func(args map[string]interface{}) (interface{}, *JSONRPCError){
		"uninstall_precommit_guard": func(args map[string]interface{}) (interface{}, *JSONRPCError) {
			return map[string]interface{}{"uninstalled": true}, nil
		},
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	err := c.UninstallPrecommitGuard(context.Background(), "/data/projects/ntm")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// capturedRustMessageResourceResult is the JSON-RPC result of a real
// resources/read of resource://message/54719 against the Rust Agent Mail
// server, am 0.3.36: a self-addressed message.
const capturedRustMessageResourceResult = `{"contents":[{"uri":"resource://message/54719?project=/Volumes/1tb/Projects/ntm","text":"{\"id\":54719,\"project_id\":65,\"sender_id\":2121,\"thread_id\":\"bd-xqu7h\",\"topic\":null,\"subject\":\"[bd-xqu7h] fixture: read through resource://message\",\"body_md\":\"Self-addressed fixture for bd-xqu7h. Body line with **markdown**.\",\"importance\":\"high\",\"ack_required\":true,\"created_ts\":\"2026-10-20T03:04:19.300659Z\",\"attachments\":[],\"from\":\"VioletLantern\",\"to\":[\"VioletLantern\"],\"cc\":[],\"bcc\":[]}","mimeType":"text/plain"}]}`

// pythonMessageResourceResult is the same message as the Python server's
// message_resource builds it (app.py _message_to_dict plus
// _apply_sender_identity): reply_to and an offset timestamp, and no to, cc or
// bcc.
const pythonMessageResourceResult = `{"contents":[{"uri":"resource://message/54719?project=/Volumes/1tb/Projects/ntm","mimeType":"application/json","text":"{\"id\":54719,\"project_id\":65,\"sender_id\":2121,\"thread_id\":\"bd-xqu7h\",\"reply_to\":null,\"topic\":null,\"subject\":\"[bd-xqu7h] fixture: read through resource://message\",\"importance\":\"high\",\"ack_required\":true,\"created_ts\":\"2026-10-20T03:04:19.300659+00:00\",\"attachments\":[],\"body_md\":\"Self-addressed fixture for bd-xqu7h. Body line with **markdown**.\",\"from\":\"VioletLantern\"}"}]}`

// messageResourceServer answers resources/read with result and every other
// method with "Method not found", as the servers answer a get_message call.
// It records each resource URI it was asked for.
func messageResourceServer(t *testing.T, result string, uris *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req JSONRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		resp := JSONRPCResponse{JSONRPC: "2.0", ID: req.ID}
		if req.Method == "resources/read" {
			params, _ := req.Params.(map[string]interface{})
			uri, _ := params["uri"].(string)
			*uris = append(*uris, uri)
			resp.Result = json.RawMessage(result)
		} else {
			resp.Error = &JSONRPCError{Code: -32601, Message: "Method not found"}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

// TestGetMessage_ReadsMessageResource replays both servers' message resource
// through GetMessage. Neither server has a get_message tool, so the read must
// go through resource://message/{id}.
func TestGetMessage_ReadsMessageResource(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 10, 20, 3, 4, 19, 300659000, time.UTC)
	tests := []struct {
		name   string
		result string
		wantTo []string
	}{
		{name: "rust", result: capturedRustMessageResourceResult, wantTo: []string{"VioletLantern"}},
		{name: "python", result: pythonMessageResourceResult},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var uris []string
			server := messageResourceServer(t, tt.result, &uris)
			defer server.Close()

			c := NewClient(WithBaseURL(server.URL + "/"))
			msg, err := c.GetMessage(context.Background(), "/Volumes/1tb/Projects/ntm", "", 54719)
			if err != nil {
				t.Fatalf("GetMessage: %v", err)
			}
			if msg.ID != 54719 || msg.ProjectID != 65 || msg.SenderID != 2121 || msg.From != "VioletLantern" ||
				msg.Subject != "[bd-xqu7h] fixture: read through resource://message" ||
				msg.BodyMD != "Self-addressed fixture for bd-xqu7h. Body line with **markdown**." ||
				msg.Importance != "high" || !msg.AckRequired ||
				msg.ThreadID == nil || *msg.ThreadID != "bd-xqu7h" || !msg.CreatedTS.Time.Equal(created) {
				t.Errorf("message decoded as %+v", *msg)
			}
			if strings.Join(msg.To, ",") != strings.Join(tt.wantTo, ",") {
				t.Errorf("To = %v, want %v", msg.To, tt.wantTo)
			}

			if len(uris) != 1 {
				t.Fatalf("resource reads = %v, want one", uris)
			}
			u, err := url.Parse(uris[0])
			if err != nil {
				t.Fatalf("parse resource URI %q: %v", uris[0], err)
			}
			if u.Scheme != "resource" || u.Host != "message" || u.Path != "/54719" {
				t.Errorf("resource URI = %q, want resource://message/54719", uris[0])
			}
			q := u.Query()
			if q.Get("project") != "/Volumes/1tb/Projects/ntm" || q.Get("format") != "json" {
				t.Errorf("resource query = %v, want project and format=json", q)
			}
			if q.Has("agent") || q.Has("agent_token") {
				t.Errorf("resource query = %v, want no reader without an agent name", q)
			}
		})
	}
}

// TestGetMessage_SendsReaderAndCachedToken checks the reader the Python server
// authenticates: the agent name, plus its registration token once cached.
func TestGetMessage_SendsReaderAndCachedToken(t *testing.T) {
	t.Parallel()

	var uris []string
	server := messageResourceServer(t, capturedRustMessageResourceResult, &uris)
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	if _, err := c.GetMessage(context.Background(), "/proj", "BlueLake", 54719); err != nil {
		t.Fatalf("GetMessage without token: %v", err)
	}
	c.SetRegistrationToken("/proj", "BlueLake", "tok-123")
	if _, err := c.GetMessage(context.Background(), "/proj", "BlueLake", 54719); err != nil {
		t.Fatalf("GetMessage with token: %v", err)
	}

	if len(uris) != 2 {
		t.Fatalf("resource reads = %v, want two", uris)
	}
	for i, wantToken := range []string{"", "tok-123"} {
		u, err := url.Parse(uris[i])
		if err != nil {
			t.Fatalf("parse resource URI %q: %v", uris[i], err)
		}
		q := u.Query()
		if q.Get("project") != "/proj" || q.Get("agent") != "BlueLake" || q.Get("agent_token") != wantToken {
			t.Errorf("read %d query = %v, want project /proj, agent BlueLake, agent_token %q", i, q, wantToken)
		}
	}
}

// TestGetMessage_NotFound replays the Rust server's answer for a missing id
// (am 0.3.36, error data trimmed to its type).
func TestGetMessage_NotFound(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req JSONRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32000,"message":"Message not found: 999999999","data":{"error":{"type":"NOT_FOUND","message":"Message not found: 999999999","recoverable":true}}},"id":1}`))
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	_, err := c.GetMessage(context.Background(), "/Volumes/1tb/Projects/ntm", "", 999999999)
	if !errors.Is(err, ErrMessageNotFound) {
		t.Fatalf("GetMessage error = %v, want ErrMessageNotFound", err)
	}
}

// TestServerRefusalsClassify replays refusals both servers sent live and checks
// each comes back as the sentinel error serve maps to an HTTP status. The Rust
// server (am 0.3.36) sends a tool error's type and message as JSON in the
// result text and a resource error's type in the JSON-RPC error data. The
// Python server (mcp_agent_mail 0.3.4) sends only text: "Error calling tool
// 'X': <message>" for a tool, JSON-RPC code 0 for a resource. Large data
// fields are trimmed, and the Python project path is shortened to /p.
func TestServerRefusalsClassify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		resource bool // resources/read rather than tools/call
		body     string
		want     error
		wantText string
	}{
		{
			name:     "rust force-release of an active reservation",
			body:     `{"jsonrpc":"2.0","result":{"content":[{"type":"text","text":"{\"error\":{\"type\":\"RESERVATION_ACTIVE\",\"message\":\"Reservation still shows recent activity; refusing forced release.\",\"recoverable\":true,\"data\":{\"file_reservation_id\":14598,\"stale_reasons\":[\"agent_recently_active\",\"no_recent_mail_activity>900s\",\"path_pattern_unmatched\"]}}}"}],"isError":true},"id":7}`,
			want:     ErrReservationConflict,
			wantText: "RESERVATION_ACTIVE: Reservation still shows recent activity; refusing forced release.",
		},
		{
			name:     "python force-release of an active reservation",
			body:     `{"jsonrpc":"2.0","id":38,"result":{"content":[{"type":"text","text":"Error calling tool 'force_release_file_reservation': Reservation still shows recent activity; refusing forced release."}],"isError":true}}`,
			want:     ErrReservationConflict,
			wantText: ": Reservation still shows recent activity; refusing forced release.",
		},
		{
			name:     "rust force-release of a missing id",
			body:     `{"jsonrpc":"2.0","result":{"content":[{"type":"text","text":"{\"error\":{\"type\":\"NOT_FOUND\",\"message\":\"File reservation id=999999999 not found for project '/Volumes/1tb/Projects/ntm'.\",\"recoverable\":true,\"data\":{\"file_reservation_id\":999999999}}}"}],"isError":true},"id":5}`,
			want:     ErrNotFound,
			wantText: "NOT_FOUND: File reservation id=999999999 not found",
		},
		{
			name:     "rust whois of a missing agent",
			body:     `{"jsonrpc":"2.0","result":{"content":[{"type":"text","text":"{\"error\":{\"type\":\"AGENT_NOT_FOUND\",\"message\":\"Agent 'NoSuchAgentZzq' not found in project 'volumes-1tb-projects-ntm'\",\"recoverable\":true,\"data\":{\"agent_name\":\"NoSuchAgentZzq\",\"project\":\"volumes-1tb-projects-ntm\"}}}"}],"isError":true},"id":1}`,
			want:     ErrAgentNotRegistered,
			wantText: "AGENT_NOT_FOUND: Agent 'NoSuchAgentZzq' not found",
		},
		{
			name:     "rust whois in a missing project",
			body:     `{"jsonrpc":"2.0","result":{"content":[{"type":"text","text":"{\"error\":{\"type\":\"NOT_FOUND\",\"message\":\"Project '/nonexistent/bd-m0gm2-probe' not found. Did you mean: 'volumes-1tb-projects-pi', 'volumes-1tb-projects-ntm'? Use ensure_project to create a new project, or check spelling.\",\"recoverable\":true,\"data\":{\"identifier\":\"/nonexistent/bd-m0gm2-probe\"}}}"}],"isError":true},"id":2}`,
			want:     ErrNotFound,
			wantText: "NOT_FOUND: Project '/nonexistent/bd-m0gm2-probe' not found.",
		},
		{
			name:     "rust send with a wrong sender_token",
			body:     `{"jsonrpc":"2.0","result":{"content":[{"type":"text","text":"{\"error\":{\"type\":\"SENDER_TOKEN_MISMATCH\",\"message\":\"sender_token does not match the registered token for agent 'RoseRiver'. Only the agent's owner (the session that called register_agent) can send messages as this agent. Use the registration_token returned by register_agent.\",\"recoverable\":false,\"data\":{\"sender_name\":\"RoseRiver\",\"hint\":\"Use the registration_token returned by register_agent as sender_token\"}}}"}],"isError":true},"id":9}`,
			want:     ErrUnauthorized,
			wantText: "SENDER_TOKEN_MISMATCH: sender_token does not match the registered token for agent 'RoseRiver'.",
		},
		{
			name:     "rust retire with a wrong registration_token",
			body:     `{"jsonrpc":"2.0","result":{"content":[{"type":"text","text":"{\"error\":{\"type\":\"AUTHENTICATION_REQUIRED\",\"message\":\"retire_agent was refused: the supplied registration_token does not match agent 'RoseRiver'. Present the token returned when 'RoseRiver' was registered (the ` + "`registration_token`" + ` field of the register_agent / create_agent_identity / macro_start_session response).\",\"recoverable\":true,\"data\":{\"agent_name\":\"RoseRiver\",\"token_param\":\"registration_token\",\"reason\":\"token_mismatch\"}}}"}],"isError":true},"id":10}`,
			want:     ErrUnauthorized,
			wantText: "AUTHENTICATION_REQUIRED: retire_agent was refused",
		},
		{
			name:     "python whois without a token",
			body:     `{"jsonrpc":"2.0","id":14,"result":{"content":[{"type":"text","text":"Error calling tool 'whois': whois requires registration_token for agent 'GreenCastle', unless this MCP session has already authenticated as that agent."}],"isError":true}}`,
			want:     ErrUnauthorized,
			wantText: ": whois requires registration_token for agent 'GreenCastle'",
		},
		{
			name:     "python reply without a sender_token",
			body:     `{"jsonrpc":"2.0","id":19,"result":{"content":[{"type":"text","text":"Error calling tool 'reply_message': reply_message requires sender_token for agent 'GreenCastle', unless this MCP session has already authenticated as that agent."}],"isError":true}}`,
			want:     ErrUnauthorized,
			wantText: ": reply_message requires sender_token for agent 'GreenCastle'",
		},
		{
			name:     "python search without an agent",
			body:     `{"jsonrpc":"2.0","id":20,"result":{"content":[{"type":"text","text":"Error calling tool 'search_messages': search_messages requires an authenticated agent for project '/p'. Provide agent_name plus registration_token (or agent_token for resources), or authenticate in this session first."}],"isError":true}}`,
			want:     ErrUnauthorized,
			wantText: ": search_messages requires an authenticated agent for project '/p'.",
		},
		{
			name:     "python fetch_inbox given mark_read",
			body:     `{"jsonrpc":"2.0","id":16,"result":{"content":[{"type":"text","text":"1 validation error for call[fetch_inbox]\nmark_read\n  Unexpected keyword argument [type=unexpected_keyword_argument, input_value=False, input_type=bool]\n    For further information visit https://errors.pydantic.dev/2.12/v/unexpected_keyword_argument"}],"isError":true}}`,
			want:     ErrInvalidRequest,
			wantText: "1 validation error for call[fetch_inbox]",
		},
		{
			name:     "python message resource without an agent",
			resource: true,
			body:     `{"jsonrpc":"2.0","id":8,"error":{"code":0,"message":"Error reading resource from template 'resource://message/3?project=%2Fp&format=json': resource://message/{message_id} requires an authenticated agent for project '/p'. Provide agent_name plus registration_token (or agent_token for resources), or authenticate in this session first."}}`,
			want:     ErrUnauthorized,
			wantText: "requires an authenticated agent for project '/p'.",
		},
		{
			name:     "python message resource without a token",
			resource: true,
			body:     `{"jsonrpc":"2.0","id":9,"error":{"code":0,"message":"Error reading resource from template 'resource://message/3?project=%2Fp&format=json&agent=GreenCastle': resource://message/{message_id} requires agent_token for agent 'GreenCastle', unless this MCP session has already authenticated as that agent."}}`,
			want:     ErrUnauthorized,
			wantText: "requires agent_token for agent 'GreenCastle'",
		},
		{
			name:     "python message resource with a wrong token",
			resource: true,
			body:     `{"jsonrpc":"2.0","id":13,"error":{"code":0,"message":"Error reading resource from template 'resource://message/3?project=%2Fp&format=json&agent=GreenCastle&agent_token=ILoW67Kh4O_-WDlBmI64wrplclNDoDPaza9KxjEf4eA': Invalid agent_token for agent 'GreenCastle'."}}`,
			want:     ErrUnauthorized,
			wantText: "Invalid agent_token for agent 'GreenCastle'.",
		},
		{
			name:     "python message resource for a non-participant",
			resource: true,
			body:     `{"jsonrpc":"2.0","id":12,"error":{"code":0,"message":"Error reading resource from template 'resource://message/3?project=%2Fp&format=json&agent=RedStone&agent_token=TWtYrUBgdJWO-kD0Uu0VWqQt7k0jVSyRp3tt2hxyDKU': Message '3' not found or not visible to agent 'RedStone' in project '/p'."}}`,
			want:     ErrNotFound,
			wantText: "Message '3' not found or not visible to agent 'RedStone'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			c := NewClient(WithBaseURL(server.URL + "/"))
			var err error
			if tt.resource {
				_, err = c.ReadResource(context.Background(), "resource://message/3")
			} else {
				_, err = c.callTool(context.Background(), "replayed_tool", map[string]interface{}{})
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
			if !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tt.wantText)
			}
			// The Python resource wrapping repeats the request URI, agent_token
			// included, which serve would hand to its caller.
			for _, wrapping := range []string{`{"error"`, "Error calling tool", "Error reading resource", "agent_token="} {
				if strings.Contains(err.Error(), wrapping) {
					t.Errorf("error = %q, want the server's message without its wrapping %q", err.Error(), wrapping)
				}
			}
		})
	}
}

// TestServerRefusalsLeftUnclassified pins that a refusal naming none of the
// classified conditions stays a plain tool error, so serve keeps answering 500
// for what is a server fault. The CONFLICT reply is built from the Rust
// server's identity.rs (agent name namespace exhausted), which shares its type
// with a lost force-release race but is not a reservation conflict.
func TestServerRefusalsLeftUnclassified(t *testing.T) {
	t.Parallel()

	sentinels := []error{
		ErrReservationConflict, ErrUnauthorized, ErrNotFound, ErrAgentNotRegistered, ErrMessageNotFound,
		ErrContactBlocked, ErrInvalidRequest, ErrTransientBusy, ErrTimeout, ErrNotImplemented,
	}
	for _, text := range []string{
		`{"error":{"type":"CONFLICT","message":"Could not auto-generate an unused agent name after 512 draws; this project's adjective+noun namespace is heavily used.","recoverable":true}}`,
		`{"error":{"type":"DATABASE_ERROR","message":"Database error: disk I/O error","recoverable":false}}`,
		`Agent name already in use`,
	} {
		err := classifyToolErrorText(text)
		for _, sentinel := range sentinels {
			if errors.Is(err, sentinel) {
				t.Errorf("classifyToolErrorText(%q) = %v, want no sentinel, matched %v", text, err, sentinel)
			}
		}
		if !strings.HasPrefix(err.Error(), "tool error: ") {
			t.Errorf("classifyToolErrorText(%q) = %q, want a plain tool error", text, err.Error())
		}
	}
}

// pythonToolReplay stands in for one agent-scoped tool of the Python server
// (mcp_agent_mail 0.3.4), built from its app.py and the replies it sent live.
// Like the real server it validates the arguments before the tool runs, so a
// call carrying an argument the tool does not take gets pydantic's report;
// the tool then authenticates agent_name and refuses a call without the
// agent's registration_token or with a wrong one; any other call gets reply.
// It records the arguments of every call.
type pythonToolReplay struct {
	tool   string
	params []string // the tool's parameters, from its signature in app.py
	issued string   // the registration_token the agent was issued
	reply  string   // the JSON-RPC body sent to an authenticated call

	mu    sync.Mutex
	calls []map[string]interface{}
}

func (p *pythonToolReplay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method string `json:"method"`
		Params struct {
			Name      string                 `json:"name"`
			Arguments map[string]interface{} `json:"arguments"`
		} `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Method != "tools/call" || req.Params.Name != p.tool {
		http.Error(w, "unexpected request", http.StatusBadRequest)
		return
	}
	args := req.Params.Arguments
	p.mu.Lock()
	p.calls = append(p.calls, args)
	p.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	names := make([]string, 0, len(args))
	for name := range args {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !slices.Contains(p.params, name) {
			_, _ = w.Write([]byte(mcpToolError(fmt.Sprintf("1 validation error for call[%s]\n%s\n  Unexpected keyword argument [type=unexpected_keyword_argument, input_value=%s, input_type=%s]\n    For further information visit https://errors.pydantic.dev/2.12/v/unexpected_keyword_argument",
				p.tool, name, pythonRepr(args[name]), pythonTypeName(args[name])))))
			return
		}
	}
	agent, _ := args["agent_name"].(string)
	switch presented, _ := args["registration_token"].(string); {
	case presented == "":
		_, _ = w.Write([]byte(mcpToolError(fmt.Sprintf("Error calling tool '%s': %s requires registration_token for agent '%s', unless this MCP session has already authenticated as that agent.", p.tool, p.tool, agent))))
	case presented != p.issued:
		_, _ = w.Write([]byte(mcpToolError(fmt.Sprintf("Error calling tool '%s': Invalid registration_token for agent '%s'.", p.tool, agent))))
	default:
		_, _ = w.Write([]byte(p.reply))
	}
}

// argsOfCalls returns the arguments of each call the replay got, in order.
func (p *pythonToolReplay) argsOfCalls() []map[string]interface{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]map[string]interface{}(nil), p.calls...)
}

// mcpToolError wraps text in the JSON-RPC envelope both servers send for a
// tool error.
func mcpToolError(text string) string {
	body, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"result":  map[string]interface{}{"content": []map[string]string{{"type": "text", "text": text}}, "isError": true},
	})
	return string(body)
}

// pythonRepr and pythonTypeName render a JSON argument the way pydantic's
// report names it.
func pythonRepr(value interface{}) string {
	switch v := value.(type) {
	case bool:
		if v {
			return "True"
		}
		return "False"
	case string:
		return "'" + v + "'"
	default:
		return fmt.Sprint(v)
	}
}

func pythonTypeName(value interface{}) string {
	switch value.(type) {
	case bool:
		return "bool"
	case string:
		return "str"
	case float64:
		return "int"
	default:
		return fmt.Sprintf("%T", value)
	}
}

// Replies the Python server sent live to calls carrying a valid
// registration_token; the project path is shortened to /p.
const (
	capturedPythonFetchInboxReply    = `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"[{\"id\":2,\"project_id\":1,\"sender_id\":1,\"thread_id\":\"SEED-1\",\"reply_to\":null,\"topic\":null,\"subject\":\"seed subject\",\"importance\":\"normal\",\"ack_required\":false,\"created_ts\":\"2026-10-20T05:07:25.671399+00:00\",\"attachments\":[],\"from\":\"BlueLake\",\"kind\":\"to\",\"read_at\":null}]"}],"structuredContent":{"result":[{"id":2,"project_id":1,"sender_id":1,"thread_id":"SEED-1","reply_to":null,"topic":null,"subject":"seed subject","importance":"normal","ack_required":false,"created_ts":"2026-10-20T05:07:25.671399+00:00","attachments":[],"from":"BlueLake","kind":"to","read_at":null}]},"isError":false}}`
	capturedPythonWhoisReply         = `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"id\":2,\"name\":\"GreenCastle\",\"program\":\"probe\",\"model\":\"probe\",\"task_description\":\"bd-m0gm2 seed\",\"inception_ts\":\"2026-10-20T05:07:25.327443+00:00\",\"last_active_ts\":\"2026-10-20T05:24:42.232976+00:00\",\"project_id\":1,\"attachments_policy\":\"auto\",\"recent_commits\":[]}"}],"structuredContent":{"id":2,"name":"GreenCastle","program":"probe","model":"probe","task_description":"bd-m0gm2 seed","inception_ts":"2026-10-20T05:07:25.327443+00:00","last_active_ts":"2026-10-20T05:24:42.232976+00:00","project_id":1,"attachments_policy":"auto","recent_commits":[]},"isError":false}}`
	capturedPythonActiveForceReply   = `{"jsonrpc":"2.0","id":38,"result":{"content":[{"type":"text","text":"Error calling tool 'force_release_file_reservation': Reservation still shows recent activity; refusing forced release."}],"isError":true}}`
	capturedPythonReservationReply   = `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"granted\":[{\"id\":4,\"path_pattern\":\"probe/c.txt\",\"exclusive\":true,\"reason\":\"bd-q7kzm capture\",\"expires_ts\":\"2026-10-20T05:26:42.061712+00:00\",\"reused\":false}],\"conflicts\":[],\"warnings\":[\"enforcement_off_for_code_paths: 1 of 1 reserved paths are code-repo paths; server-side exclusivity is advisory only. Install the pre-commit guard via ` + "`install_precommit_guard`" + ` for the authoritative reservation gate.\"]}"}],"structuredContent":{"granted":[{"id":4,"path_pattern":"probe/c.txt","exclusive":true,"reason":"bd-q7kzm capture","expires_ts":"2026-10-20T05:26:42.061712+00:00","reused":false}],"conflicts":[],"warnings":["enforcement_off_for_code_paths: 1 of 1 reserved paths are code-repo paths; server-side exclusivity is advisory only. Install the pre-commit guard via ` + "`install_precommit_guard`" + ` for the authoritative reservation gate."]},"isError":false}}`
	capturedRustReservationGrantText = `{"granted":[{"id":14598,"path_pattern":"scratch-bd-m0gm2-probe/does-not-exist.txt","exclusive":true,"reason":"bd-m0gm2 live probe (nonexistent path)","expires_ts":"2026-10-20T04:48:02.649015Z"}],"conflicts":[]}`
)

// TestPeekInboxes_PythonServer: the Python server's fetch_inbox takes no
// mark_read and refuses a call that sends it, but never changes read state,
// so a peek there retries without the flag. The Rust server needs the flag,
// so it goes out first on every peek.
func TestPeekInboxes_PythonServer(t *testing.T) {
	t.Parallel()

	newReplay := func() *pythonToolReplay {
		return &pythonToolReplay{
			tool:   "fetch_inbox",
			params: []string{"project_key", "agent_name", "limit", "urgent_only", "include_bodies", "since_ts", "topic", "unread_only", "registration_token", "format"},
			issued: "TOK-GREEN",
			reply:  capturedPythonFetchInboxReply,
		}
	}

	t.Run("peek", func(t *testing.T) {
		t.Parallel()
		replay := newReplay()
		server := httptest.NewServer(replay)
		defer server.Close()

		c := NewClient(WithBaseURL(server.URL + "/"))
		c.SetRegistrationToken("/p", "GreenCastle", "TOK-GREEN")
		peek := c.PeekInboxes(context.Background(), FetchInboxOptions{ProjectKey: "/p", Limit: 1}, []string{"GreenCastle"})["GreenCastle"]
		if peek.Err != nil {
			t.Fatalf("peek: %v", peek.Err)
		}
		if len(peek.Messages) != 1 || peek.Messages[0].ID != 2 || peek.Messages[0].From != "BlueLake" || peek.Messages[0].ReadAt != nil {
			t.Fatalf("messages = %+v, want unread message 2 from BlueLake", peek.Messages)
		}
		calls := replay.argsOfCalls()
		if len(calls) != 2 {
			t.Fatalf("calls = %v, want the refused call with mark_read and one without", calls)
		}
		if markRead, set := calls[0]["mark_read"]; !set || markRead != false {
			t.Errorf("first call mark_read = %v (set=%t), want false", markRead, set)
		}
		// The replay refuses a call without the issued token, so the
		// messages above already show the retry carried it.
		if _, set := calls[1]["mark_read"]; set {
			t.Errorf("retry still sends mark_read: %v", calls[1])
		}
	})

	t.Run("read sends no mark_read, so no retry", func(t *testing.T) {
		t.Parallel()
		replay := newReplay()
		server := httptest.NewServer(replay)
		defer server.Close()

		c := NewClient(WithBaseURL(server.URL + "/"))
		c.SetRegistrationToken("/p", "GreenCastle", "TOK-GREEN")
		if _, err := c.FetchInbox(context.Background(), FetchInboxOptions{ProjectKey: "/p", AgentName: "GreenCastle"}); err != nil {
			t.Fatalf("FetchInbox: %v", err)
		}
		if calls := replay.argsOfCalls(); len(calls) != 1 {
			t.Errorf("calls = %v, want one", calls)
		}
	})

	t.Run("the retry's own refusal is the error", func(t *testing.T) {
		t.Parallel()
		replay := newReplay()
		server := httptest.NewServer(replay)
		defer server.Close()

		c := NewClient(WithBaseURL(server.URL + "/"))
		peek := c.PeekInboxes(context.Background(), FetchInboxOptions{ProjectKey: "/p"}, []string{"GreenCastle"})["GreenCastle"]
		if !errors.Is(peek.Err, ErrUnauthorized) {
			t.Fatalf("peek without a token: err = %v, want ErrUnauthorized", peek.Err)
		}
		if calls := replay.argsOfCalls(); len(calls) != 2 {
			t.Errorf("calls = %v, want two", calls)
		}
	})
}

func TestRejectsArgument(t *testing.T) {
	t.Parallel()

	markRead := classifyToolErrorText("1 validation error for call[fetch_inbox]\nmark_read\n  Unexpected keyword argument [type=unexpected_keyword_argument, input_value=False, input_type=bool]\n    For further information visit https://errors.pydantic.dev/2.12/v/unexpected_keyword_argument")
	tests := []struct {
		name string
		err  error
		arg  string
		want bool
	}{
		{name: "the named argument", err: NewAPIError("fetch_inbox", 0, markRead), arg: "mark_read", want: true},
		{name: "another argument", err: NewAPIError("fetch_inbox", 0, markRead), arg: "limit", want: false},
		{name: "a wrong value, not an unknown argument", err: classifyToolErrorText("1 validation error for call[fetch_inbox]\nmark_read\n  Input should be a valid boolean [type=bool_type, input_value='x', input_type=str]"), arg: "mark_read", want: false},
		{name: "another refusal naming it", err: fmt.Errorf("%w: mark_read is off", ErrUnauthorized), arg: "mark_read", want: false},
		{name: "no error", err: nil, arg: "mark_read", want: false},
	}
	for _, tt := range tests {
		if got := rejectsArgument(tt.err, tt.arg); got != tt.want {
			t.Errorf("%s: rejectsArgument(%v, %q) = %t, want %t", tt.name, tt.err, tt.arg, got, tt.want)
		}
	}
}

// TestWhois_PythonServerNeedsRegistrationToken: the Python server refuses
// whois without the looked-up agent's registration_token.
func TestWhois_PythonServerNeedsRegistrationToken(t *testing.T) {
	t.Parallel()

	replay := &pythonToolReplay{
		tool:   "whois",
		params: []string{"project_key", "agent_name", "registration_token", "include_recent_commits", "commit_limit", "format"},
		issued: "TOK-GREEN",
		reply:  capturedPythonWhoisReply,
	}
	server := httptest.NewServer(replay)
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	c.SetRegistrationToken("/p", "GreenCastle", "TOK-GREEN")
	agent, err := c.Whois(context.Background(), "/p", "GreenCastle", false)
	if err != nil {
		t.Fatalf("Whois: %v", err)
	}
	if agent.Name != "GreenCastle" || agent.ID != 2 || agent.ProjectID != 1 || agent.Program != "probe" {
		t.Errorf("agent = %+v, want GreenCastle (id 2, project 1)", agent)
	}
}

// TestForceReleaseReservation_PythonServerNeedsRegistrationToken: the Python
// server authenticates the releasing agent before it weighs the reservation,
// so without the token serve never learns the reservation is still active.
func TestForceReleaseReservation_PythonServerNeedsRegistrationToken(t *testing.T) {
	t.Parallel()

	replay := &pythonToolReplay{
		tool:   "force_release_file_reservation",
		params: []string{"project_key", "agent_name", "file_reservation_id", "notify_previous", "note", "registration_token", "format"},
		issued: "TOK-RED",
		reply:  capturedPythonActiveForceReply,
	}
	server := httptest.NewServer(replay)
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	c.SetRegistrationToken("/p", "RedStone", "TOK-RED")
	_, err := c.ForceReleaseReservation(context.Background(), ForceReleaseOptions{
		ProjectKey: "/p", AgentName: "RedStone", ReservationID: 1, Note: "stale", NotifyPrevious: true,
	})
	if !errors.Is(err, ErrReservationConflict) {
		t.Fatalf("err = %v, want the server's still-active refusal (ErrReservationConflict)", err)
	}
}

// TestReservePaths_ReplayedGrants: neither server names the holder, the
// project or a creation time in a grant, since every grant goes to the agent
// the call named. The holder is filled in from the call; the project and
// creation time are left out of the JSON rather than sent as 0 and year 1.
func TestReservePaths_ReplayedGrants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		handler   http.Handler
		agent     string
		wantID    int
		wantAgent string
		wantPath  string
		wantUntil time.Time
	}{
		{
			name: "rust",
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(mcpTextReply(capturedRustReservationGrantText)))
			}),
			agent:     "RoseRiver",
			wantID:    14598,
			wantAgent: "RoseRiver",
			wantPath:  "scratch-bd-m0gm2-probe/does-not-exist.txt",
			wantUntil: time.Date(2026, 10, 20, 4, 48, 2, 649015000, time.UTC),
		},
		{
			name: "python",
			handler: &pythonToolReplay{
				tool:   "file_reservation_paths",
				params: []string{"project_key", "agent_name", "paths", "ttl_seconds", "exclusive", "reason", "registration_token", "format"},
				issued: "TOK-RED",
				reply:  capturedPythonReservationReply,
			},
			agent:     "RedStone",
			wantID:    4,
			wantAgent: "RedStone",
			wantPath:  "probe/c.txt",
			wantUntil: time.Date(2026, 10, 20, 5, 26, 42, 61712000, time.UTC),
		},
		{
			// No server sends one today; a holder a grant does name is kept,
			// so a receipt check can still catch a grant for someone else.
			name: "a grant naming its holder",
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(mcpTextReply(`{"granted":[{"id":7,"agent_name":"OtherAgent","path_pattern":"a.txt","exclusive":true,"reason":"r","expires_ts":"2026-10-20T04:48:02Z"}],"conflicts":[]}`)))
			}),
			agent:     "RoseRiver",
			wantID:    7,
			wantAgent: "OtherAgent",
			wantPath:  "a.txt",
			wantUntil: time.Date(2026, 10, 20, 4, 48, 2, 0, time.UTC),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(tt.handler)
			defer server.Close()

			c := NewClient(WithBaseURL(server.URL + "/"))
			c.SetRegistrationToken("/p", "RedStone", "TOK-RED")
			result, err := c.ReservePaths(context.Background(), FileReservationOptions{
				ProjectKey: "/p", AgentName: tt.agent, Paths: []string{tt.wantPath}, TTLSeconds: 120, Exclusive: true, Reason: "probe",
			})
			if err != nil {
				t.Fatalf("ReservePaths: %v", err)
			}
			if len(result.Granted) != 1 {
				t.Fatalf("granted = %+v, want one", result.Granted)
			}
			granted := result.Granted[0]
			if granted.ID != tt.wantID || granted.AgentName != tt.wantAgent || granted.PathPattern != tt.wantPath ||
				!granted.Exclusive || !granted.ExpiresTS.Equal(tt.wantUntil) {
				t.Errorf("granted = %+v, want id %d held by %s on %s until %s", granted, tt.wantID, tt.wantAgent, tt.wantPath, tt.wantUntil)
			}

			encoded, err := json.Marshal(granted)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var fields map[string]interface{}
			if err := json.Unmarshal(encoded, &fields); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			for _, absent := range []string{"project_id", "created_ts"} {
				if value, present := fields[absent]; present {
					t.Errorf("granted JSON carries %s = %v, which the server never sent: %s", absent, value, encoded)
				}
			}
			if fields["agent_name"] != tt.wantAgent {
				t.Errorf("granted JSON agent_name = %v, want %s", fields["agent_name"], tt.wantAgent)
			}
		})
	}
}

func TestSetContactPolicy(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(mockMCPHandler(t, map[string]func(args map[string]interface{}) (interface{}, *JSONRPCError){
		"set_contact_policy": func(args map[string]interface{}) (interface{}, *JSONRPCError) {
			// Both servers reply {agent, policy} (Rust contacts.rs
			// SimplePolicyResponse, Python app.py set_contact_policy).
			return map[string]interface{}{
				"agent":  "BlueLake",
				"policy": args["policy"],
			}, nil
		},
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	result, err := c.SetContactPolicy(context.Background(), "/test", "BlueLake", "open")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.AgentName != "BlueLake" {
		t.Fatalf("agent_name = %q, want BlueLake", result.AgentName)
	}
	if result.Policy != "open" {
		t.Fatalf("policy = %q, want open", result.Policy)
	}
}

func TestCheckConflicts(t *testing.T) {
	t.Parallel()

	mustFlexTime := func(value string) FlexTime {
		t.Helper()
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			t.Fatalf("parse time %q: %v", value, err)
		}
		return FlexTime{Time: parsed}
	}

	server := httptest.NewServer(mockMCPHandler(t, map[string]func(args map[string]interface{}) (interface{}, *JSONRPCError){
		"list_file_reservations": func(args map[string]interface{}) (interface{}, *JSONRPCError) {
			return []FileReservation{
				{
					ID:          1,
					PathPattern: "internal/**",
					AgentName:   "BlueLake",
					Exclusive:   true,
					CreatedTS:   mustFlexTime("2026-01-01T00:00:00Z"),
					ExpiresTS:   mustFlexTime("2099-01-01T00:00:00Z"),
				},
				{
					ID:          2,
					PathPattern: "internal/cli/*.go",
					AgentName:   "RedStone",
					Exclusive:   false,
					CreatedTS:   mustFlexTime("2026-01-01T00:00:00Z"),
					ExpiresTS:   mustFlexTime("2099-01-01T00:00:00Z"),
				},
				{
					ID:          3,
					PathPattern: "docs/**",
					AgentName:   "GreenLake",
					Exclusive:   false,
					CreatedTS:   mustFlexTime("2026-01-01T00:00:00Z"),
					ExpiresTS:   mustFlexTime("2099-01-01T00:00:00Z"),
				},
			}, nil
		},
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	conflicts, err := c.CheckConflicts(context.Background(), "/test", []string{"internal/cli/*.go", "docs/**"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("len(conflicts) = %d, want 1", len(conflicts))
	}
	if conflicts[0].Path != "internal/cli/*.go" {
		t.Errorf("path = %q, want internal/cli/*.go", conflicts[0].Path)
	}
	if got := strings.Join(conflicts[0].Holders, ","); got != "BlueLake,RedStone" {
		t.Errorf("holders = %q, want BlueLake,RedStone", got)
	}
}

func TestReservationPatternsOverlapKeepsQualifiedWildcardsWithinOneDirectory(t *testing.T) {
	t.Parallel()

	if reservationPatternsOverlap("internal/cli/subdir/file.go", "internal/cli/*.go") {
		t.Fatal("qualified wildcard matched a file in a nested directory")
	}
	if !reservationPatternsOverlap("internal/cli/file.go", "internal/cli/*.go") {
		t.Fatal("qualified wildcard did not match a file in its directory")
	}
	if !reservationPatternsOverlap("internal/cli/subdir/file.go", "*.go") {
		t.Fatal("basename wildcard did not match a nested Go file")
	}
}

func TestForceReleaseReservation(t *testing.T) {
	t.Parallel()

	// Real replies from the Rust server (am 0.3.36) to force-releasing an
	// expired reservation twice; the Python server builds the same shape. A
	// refused release comes back as a tool error instead.
	releasedAt := &FlexTime{Time: time.Date(2026, 10, 20, 2, 54, 51, 367874000, time.UTC)}
	tests := []struct {
		name  string
		reply string
		want  ForceReleaseResult
	}{
		{
			name:  "released",
			reply: `{"released":1,"released_at":"2026-10-20T02:54:51.367874Z","reservation":{"id":14493,"agent":"SapphireSquirrel","path_pattern":"scratch-bd-0clbc-force-release-probe/*","exclusive":true,"reason":"bd-0clbc live force-release reply capture (nonexistent path)","created_ts":"2026-10-20T02:49:44.568450Z","expires_ts":"2026-10-20T02:50:44.568450Z","released_ts":"2026-10-20T02:54:51.367874Z","stale_reasons":["agent_inactive>1800s","mail_activity_recent","path_pattern_unmatched"],"last_agent_activity_ts":"2026-10-20T02:16:43.412357Z","last_mail_activity_ts":"2026-10-20T02:50:15.124522Z","last_filesystem_activity_ts":null,"last_git_activity_ts":null,"notified":false}}`,
			want: ForceReleaseResult{
				Success:        true,
				ReleasedAt:     releasedAt,
				PreviousHolder: "SapphireSquirrel",
				PathPattern:    "scratch-bd-0clbc-force-release-probe/*",
			},
		},
		{
			name:  "already released",
			reply: `{"released":0,"released_at":"2026-10-20T02:54:51.367874Z","already_released":true}`,
			want:  ForceReleaseResult{AlreadyReleased: true, ReleasedAt: releasedAt},
		},
		{
			// The Python server releases a reservation whose TTL ran out
			// before it looks at activity, and says so with expired.
			name:  "python expired",
			reply: `{"released":0,"released_at":"2026-10-20T02:54:51.367874+00:00","already_released":true,"expired":true}`,
			want:  ForceReleaseResult{AlreadyReleased: true, ReleasedAt: releasedAt},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(mockMCPHandler(t, map[string]func(args map[string]interface{}) (interface{}, *JSONRPCError){
				"force_release_file_reservation": func(args map[string]interface{}) (interface{}, *JSONRPCError) {
					return json.RawMessage(tt.reply), nil
				},
			}))
			defer server.Close()

			c := NewClient(WithBaseURL(server.URL + "/"))
			result, err := c.ForceReleaseReservation(context.Background(), ForceReleaseOptions{
				ProjectKey:     "/test",
				AgentName:      "TestAgent",
				ReservationID:  1,
				Note:           "stale lock",
				NotifyPrevious: true,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.Success != tt.want.Success || result.AlreadyReleased != tt.want.AlreadyReleased ||
				result.PreviousHolder != tt.want.PreviousHolder ||
				result.PathPattern != tt.want.PathPattern || result.Notified != tt.want.Notified {
				t.Errorf("result = %+v, want %+v", *result, tt.want)
			}
			if result.ReleasedAt == nil || !result.ReleasedAt.Time.Equal(tt.want.ReleasedAt.Time) {
				t.Errorf("released_at = %v, want %v", result.ReleasedAt, tt.want.ReleasedAt.Time)
			}
		})
	}
}

func TestCallToolWithTimeout(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(mockMCPHandler(t, map[string]func(args map[string]interface{}) (interface{}, *JSONRPCError){
		"test_tool": func(args map[string]interface{}) (interface{}, *JSONRPCError) {
			return map[string]interface{}{"ok": true}, nil
		},
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	result, err := c.callToolWithTimeout(context.Background(), "test_tool", nil, 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(result), "true") {
		t.Errorf("unexpected result: %s", string(result))
	}
}

func TestRenewReservationsWithOptions_Alias(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(mockMCPHandler(t, map[string]func(args map[string]interface{}) (interface{}, *JSONRPCError){
		"renew_file_reservations": func(args map[string]interface{}) (interface{}, *JSONRPCError) {
			return RenewReservationsResult{Renewed: 1}, nil
		},
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	result, err := c.RenewReservationsWithOptions(context.Background(), RenewReservationsOptions{
		ProjectKey:    "/test",
		AgentName:     "TestAgent",
		ExtendSeconds: 900,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Renewed != 1 {
		t.Errorf("renewed = %d, want 1", result.Renewed)
	}
}

func TestListAgents_Alias(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(mockMCPHandler(t, map[string]func(args map[string]interface{}) (interface{}, *JSONRPCError){
		"resource:resource://agents/%2Ftest": func(args map[string]interface{}) (interface{}, *JSONRPCError) {
			return map[string]interface{}{
				"contents": []map[string]interface{}{
					{"text": `[{"id":1,"name":"BlueLake","program":"claude-code","model":"opus-4.5"}]`},
				},
			}, nil
		},
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	agents, err := c.ListAgents(context.Background(), "/test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("len(agents) = %d, want 1", len(agents))
	}
	if agents[0].Name != "BlueLake" {
		t.Errorf("agent name = %q, want BlueLake", agents[0].Name)
	}
}

func TestRegisterAgent_WithOptionalFields(t *testing.T) {
	t.Parallel()

	var receivedArgs map[string]interface{}
	server := httptest.NewServer(mockMCPHandler(t, map[string]func(args map[string]interface{}) (interface{}, *JSONRPCError){
		"register_agent": func(args map[string]interface{}) (interface{}, *JSONRPCError) {
			receivedArgs = args
			return Agent{ID: 1, Name: "TestAgent", Program: "ntm", Model: "opus"}, nil
		},
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	_, err := c.RegisterAgent(context.Background(), RegisterAgentOptions{
		ProjectKey:      "/test",
		Program:         "claude-code",
		Model:           "opus-4.5",
		Name:            "TestAgent",
		TaskDescription: "testing",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify optional fields were passed
	if receivedArgs["name"] != "TestAgent" {
		t.Errorf("name = %v, want TestAgent", receivedArgs["name"])
	}
	if receivedArgs["task_description"] != "testing" {
		t.Errorf("task_description = %v, want testing", receivedArgs["task_description"])
	}
}

func TestFetchInbox_WithAllOptions(t *testing.T) {
	t.Parallel()

	var receivedArgs map[string]interface{}
	server := httptest.NewServer(mockMCPHandler(t, map[string]func(args map[string]interface{}) (interface{}, *JSONRPCError){
		"fetch_inbox": func(args map[string]interface{}) (interface{}, *JSONRPCError) {
			receivedArgs = args
			return map[string]interface{}{
				"result": []InboxMessage{
					{ID: 1, Subject: "test", From: "BlueLake"},
				},
			}, nil
		},
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	sinceTS := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	msgs, err := c.FetchInbox(context.Background(), FetchInboxOptions{
		ProjectKey:    "/test",
		AgentName:     "TestAgent",
		UrgentOnly:    true,
		SinceTS:       &sinceTS,
		Limit:         5,
		IncludeBodies: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %d, want 1", len(msgs))
	}

	// Verify optional args were set
	if receivedArgs["urgent_only"] != true {
		t.Errorf("urgent_only = %v, want true", receivedArgs["urgent_only"])
	}
	if receivedArgs["include_bodies"] != true {
		t.Errorf("include_bodies = %v, want true", receivedArgs["include_bodies"])
	}
	if receivedArgs["limit"] == nil {
		t.Error("expected limit to be set")
	}
	if receivedArgs["since_ts"] == nil {
		t.Error("expected since_ts to be set")
	}
	if _, set := receivedArgs["mark_read"]; set {
		t.Errorf("mark_read = %v, want it left to the server default when not peeking", receivedArgs["mark_read"])
	}
}

// TestFetchInbox_PeekDoesNotMarkRead guards observers such as the robot
// projection refresh: fetch_inbox marks messages read by default, so reading
// another agent's inbox for status must send mark_read=false.
func TestFetchInbox_PeekDoesNotMarkRead(t *testing.T) {
	t.Parallel()

	var receivedArgs map[string]interface{}
	server := httptest.NewServer(mockMCPHandler(t, map[string]func(args map[string]interface{}) (interface{}, *JSONRPCError){
		"fetch_inbox": func(args map[string]interface{}) (interface{}, *JSONRPCError) {
			receivedArgs = args
			return []InboxMessage{}, nil
		},
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	if _, err := c.FetchInbox(context.Background(), FetchInboxOptions{
		ProjectKey: "/test",
		AgentName:  "TestAgent",
		Peek:       true,
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if markRead, set := receivedArgs["mark_read"]; !set || markRead != false {
		t.Fatalf("mark_read = %v (set=%t), want false", markRead, set)
	}
}

func TestCallTool_ServerDown(t *testing.T) {
	t.Parallel()

	c := NewClient(WithBaseURL("http://localhost:1/"))
	_, err := c.callTool(context.Background(), "test_tool", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !IsServerUnavailable(err) {
		t.Errorf("expected server unavailable error, got: %v", err)
	}
}

func TestCallTool_ContextTimeout(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	c := NewClient(WithBaseURL(server.URL + "/"))
	_, err := c.callTool(ctx, "test_tool", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !IsTimeout(err) {
		t.Errorf("expected timeout error, got: %v", err)
	}
}

func TestCallTool_Non200Status(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("error"))
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	_, err := c.callTool(context.Background(), "test_tool", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestCallTool_InvalidJSON(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("not json"))
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	_, err := c.callTool(context.Background(), "test_tool", nil)
	if err == nil {
		t.Fatal("expected error for invalid JSON response")
	}
}

func TestCallTool_WithBearerToken(t *testing.T) {
	t.Parallel()

	var receivedAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		resp := JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      1,
			Result:  json.RawMessage(`{"ok":true}`),
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL+"/"), WithToken("my-secret"))
	_, err := c.callTool(context.Background(), "test", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedAuth != "Bearer my-secret" {
		t.Errorf("auth = %q, want 'Bearer my-secret'", receivedAuth)
	}
}

func TestReadResource_Errors(t *testing.T) {
	t.Parallel()

	t.Run("unauthorized", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer server.Close()

		c := NewClient(WithBaseURL(server.URL + "/"))
		_, err := c.ReadResource(context.Background(), "resource://agents/test")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !IsUnauthorized(err) {
			t.Errorf("expected unauthorized, got: %v", err)
		}
	})

	t.Run("server_error", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("error"))
		}))
		defer server.Close()

		c := NewClient(WithBaseURL(server.URL + "/"))
		_, err := c.ReadResource(context.Background(), "resource://agents/test")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("timeout", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(200 * time.Millisecond)
		}))
		defer server.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()

		c := NewClient(WithBaseURL(server.URL + "/"))
		_, err := c.ReadResource(ctx, "resource://agents/test")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !IsTimeout(err) {
			t.Errorf("expected timeout, got: %v", err)
		}
	})

	t.Run("json_rpc_error", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			resp := JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      1,
				Error:   &JSONRPCError{Code: -32000, Message: "resource unavailable"},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
		}))
		defer server.Close()

		c := NewClient(WithBaseURL(server.URL + "/"))
		_, err := c.ReadResource(context.Background(), "resource://agents/test")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}

func TestNewClient_EnvVars(t *testing.T) {
	// Test env var fallback for base URL
	t.Setenv("AGENT_MAIL_URL", "http://envhost:9999/mcp/")
	t.Setenv("AGENT_MAIL_TOKEN", "env-token")

	c := NewClient()
	if c.baseURL != "http://envhost:9999/mcp/" {
		t.Errorf("baseURL = %q, want http://envhost:9999/mcp/", c.baseURL)
	}
	if c.bearerToken != "env-token" {
		t.Errorf("bearerToken = %q, want env-token", c.bearerToken)
	}
}

func TestNewClient_EnvURLNoTrailingSlash(t *testing.T) {
	t.Setenv("AGENT_MAIL_URL", "http://envhost:9999/mcp")
	t.Setenv("AGENT_MAIL_TOKEN", "")

	c := NewClient()
	if c.baseURL != "http://envhost:9999/mcp/" {
		t.Errorf("baseURL = %q, want http://envhost:9999/mcp/", c.baseURL)
	}
}
