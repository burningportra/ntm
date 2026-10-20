package serve

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Dicklesworthstone/ntm/internal/agentmail"
	"github.com/Dicklesworthstone/ntm/internal/robot/adapters"
	"github.com/Dicklesworthstone/ntm/internal/state"
)

// isolateSessionRegistries points the ntm session registries at an empty
// directory, so the token fallback reads only what a test writes.
func isolateSessionRegistries(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
}

// tokenRecorder answers Agent Mail calls with canned results and records the
// agent token each call carried, "" for none.
type tokenRecorder struct {
	mu   sync.Mutex
	sent map[string][]string // tool -> token per call
}

func (tr *tokenRecorder) handler(tool, tokenArg string, result interface{}) func(map[string]interface{}) (interface{}, *agentmail.JSONRPCError) {
	return func(args map[string]interface{}) (interface{}, *agentmail.JSONRPCError) {
		token, _ := args[tokenArg].(string)
		if tokenArg == "agent_token" {
			uri, _ := args["uri"].(string)
			if _, query, ok := strings.Cut(uri, "?"); ok {
				for _, pair := range strings.Split(query, "&") {
					if value, found := strings.CutPrefix(pair, "agent_token="); found {
						token = value
					}
				}
			}
		}
		tr.mu.Lock()
		if tr.sent == nil {
			tr.sent = make(map[string][]string)
		}
		tr.sent[tool] = append(tr.sent[tool], token)
		tr.mu.Unlock()
		return result, nil
	}
}

func (tr *tokenRecorder) tokens(tool string) []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return append([]string(nil), tr.sent[tool]...)
}

// agentScopedRoute is a mail or reservation route that acts as GreenCastle,
// the Agent Mail call it makes, and the argument that carries the token.
type agentScopedRoute struct {
	method, path, body string
	tool, tokenArg     string
	result             interface{}
}

var agentScopedRoutes = []agentScopedRoute{
	{"GET", "/api/v1/mail/agents/GreenCastle", "", "whois", "registration_token", map[string]interface{}{"id": 1, "name": "GreenCastle"}},
	{"GET", "/api/v1/mail/inbox?agent_name=GreenCastle", "", "fetch_inbox", "registration_token", []interface{}{}},
	{"POST", "/api/v1/mail/messages", `{"sender_name":"GreenCastle","to":["BlueLake"],"subject":"s","body_md":"b"}`, "send_message", "sender_token", map[string]interface{}{"deliveries": []interface{}{}, "count": 1}},
	{"GET", "/api/v1/mail/messages/3?agent_name=GreenCastle", "", "resource:resource://message/3", "agent_token", map[string]interface{}{"contents": []map[string]string{{"text": `{"id":3,"subject":"s","from":"BlueLake"}`}}}},
	{"POST", "/api/v1/mail/messages/3/reply", `{"sender_name":"GreenCastle","body_md":"b"}`, "reply_message", "sender_token", map[string]interface{}{"id": 4, "subject": "Re: s", "from": "GreenCastle"}},
	{"POST", "/api/v1/mail/messages/3/read?agent_name=GreenCastle", "", "mark_message_read", "registration_token", map[string]interface{}{"message_id": 3, "read": true}},
	{"POST", "/api/v1/mail/messages/3/ack?agent_name=GreenCastle", "", "acknowledge_message", "registration_token", map[string]interface{}{"message_id": 3, "acknowledged": true}},
	{"GET", "/api/v1/mail/search?q=probe&agent_name=GreenCastle", "", "search_messages", "registration_token", []interface{}{}},
	{"GET", "/api/v1/mail/threads/T-1/summary?agent_name=GreenCastle", "", "summarize_thread", "registration_token", map[string]interface{}{"thread_id": "T-1", "summary": map[string]interface{}{"participants": []string{"GreenCastle"}}}},
	{"GET", "/api/v1/mail/contacts?agent_name=GreenCastle", "", "list_contacts", "registration_token", []interface{}{}},
	{"POST", "/api/v1/mail/contacts/request", `{"from_agent":"GreenCastle","to_agent":"BlueLake"}`, "request_contact", "registration_token", map[string]interface{}{"from": "GreenCastle", "to": "BlueLake", "status": "pending"}},
	{"POST", "/api/v1/mail/contacts/respond", `{"to_agent":"GreenCastle","from_agent":"BlueLake","accept":true}`, "respond_contact", "registration_token", map[string]interface{}{"from": "BlueLake", "to": "GreenCastle", "approved": true}},
	{"PUT", "/api/v1/mail/contacts/policy", `{"agent_name":"GreenCastle","policy":"open"}`, "set_contact_policy", "registration_token", map[string]interface{}{"agent": "GreenCastle", "policy": "open"}},
	{"POST", "/api/v1/reservations", `{"agent_name":"GreenCastle","paths":["a.go"]}`, "file_reservation_paths", "registration_token", map[string]interface{}{"granted": []map[string]interface{}{{"id": 5, "path_pattern": "a.go", "expires_ts": agentScopedLeaseExpiry}}, "conflicts": []interface{}{}}},
	{"DELETE", "/api/v1/reservations", `{"agent_name":"GreenCastle","ids":[5]}`, "release_file_reservations", "registration_token", map[string]interface{}{"released": 1}},
	{"POST", "/api/v1/reservations/5/renew", `{"agent_name":"GreenCastle"}`, "renew_file_reservations", "registration_token", map[string]interface{}{"renewed": 1}},
	{"POST", "/api/v1/reservations/5/force-release", `{"agent_name":"GreenCastle"}`, "force_release_file_reservation", "registration_token", map[string]interface{}{"released": 1}},
}

