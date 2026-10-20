package agentmail

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// =============================================================================
// FlexTime.UnmarshalJSON tests
// =============================================================================

func TestFlexTimeUnmarshalJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantErr bool
		wantUTC bool // expect result in UTC
	}{
		{"RFC3339", `"2026-01-15T10:30:00Z"`, false, true},
		{"RFC3339 with offset", `"2026-01-15T10:30:00+05:00"`, false, false},
		{"RFC3339Nano", `"2026-01-15T10:30:00.123456789Z"`, false, true},
		{"bare ISO8601", `"2026-01-15T10:30:00"`, false, true},
		{"bare with millis", `"2026-01-15T10:30:00.123"`, false, true},
		{"bare with micros", `"2026-01-15T10:30:00.123456"`, false, true},
		{"bare with nanos", `"2026-01-15T10:30:00.123456789"`, false, true},
		{"empty string", `""`, false, true},
		{"invalid format", `"not-a-date"`, true, false},
		{"invalid JSON", `42`, true, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var ft FlexTime
			err := json.Unmarshal([]byte(tc.input), &ft)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error for %s", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.input == `""` {
				if !ft.Time.IsZero() {
					t.Error("empty string should produce zero time")
				}
				return
			}
			if ft.Time.IsZero() {
				t.Error("parsed time should not be zero")
			}
		})
	}
}

func TestFlexTimeMarshalJSON(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC)
	ft := FlexTime{Time: ts}
	data, err := json.Marshal(ft)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	var s string
	json.Unmarshal(data, &s)
	if s == "" {
		t.Error("marshaled time should not be empty")
	}
}

func TestFlexTimeRoundTrip(t *testing.T) {
	t.Parallel()

	original := FlexTime{Time: time.Date(2026, 2, 4, 12, 0, 0, 123456789, time.UTC)}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var parsed FlexTime
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if !original.Time.Equal(parsed.Time) {
		t.Errorf("round-trip mismatch: %v != %v", original.Time, parsed.Time)
	}
}

// =============================================================================
// JSONRPCError tests
// =============================================================================

func TestJSONRPCError_Error(t *testing.T) {
	t.Parallel()

	t.Run("without data", func(t *testing.T) {
		t.Parallel()
		e := &JSONRPCError{Code: -32600, Message: "invalid request"}
		got := e.Error()
		if got != "JSON-RPC error -32600: invalid request" {
			t.Errorf("Error() = %q", got)
		}
	})

	t.Run("with data", func(t *testing.T) {
		t.Parallel()
		e := &JSONRPCError{Code: -32602, Message: "invalid params", Data: "missing field"}
		got := e.Error()
		if got != "JSON-RPC error -32602: invalid params (data: missing field)" {
			t.Errorf("Error() = %q", got)
		}
	})
}

// =============================================================================
// APIError tests
// =============================================================================

func TestAPIError_Error(t *testing.T) {
	t.Parallel()

	t.Run("with status code", func(t *testing.T) {
		t.Parallel()
		e := &APIError{Operation: "send_message", StatusCode: 500, Err: errors.New("internal error")}
		got := e.Error()
		want := "agentmail: send_message failed (HTTP 500): internal error"
		if got != want {
			t.Errorf("Error() = %q, want %q", got, want)
		}
	})

	t.Run("without status code", func(t *testing.T) {
		t.Parallel()
		e := &APIError{Operation: "fetch_inbox", Err: errors.New("connection refused")}
		got := e.Error()
		want := "agentmail: fetch_inbox failed: connection refused"
		if got != want {
			t.Errorf("Error() = %q, want %q", got, want)
		}
	})

	t.Run("unwrap", func(t *testing.T) {
		t.Parallel()
		inner := errors.New("root cause")
		e := &APIError{Operation: "test", Err: inner}
		if e.Unwrap() != inner {
			t.Error("Unwrap() should return inner error")
		}
	})
}

func TestNewAPIError(t *testing.T) {
	t.Parallel()

	inner := errors.New("cause")
	e := NewAPIError("register_agent", 400, inner)
	if e.Operation != "register_agent" {
		t.Errorf("Operation = %q, want %q", e.Operation, "register_agent")
	}
	if e.StatusCode != 400 {
		t.Errorf("StatusCode = %d, want 400", e.StatusCode)
	}
	if e.Err != inner {
		t.Error("Err should be the provided error")
	}
}