// agentScopedLeaseExpiry keeps the granted lease live through the readback.
var agentScopedLeaseExpiry = time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)

// agentScopedHandlers answers the route's Agent Mail call through recorder.
// A grant is verified against the project resource and the active listing
// (GH#328), so the reservation route also answers those reads.
func agentScopedHandlers(srv *Server, route agentScopedRoute, recorder *tokenRecorder) map[string]func(map[string]interface{}) (interface{}, *agentmail.JSONRPCError) {
	handlers := map[string]func(map[string]interface{}) (interface{}, *agentmail.JSONRPCError){
		route.tool: recorder.handler(route.tool, route.tokenArg, route.result),
	}
	if route.tool != "file_reservation_paths" {
		return handlers
	}
	project := srv.projectDirSnapshot()
	resource := func(text string) func(map[string]interface{}) (interface{}, *agentmail.JSONRPCError) {
		return func(map[string]interface{}) (interface{}, *agentmail.JSONRPCError) {
			return map[string]interface{}{"contents": []map[string]string{{"text": text}}}, nil
		}
	}
	handlers["resource:resource://project/"+url.PathEscape(project)] = resource(fmt.Sprintf(`{"id":4,"slug":"p","human_key":%q}`, project))
	handlers["resource:resource://file_reservations/"+url.PathEscape(project)] = resource(fmt.Sprintf(
		`[{"id":5,"project_id":4,"agent":"GreenCastle","path_pattern":"a.go","exclusive":false,"reason":"","expires_ts":%q}]`, agentScopedLeaseExpiry))
	return handlers
}

func serveAgentScopedRoute(t *testing.T, srv *Server, route agentScopedRoute, token string) {
	t.Helper()
	req := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
	if route.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set(AgentMailTokenHeader, token)
	}
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code >= 300 {
		t.Fatalf("%s %s: status %d, body %s", route.method, route.path, rec.Code, rec.Body.String())
	}
}