// =============================================================================
// mapJSONRPCError tests
// =============================================================================

func TestMapJSONRPCError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		input     *JSONRPCError
		wantNil   bool
		checkWith error // use errors.Is
	}{
		{"nil input", nil, true, nil},
		{"agent not registered", &JSONRPCError{Code: -32000, Message: "Agent not registered in project"}, false, ErrAgentNotRegistered},
		{"message not found", &JSONRPCError{Code: -32000, Message: "Message not found"}, false, ErrMessageNotFound},
		{"contact blocked", &JSONRPCError{Code: -32000, Message: "CONTACT_BLOCKED: target agent only accepts approved contacts"}, false, ErrContactBlocked},
		{"reservation conflict", &JSONRPCError{Code: -32000, Message: "Reservation conflict detected"}, false, ErrReservationConflict},
		{"reservation not found", &JSONRPCError{Code: -32000, Message: "Reservation 42 not found"}, false, ErrNotFound},
		{"generic not found", &JSONRPCError{Code: -32000, Message: "Thread not found"}, false, ErrNotFound},
		{"literal method not found", &JSONRPCError{Code: -32601, Message: "Method not found"}, false, ErrNotImplemented},
		{"invalid request code", &JSONRPCError{Code: -32600, Message: "bad request"}, false, ErrInvalidRequest},
		{"method not found code", &JSONRPCError{Code: -32601, Message: "unknown tool: get_message"}, false, ErrNotImplemented},
		{"invalid params code", &JSONRPCError{Code: -32602, Message: "params"}, false, ErrInvalidRequest},
		{"unknown code", &JSONRPCError{Code: -32099, Message: "custom error"}, false, nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := mapJSONRPCError(tc.input)
			if tc.wantNil {
				if got != nil {
					t.Errorf("expected nil, got %v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("expected non-nil error")
			}
			if tc.checkWith != nil && !errors.Is(got, tc.checkWith) {
				t.Errorf("errors.Is(%v, %v) = false", got, tc.checkWith)
			}
		})
	}
}

// =============================================================================
// Error sentinel helpers
// =============================================================================

func TestErrorSentinelHelpers(t *testing.T) {
	t.Parallel()

	t.Run("IsServerUnavailable", func(t *testing.T) {
		t.Parallel()
		if !IsServerUnavailable(ErrServerUnavailable) {
			t.Error("should detect ErrServerUnavailable")
		}
		if IsServerUnavailable(errors.New("other")) {
			t.Error("should not match other errors")
		}
	})

	t.Run("IsUnauthorized", func(t *testing.T) {
		t.Parallel()
		if !IsUnauthorized(ErrUnauthorized) {
			t.Error("should detect ErrUnauthorized")
		}
	})

	t.Run("IsNotFound", func(t *testing.T) {
		t.Parallel()
		if !IsNotFound(ErrNotFound) {
			t.Error("should detect ErrNotFound")
		}
	})

	t.Run("IsInvalidRequest", func(t *testing.T) {
		t.Parallel()
		if !IsInvalidRequest(ErrInvalidRequest) {
			t.Error("should detect ErrInvalidRequest")
		}
	})

	t.Run("IsTimeout", func(t *testing.T) {
		t.Parallel()
		if !IsTimeout(ErrTimeout) {
			t.Error("should detect ErrTimeout")
		}
	})

	t.Run("IsNotImplemented", func(t *testing.T) {
		t.Parallel()
		if !IsNotImplemented(ErrNotImplemented) {
			t.Error("should detect ErrNotImplemented")
		}
	})

	t.Run("IsReservationConflict", func(t *testing.T) {
		t.Parallel()
		if !IsReservationConflict(ErrReservationConflict) {
			t.Error("should detect ErrReservationConflict")
		}
	})

	t.Run("IsContactBlocked", func(t *testing.T) {
		t.Parallel()
		if !IsContactBlocked(ErrContactBlocked) {
			t.Error("should detect ErrContactBlocked")
		}
	})

	t.Run("wrapped error", func(t *testing.T) {
		t.Parallel()
		wrapped := &APIError{Operation: "test", Err: ErrNotFound}
		if !IsNotFound(wrapped) {
			t.Error("should detect wrapped ErrNotFound via errors.Is")
		}
	})
}

// =============================================================================
// ReservationConflict.UnmarshalJSON tests
// =============================================================================

func TestReservationConflictUnmarshalJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		input       string
		wantPath    string
		wantHolders []string
		wantErr     bool
	}{
		{
			name:        "null holders",
			input:       `{"path":"src/main.go","holders":null}`,
			wantPath:    "src/main.go",
			wantHolders: []string{},
			wantErr:     false,
		},
		{
			name:        "empty array holders",
			input:       `{"path":"src/lib.go","holders":[]}`,
			wantPath:    "src/lib.go",
			wantHolders: []string{},
			wantErr:     false,
		},
		{
			name:        "legacy string array holders",
			input:       `{"path":"internal/*.go","holders":["BlueLake","GreenCastle"]}`,
			wantPath:    "internal/*.go",
			wantHolders: []string{"BlueLake", "GreenCastle"},
			wantErr:     false,
		},
		{
			name:        "current format with agent field",
			input:       `{"path":"api/routes.go","holders":[{"agent":"RedMountain"},{"agent":"SilverRiver"}]}`,
			wantPath:    "api/routes.go",
			wantHolders: []string{"RedMountain", "SilverRiver"},
			wantErr:     false,
		},
		{
			name:        "current format with agent_name field",
			input:       `{"path":"db/schema.go","holders":[{"agent_name":"PurpleCloud"}]}`,
			wantPath:    "db/schema.go",
			wantHolders: []string{"PurpleCloud"},
			wantErr:     false,
		},
		{
			name:        "mixed agent and agent_name fields",
			input:       `{"path":"pkg/util.go","holders":[{"agent":"GoldStar"},{"agent_name":"BronzeMoon"},{"agent":"SilverSun","agent_name":"ignored"}]}`,
			wantPath:    "pkg/util.go",
			wantHolders: []string{"GoldStar", "BronzeMoon", "SilverSun"},
			wantErr:     false,
		},
		{
			name:        "object with empty agent fields skipped",
			input:       `{"path":"test.go","holders":[{"agent":""},{"agent":"ValidAgent"},{"agent_name":""}]}`,
			wantPath:    "test.go",
			wantHolders: []string{"ValidAgent"},
			wantErr:     false,
		},
		{
			name:        "omitted holders field treated as empty",
			input:       `{"path":"foo.go"}`,
			wantPath:    "foo.go",
			wantHolders: []string{},
			wantErr:     false,
		},
		{
			name:    "invalid JSON",
			input:   `{"path":"bad.go","holders":`,
			wantErr: true,
		},
		{
			name:    "unsupported holders format (number)",
			input:   `{"path":"num.go","holders":42}`,
			wantErr: true,
		},
		{
			name:    "unsupported holders format (string)",
			input:   `{"path":"str.go","holders":"not-an-array"}`,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var rc ReservationConflict
			err := json.Unmarshal([]byte(tc.input), &rc)

			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error for input %s", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if rc.Path != tc.wantPath {
				t.Errorf("Path = %q, want %q", rc.Path, tc.wantPath)
			}
			if len(rc.Holders) != len(tc.wantHolders) {
				t.Fatalf("Holders length = %d, want %d", len(rc.Holders), len(tc.wantHolders))
			}
			for i, h := range rc.Holders {
				if h != tc.wantHolders[i] {
					t.Errorf("Holders[%d] = %q, want %q", i, h, tc.wantHolders[i])
				}
			}
		})
	}
}

// TestReservationConflictUnmarshalJSON_DirectInvalid calls UnmarshalJSON directly
// with invalid JSON bytes to exercise the inner json.Unmarshal error path (line 196-198).
// Going through json.Unmarshal fails at the outer decoder before calling the method.
func TestReservationConflictUnmarshalJSON_DirectInvalid(t *testing.T) {
	t.Parallel()

	var rc ReservationConflict
	err := rc.UnmarshalJSON([]byte("not valid json"))
	if err == nil {
		t.Error("expected error for invalid JSON bytes passed directly to UnmarshalJSON")
	}
}