// TestAgentScopedMailRoutesSendTheCallersToken: every route that acts as an
// agent sends the token its caller put in X-Agent-Mail-Token as that agent's
// token, and only on that request. Before bd-6m68a serve sent a token only
// for agents it had registered itself, so on the Python Agent Mail server
// every one of these calls was refused.
func TestAgentScopedMailRoutesSendTheCallersToken(t *testing.T) {
	isolateSessionRegistries(t)
	// Force-release reaches Agent Mail directly only under the "auto" policy;
	// the default approval workflow is covered by the HTTP fixture tests.
	setForceReleasePolicy(t, "auto")
	routes := append([]agentScopedRoute{
		// Release by id needs its own call, since it shares the tool with
		// DELETE /reservations.
		{"POST", "/api/v1/reservations/5/release?agent_name=GreenCastle", "", "release_file_reservations", "registration_token", map[string]interface{}{"released": 1}},
	}, agentScopedRoutes...)

	for _, route := range routes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			srv, _ := setupTestServer(t)
			recorder := &tokenRecorder{}
			srv.mailClient = newMockAgentMailMCPClient(t, agentScopedHandlers(srv, route, recorder))

			serveAgentScopedRoute(t, srv, route, "TOK-CALLER")
			serveAgentScopedRoute(t, srv, route, "")

			if got := recorder.tokens(route.tool); fmt.Sprint(got) != fmt.Sprint([]string{"TOK-CALLER", ""}) {
				t.Errorf("%s tokens per request = %q, want the caller's, then none", route.tokenArg, got)
			}
			if cached := srv.mailClient.RegistrationToken(srv.projectDirSnapshot(), "GreenCastle"); cached != "" {
				t.Errorf("shared client cached the caller's token %q", cached)
			}
		})
	}
}

// TestAgentScopedMailRoutesFallBackToSavedTokens: without a caller token,
// serve sends the token ntm saved for an agent it spawned, including one
// spawned after serve built its mail client. A caller's token still wins.
func TestAgentScopedMailRoutesFallBackToSavedTokens(t *testing.T) {
	isolateSessionRegistries(t)
	setForceReleasePolicy(t, "auto")

	for _, route := range agentScopedRoutes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			srv, _ := setupTestServer(t)
			recorder := &tokenRecorder{}
			srv.mailClient = newMockAgentMailMCPClient(t, agentScopedHandlers(srv, route, recorder))
			serveAgentScopedRoute(t, srv, route, "")

			registry := agentmail.NewSessionAgentRegistry("bd6m68a", srv.projectDirSnapshot())
			registry.Agents["bd6m68a__cc_1"] = "GreenCastle"
			registry.SetRegistrationToken("GreenCastle", "TOK-SAVED")
			if err := agentmail.SaveSessionAgentRegistry(registry); err != nil {
				t.Fatalf("save registry: %v", err)
			}
			serveAgentScopedRoute(t, srv, route, "")
			serveAgentScopedRoute(t, srv, route, "TOK-CALLER")

			if got := recorder.tokens(route.tool); fmt.Sprint(got) != fmt.Sprint([]string{"", "TOK-SAVED", "TOK-CALLER"}) {
				t.Errorf("%s tokens per request = %q, want none, the saved one, then the caller's", route.tokenArg, got)
			}
		})
	}
}

// TestCreateMailAgentUsesOnlyTheCallersToken: registering an existing name
// proves the identity with its token and hands the token back, so serve must
// not prove it with a token ntm saved, or any mail:write caller could read an
// ntm-spawned agent's token out of the reply. The token in the reply is the
// caller's, so serve does not cache it either.
func TestCreateMailAgentUsesOnlyTheCallersToken(t *testing.T) {
	isolateSessionRegistries(t)
	srv, _ := setupTestServer(t)
	recorder := &tokenRecorder{}
	register := recorder.handler("register_agent", "registration_token", nil)
	srv.mailClient = newMockAgentMailMCPClient(t, map[string]func(map[string]interface{}) (interface{}, *agentmail.JSONRPCError){
		"register_agent": func(args map[string]interface{}) (interface{}, *agentmail.JSONRPCError) {
			_, _ = register(args)
			name, _ := args["name"].(string)
			if name == "" {
				name = "AmberFox"
			}
			return map[string]interface{}{"id": 1, "name": name, "program": "p", "model": "m", "registration_token": "TOK-ISSUED-" + name}, nil
		},
	})
	project := srv.projectDirSnapshot()
	srv.mailClient.SetRegistrationToken(project, "GreenCastle", "TOK-SAVED")

	create := func(body, token string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/mail/agents", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set(AgentMailTokenHeader, token)
		}
		rec := httptest.NewRecorder()
		srv.Router().ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s: status %d, body %s", body, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}

	create(`{"program":"p","model":"m","name":"GreenCastle"}`, "")
	create(`{"program":"p","model":"m","name":"GreenCastle"}`, "TOK-CALLER")
	if body := create(`{"program":"p","model":"m"}`, ""); !strings.Contains(body, "TOK-ISSUED-AmberFox") {
		t.Errorf("new agent's reply = %s, want its registration_token", body)
	}

	if got := recorder.tokens("register_agent"); fmt.Sprint(got) != fmt.Sprint([]string{"", "TOK-CALLER", ""}) {
		t.Errorf("registration_token per request = %q, want none, the caller's, none", got)
	}
	for name, want := range map[string]string{"GreenCastle": "TOK-SAVED", "AmberFox": ""} {
		if cached := srv.mailClient.RegistrationToken(project, name); cached != want {
			t.Errorf("cached token for %s = %q, want %q", name, cached, want)
		}
	}
}

// TestCallerTokensStayWithTheirRequests: concurrent requests acting as
// different agents each send their own caller's token and nothing is cached.
func TestCallerTokensStayWithTheirRequests(t *testing.T) {
	isolateSessionRegistries(t)
	srv, _ := setupTestServer(t)
	var mu sync.Mutex
	sent := map[string][]string{}
	srv.mailClient = newMockAgentMailMCPClient(t, map[string]func(map[string]interface{}) (interface{}, *agentmail.JSONRPCError){
		"whois": func(args map[string]interface{}) (interface{}, *agentmail.JSONRPCError) {
			name, _ := args["agent_name"].(string)
			token, _ := args["registration_token"].(string)
			mu.Lock()
			sent[name] = append(sent[name], token)
			mu.Unlock()
			return map[string]interface{}{"id": 1, "name": name}, nil
		},
	})

	const agents = 16
	var wg sync.WaitGroup
	for i := range agents {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/mail/agents/Agent%02d", i), nil)
			req.Header.Set(AgentMailTokenHeader, fmt.Sprintf("TOK-%02d", i))
			rec := httptest.NewRecorder()
			srv.Router().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("whois Agent%02d: status %d", i, rec.Code)
			}
		}()
	}
	wg.Wait()

	for i := range agents {
		name := fmt.Sprintf("Agent%02d", i)
		if want := fmt.Sprintf("TOK-%02d", i); fmt.Sprint(sent[name]) != fmt.Sprint([]string{want}) {
			t.Errorf("%s sent %q, want only %q", name, sent[name], want)
		}
		if cached := srv.mailClient.RegistrationToken(srv.projectDirSnapshot(), name); cached != "" {
			t.Errorf("shared client cached %s's caller token %q", name, cached)
		}
	}
}