// TestFlexTimeUnmarshalJSON_BareFormatParsedAsUTC verifies bare ISO8601
// timestamps (without timezone) are parsed as UTC.
func TestFlexTimeUnmarshalJSON_BareFormatParsedAsUTC(t *testing.T) {
	t.Parallel()

	var ft FlexTime
	err := ft.UnmarshalJSON([]byte(`"2026-03-15T14:30:00"`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ft.Time.Year() != 2026 || ft.Time.Month() != 3 || ft.Time.Day() != 15 {
		t.Errorf("parsed date = %v, want 2026-03-15", ft.Time)
	}
}

// =============================================================================
// InboxMessage read state
// =============================================================================

// capturedRustFetchInboxResult is the JSON-RPC result of a real fetch_inbox
// call (mark_read=false) against the Rust Agent Mail server, am 0.3.36.
// Message 54666 had been marked read with mark_message_read; 54667 had not.
const capturedRustFetchInboxResult = `{"content":[{"type":"text","text":"[{\"id\":54667,\"project_id\":65,\"sender_id\":2109,\"thread_id\":\"bd-0clbc\",\"topic\":null,\"subject\":\"[bd-0clbc] fixture B: normal, stays unread\",\"importance\":\"normal\",\"ack_required\":false,\"from\":\"SapphireSquirrel\",\"created_ts\":\"2026-10-20T02:21:16.648945Z\",\"kind\":\"to\",\"attachments\":[]},{\"id\":54666,\"project_id\":65,\"sender_id\":2109,\"thread_id\":\"bd-0clbc\",\"topic\":null,\"subject\":\"[bd-0clbc] fixture A: urgent, will be marked read\",\"importance\":\"urgent\",\"ack_required\":true,\"from\":\"SapphireSquirrel\",\"created_ts\":\"2026-10-20T02:21:15.751360Z\",\"read_ts\":\"2026-10-20T02:21:19.493225Z\",\"kind\":\"to\",\"attachments\":[]}]"}]}`

// TestFetchInbox_CapturedRustReplyKeepsReadState replays a real server reply
// through FetchInbox. The Rust server sends the read time as read_ts, and
// every unread count in ntm depends on ReadAt picking it up.
func TestFetchInbox_CapturedRustReplyKeepsReadState(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req JSONRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		result := json.RawMessage(`{}`)
		if req.Method == "tools/call" {
			result = json.RawMessage(capturedRustFetchInboxResult)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: result})
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	msgs, err := c.FetchInbox(context.Background(), FetchInboxOptions{
		ProjectKey: "/Volumes/1tb/Projects/ntm",
		AgentName:  "SapphireSquirrel",
		Peek:       true,
	})
	if err != nil {
		t.Fatalf("FetchInbox: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("len(msgs) = %d, want 2", len(msgs))
	}

	byID := map[int]InboxMessage{}
	for _, m := range msgs {
		byID[m.ID] = m
	}
	read, unread := byID[54666], byID[54667]
	wantReadAt := time.Date(2026, 10, 20, 2, 21, 19, 493225000, time.UTC)
	if read.ReadAt == nil || !read.ReadAt.Time.Equal(wantReadAt) {
		t.Errorf("message 54666 ReadAt = %v, want %v", read.ReadAt, wantReadAt)
	}
	if read.Importance != "urgent" || !read.AckRequired || read.ThreadID == nil || *read.ThreadID != "bd-0clbc" {
		t.Errorf("message 54666 decoded as %+v", read)
	}
	if unread.ReadAt != nil {
		t.Errorf("message 54667 ReadAt = %v, want nil (unread)", unread.ReadAt.Time)
	}
}

// TestInboxMessageUnmarshalJSON_ReadState covers both servers' names for the
// read time: read_ts from the Rust server and read_at from the Python server
// (app.py _list_inbox sets read_at to an ISO time or null).
func TestInboxMessageUnmarshalJSON_ReadState(t *testing.T) {
	t.Parallel()

	readTime := time.Date(2026, 10, 20, 2, 21, 19, 493225000, time.UTC)
	tests := []struct {
		name       string
		row        string
		wantReadAt *time.Time
	}{
		{
			name:       "rust read_ts",
			row:        `{"id":1,"subject":"s","from":"A","created_ts":"2026-10-20T02:21:15.751360Z","read_ts":"2026-10-20T02:21:19.493225Z","kind":"to"}`,
			wantReadAt: &readTime,
		},
		{
			name: "rust unread omits read_ts",
			row:  `{"id":1,"subject":"s","from":"A","created_ts":"2026-10-20T02:21:15.751360Z","kind":"to"}`,
		},
		{
			name:       "python read_at",
			row:        `{"id":1,"project_id":2,"sender_id":3,"thread_id":null,"reply_to":null,"topic":null,"subject":"s","importance":"normal","ack_required":false,"created_ts":"2026-10-20T02:21:15.751360+00:00","attachments":[],"from":"A","kind":"to","read_at":"2026-10-20T02:21:19.493225+00:00"}`,
			wantReadAt: &readTime,
		},
		{
			name: "python unread sends null read_at",
			row:  `{"id":1,"project_id":2,"sender_id":3,"thread_id":null,"reply_to":null,"topic":null,"subject":"s","importance":"normal","ack_required":false,"created_ts":"2026-10-20T02:21:15.751360+00:00","attachments":[],"from":"A","kind":"to","read_at":null}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var m InboxMessage
			if err := json.Unmarshal([]byte(tt.row), &m); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if m.ID != 1 || m.From != "A" || m.Kind != "to" {
				t.Errorf("other fields decoded as %+v", m)
			}
			switch {
			case tt.wantReadAt == nil && m.ReadAt != nil:
				t.Errorf("ReadAt = %v, want nil", m.ReadAt.Time)
			case tt.wantReadAt != nil && (m.ReadAt == nil || !m.ReadAt.Time.Equal(*tt.wantReadAt)):
				t.Errorf("ReadAt = %v, want %v", m.ReadAt, *tt.wantReadAt)
			}
		})
	}
}

// TestCustomCodecsWriteTheServerShape checks that the types with a custom
// decoder also encode to the reply shape the servers send, so a stub server
// that returns one of them is decoded the way a real reply is.
func TestCustomCodecsWriteTheServerShape(t *testing.T) {
	t.Parallel()

	released := ForceReleaseResult{
		Success:        true,
		ReleasedAt:     &FlexTime{Time: time.Date(2026, 10, 20, 2, 50, 0, 0, time.UTC)},
		PreviousHolder: "BlueLake",
		PathPattern:    "a/*",
		Notified:       true,
	}
	data, err := json.Marshal(released)
	if err != nil {
		t.Fatalf("Marshal ForceReleaseResult: %v", err)
	}
	var wire struct {
		Released    int `json:"released"`
		Reservation struct {
			Agent       string `json:"agent"`
			PathPattern string `json:"path_pattern"`
			Notified    bool   `json:"notified"`
		} `json:"reservation"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("Unmarshal wire: %v", err)
	}
	if wire.Released != 1 || wire.Reservation.Agent != "BlueLake" || wire.Reservation.PathPattern != "a/*" || !wire.Reservation.Notified {
		t.Errorf("force-release wire = %s", data)
	}
	var back ForceReleaseResult
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("Unmarshal ForceReleaseResult: %v", err)
	}
	if back.Success != released.Success || back.PreviousHolder != released.PreviousHolder ||
		back.PathPattern != released.PathPattern || back.Notified != released.Notified ||
		back.ReleasedAt == nil || !back.ReleasedAt.Time.Equal(released.ReleasedAt.Time) {
		t.Errorf("force-release round trip = %+v, want %+v", back, released)
	}

	data, err = json.Marshal(ForceReleaseResult{AlreadyReleased: true, ReleasedAt: released.ReleasedAt})
	if err != nil {
		t.Fatalf("Marshal already-released ForceReleaseResult: %v", err)
	}
	if err := json.Unmarshal(data, &back); err != nil || back.Success || !back.AlreadyReleased ||
		back.ReleasedAt == nil || !back.ReleasedAt.Time.Equal(released.ReleasedAt.Time) {
		t.Errorf("already-released round trip = %+v (err %v) from %s, want AlreadyReleased and no Success", back, err, data)
	}
	if !strings.Contains(string(data), `"already_released":true`) || strings.Contains(string(data), `"reservation"`) {
		t.Errorf("already-released wire = %s, want already_released and no reservation", data)
	}

	row := SearchResult{ID: 7, Subject: "s", AckRequired: true, From: "BlueLake"}
	data, err = json.Marshal(row)
	if err != nil {
		t.Fatalf("Marshal SearchResult: %v", err)
	}
	var ack struct {
		AckRequired json.RawMessage `json:"ack_required"`
	}
	if err := json.Unmarshal(data, &ack); err != nil || string(ack.AckRequired) != "1" {
		t.Errorf("search row ack_required = %s (err %v), want 1", ack.AckRequired, err)
	}
	var backRow SearchResult
	if err := json.Unmarshal(data, &backRow); err != nil || !backRow.AckRequired || backRow.ID != 7 || backRow.From != "BlueLake" {
		t.Errorf("search round trip = %+v (err %v), want %+v", backRow, err, row)
	}
}