// TestCallerTokenStaysOutOfErrors: FastMCP's masked resource error quotes the
// URI, agent_token included, and serve hands Agent Mail errors back to the
// caller. The token must not be in that text.
func TestCallerTokenStaysOutOfErrors(t *testing.T) {
	isolateSessionRegistries(t)
	srv, _ := setupTestServer(t)
	srv.mailClient = newMockAgentMailMCPClient(t, map[string]func(map[string]interface{}) (interface{}, *agentmail.JSONRPCError){
		"resource:resource://message/3": func(args map[string]interface{}) (interface{}, *agentmail.JSONRPCError) {
			return nil, &agentmail.JSONRPCError{Code: 0, Message: fmt.Sprintf("Error reading resource from template %q", args["uri"])}
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/mail/messages/3?agent_name=GreenCastle", nil)
	req.Header.Set(AgentMailTokenHeader, "TOK-SECRET-CALLER")
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code < 400 {
		t.Fatalf("status %d, want an error", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, "TOK-SECRET-CALLER") {
		t.Errorf("error response carries the caller's token: %s", body)
	}
}

// TestCORSAllowsTheAgentMailTokenHeader: browser callers send the token in a
// custom header, which a preflight must allow.
func TestCORSAllowsTheAgentMailTokenHeader(t *testing.T) {
	srv := New(Config{})
	handler := srv.corsMiddlewareFunc(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodOptions, "/api/v1/mail/inbox", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if headers := rec.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(headers, AgentMailTokenHeader) {
		t.Fatalf("Access-Control-Allow-Headers = %q, want %s", headers, AgentMailTokenHeader)
	}
}

// TestAgentMailRefusalsMapToHTTPStatus replays refusals the Rust (am 0.3.36)
// and Python (mcp_agent_mail 0.3.4) Agent Mail servers sent live, through the
// router, and checks the status and error code serve answers with. Before
// bd-m0gm2 every one of them answered 500 INTERNAL_ERROR.
func TestAgentMailRefusalsMapToHTTPStatus(t *testing.T) {
	// The force-release refusals come from Agent Mail itself, which the
	// handler reaches without an approval only under the "auto" policy.
	setForceReleasePolicy(t, "auto")
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

type forceReleaseHTTPFixture struct {
	server      *Server
	store       *state.Store
	router      http.Handler
	calls       atomic.Int32
	inspections atomic.Int32
	malformed   atomic.Bool
}

func newForceReleaseHTTPFixture(t *testing.T) *forceReleaseHTTPFixture {
	t.Helper()
	setForceReleasePolicy(t, "approval")
	server, store := setupTestServer(t)
	fixture := &forceReleaseHTTPFixture{server: server, store: store}
	server.mailClient = newMockAgentMailMCPClient(t, map[string]func(map[string]interface{}) (interface{}, *agentmail.JSONRPCError){
		"list_file_reservations": func(args map[string]interface{}) (interface{}, *agentmail.JSONRPCError) {
			fixture.inspections.Add(1)
			return []agentmail.FileReservation{{
				ID: 17, ProjectID: 4, AgentName: "BlueLake", PathPattern: "internal/serve/*",
				CreatedTS: agentmail.FlexTime{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
				ExpiresTS: agentmail.FlexTime{Time: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)},
				Exclusive: true,
			}}, nil
		},
		"force_release_file_reservation": func(args map[string]interface{}) (interface{}, *agentmail.JSONRPCError) {
			fixture.calls.Add(1)
			if args["project_key"] != agentmail.CanonicalProjectKey(server.projectDirSnapshot()) ||
				args["agent_name"] != "RedStone" || args["file_reservation_id"] != float64(17) {
				t.Errorf("release changed approved project/actor/reservation: %+v", args)
			}
			if fixture.malformed.Load() {
				// The server may have acted, but its receipt cannot be
				// decoded. A retry must not repeat this mutation.
				return map[string]interface{}{"success": "receipt lost"}, nil
			}
			return agentmail.ForceReleaseResult{
				Success: true, PreviousHolder: "BlueLake", PathPattern: "internal/serve/*",
			}, nil
		},
	})
	router := chi.NewRouter()
	router.Use(server.idempotencyMiddleware)
	router.With(server.RequirePermission(PermForceRelease)).Post("/api/v1/reservations/{id}/force-release", server.handleForceReleaseReservation)
	router.With(server.RequirePermission(PermApproveRequests)).Post("/api/v1/approvals/{id}/approve", server.handleApprovalApproveV1)
	router.With(server.RequirePermission(PermApproveRequests)).Post("/api/v1/approvals/{id}/deny", server.handleApprovalDenyV1)
	fixture.router = router
	return fixture
}

func (fixture *forceReleaseHTTPFixture) post(t *testing.T, actor, path, key string, body interface{}) (*httptest.ResponseRecorder, map[string]interface{}) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	req := httptest.NewRequest(http.MethodPost, path, reader)
	// Authentication's validated RBAC context is the only identity input.
	req = req.WithContext(withRoleContext(req.Context(), &RoleContext{Role: RoleAdmin, UserID: actor}))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	fixture.router.ServeHTTP(rec, req)
	var decoded map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode HTTP response: %v: %s", err, rec.Body.String())
	}
	return rec, decoded
}

func forceReleaseResponseApprovalID(t *testing.T, body map[string]interface{}) string {
	t.Helper()
	id, _ := body["approval_id"].(string)
	if id == "" {
		if details, ok := body["details"].(map[string]interface{}); ok {
			id, _ = details["approval_id"].(string)
		}
	}
	if id == "" {
		t.Fatalf("approval ID missing: %+v", body)
	}
	return id
}

func (fixture *forceReleaseHTTPFixture) approve(t *testing.T, id string) {
	t.Helper()
	rec, _ := fixture.post(t, "bob", "/api/v1/approvals/"+id+"/approve", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("second-operator approval: %d: %s", rec.Code, rec.Body.String())
	}
}

func TestForceReleaseHTTPApprovalLifecycleAndIdempotentReplay(t *testing.T) {
	fixture := newForceReleaseHTTPFixture(t)
	const path = "/api/v1/reservations/17/force-release"
	body := ForceReleaseRequest{AgentName: "RedStone", Note: "holder crashed", NotifyPrevious: true}
	rec, decoded := fixture.post(t, "alice", path, "release-one", body)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("initial request: %d: %s", rec.Code, rec.Body.String())
	}
	id := forceReleaseResponseApprovalID(t, decoded)
	record, err := fixture.store.GetApproval(id)
	if err != nil || record == nil || record.RequestedBy != "alice" || !record.RequiresSLB ||
		!strings.Contains(record.Resource, "BlueLake") || !strings.Contains(record.Resource, "internal/serve/*") {
		t.Fatalf("approval omitted principal or lease review evidence: %+v, %v", record, err)
	}
	rec, decoded = fixture.post(t, "alice", path, "release-one", body)
	if rec.Code != http.StatusForbidden || forceReleaseResponseApprovalID(t, decoded) != id {
		t.Fatalf("pending replay changed approval: %d: %s", rec.Code, rec.Body.String())
	}
	if fixture.calls.Load() != 0 {
		t.Fatal("pending request reached external release")
	}
	rec, _ = fixture.post(t, "alice", "/api/v1/approvals/"+id+"/approve", "", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("self-approval: %d: %s", rec.Code, rec.Body.String())
	}
	fixture.approve(t, id)
	rec, decoded = fixture.post(t, "alice", path, "release-one", body)
	if rec.Code != http.StatusOK || decoded["force_released"] != true || decoded["approval_status"] != "consumed" {
		t.Fatalf("approved release: %d: %s", rec.Code, rec.Body.String())
	}
	rec, _ = fixture.post(t, "alice", path, "release-one", body)
	if rec.Code != http.StatusOK || rec.Header().Get("X-Idempotent-Replay") != "true" || fixture.calls.Load() != 1 {
		t.Fatalf("successful retry repeated mutation: %d, calls=%d: %s", rec.Code, fixture.calls.Load(), rec.Body.String())
	}
	inspections := fixture.inspections.Load()
	rec, decoded = fixture.post(t, "alice", path, "a-new-http-request", body)
	if rec.Code != http.StatusConflict || forceReleaseResponseApprovalID(t, decoded) != id ||
		fixture.calls.Load() != 1 || fixture.inspections.Load() != inspections {
		t.Fatalf("spent replay must refuse before another lease read/release: %d: %s", rec.Code, rec.Body.String())
	}
	history, err := fixture.server.approvalEngine().History(t.Context())
	if err != nil || len(history) != 1 {
		t.Fatalf("replay minted a new approval: %+v, %v", history, err)
	}
}

func TestForceReleaseHTTPUnconfirmedReceiptRequiresExplicitReauthorization(t *testing.T) {
	fixture := newForceReleaseHTTPFixture(t)
	fixture.malformed.Store(true)
	const path = "/api/v1/reservations/17/force-release"
	body := ForceReleaseRequest{AgentName: "RedStone", Note: "holder crashed"}
	_, decoded := fixture.post(t, "alice", path, "uncertain-release", body)
	id := forceReleaseResponseApprovalID(t, decoded)
	fixture.approve(t, id)
	rec, decoded := fixture.post(t, "alice", path, "uncertain-release", body)
	details, _ := decoded["details"].(map[string]interface{})
	if rec.Code < 400 || details["approval_status"] != "consumed" ||
		details["execution_status"] != "unconfirmed" || fixture.calls.Load() != 1 {
		t.Fatalf("malformed receipt lost spent/uncertain evidence: %d: %s", rec.Code, rec.Body.String())
	}
	rec, _ = fixture.post(t, "alice", path, "uncertain-release", body)
	if rec.Code != http.StatusConflict || fixture.calls.Load() != 1 {
		t.Fatalf("uncertain retry repeated release: %d, calls=%d", rec.Code, fixture.calls.Load())
	}

	body.NewApproval = id
	rec, decoded = fixture.post(t, "alice", path, "deliberate-new-attempt", body)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("explicit new attempt: %d: %s", rec.Code, rec.Body.String())
	}
	nextID := forceReleaseResponseApprovalID(t, decoded)
	if nextID == id {
		t.Fatal("explicit new attempt reused consumed grant")
	}
	rec, decoded = fixture.post(t, "alice", path, "deliberate-new-attempt", body)
	if rec.Code != http.StatusForbidden || forceReleaseResponseApprovalID(t, decoded) != nextID {
		t.Fatalf("renewal replay did not reuse successor: %d: %s", rec.Code, rec.Body.String())
	}
	fixture.approve(t, nextID)
	fixture.malformed.Store(false)
	rec, _ = fixture.post(t, "alice", path, "deliberate-new-attempt", body)
	if rec.Code != http.StatusOK || fixture.calls.Load() != 2 {
		t.Fatalf("approved new attempt: %d: %s", rec.Code, rec.Body.String())
	}
	rec, _ = fixture.post(t, "alice", path, "renewal-with-another-http-key", body)
	if rec.Code != http.StatusConflict || fixture.calls.Load() != 2 {
		t.Fatalf("renewal replay minted another execution: %d: %s", rec.Code, rec.Body.String())
	}
	history, err := fixture.server.approvalEngine().History(t.Context())
	if err != nil || len(history) != 2 {
		t.Fatalf("renewal replays minted extra approvals: %+v, %v", history, err)
	}
}

func TestForceReleaseHTTPApprovalCannotChangeRequesterOrMailActor(t *testing.T) {
	fixture := newForceReleaseHTTPFixture(t)
	const path = "/api/v1/reservations/17/force-release"
	body := ForceReleaseRequest{AgentName: "RedStone"}
	_, decoded := fixture.post(t, "alice", path, "", body)
	id := forceReleaseResponseApprovalID(t, decoded)
	fixture.approve(t, id)

	spoofed := map[string]interface{}{"agent_name": "RedStone", "requested_by": "alice", "requestor": "alice"}
	rec, decoded := fixture.post(t, "mallory", path, "", spoofed)
	if rec.Code != http.StatusForbidden || fixture.calls.Load() != 0 {
		t.Fatalf("body identity spoof dispatched release: %d: %s", rec.Code, rec.Body.String())
	}
	otherID := forceReleaseResponseApprovalID(t, decoded)
	record, err := fixture.store.GetApproval(otherID)
	if err != nil || record == nil || record.RequestedBy != "mallory" || otherID == id {
		t.Fatalf("body controlled trusted requester: %+v, %v", record, err)
	}
	body.NewApproval = id
	rec, _ = fixture.post(t, "mallory", path, "", body)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign predecessor accepted: %d: %s", rec.Code, rec.Body.String())
	}
	body.AgentName = "GreenCastle"
	rec, _ = fixture.post(t, "alice", path, "", body)
	if rec.Code != http.StatusForbidden || fixture.calls.Load() != 0 {
		t.Fatalf("changed mail actor reused approval: %d: %s", rec.Code, rec.Body.String())
	}
	record, _ = fixture.store.GetApproval(id)
	if record == nil || record.Status != state.ApprovalApproved {
		t.Fatalf("foreign attempts spent original approval: %+v", record)
	}
}
