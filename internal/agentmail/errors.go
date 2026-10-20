package agentmail

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Common errors returned by the Agent Mail client.
var (
	// ErrServerUnavailable is returned when the Agent Mail server is not reachable.
	ErrServerUnavailable = errors.New("agent mail server unavailable")

	// ErrUnauthorized is returned when Agent Mail refuses the caller's
	// credentials: an invalid or missing bearer token (HTTP 401), or a
	// missing or wrong registration, sender or agent token for the agent a
	// call acts as.
	ErrUnauthorized = errors.New("unauthorized")

	// ErrNotFound is returned when a requested resource doesn't exist.
	ErrNotFound = errors.New("resource not found")

	// ErrInvalidRequest is returned when the request parameters are invalid.
	ErrInvalidRequest = errors.New("invalid request parameters")

	// ErrTimeout is returned when a request times out.
	ErrTimeout = errors.New("request timed out")

	// ErrNotImplemented is returned when the connected Agent Mail server does not
	// expose the requested MCP capability.
	ErrNotImplemented = errors.New("operation not supported by agent mail")

	// ErrDisabled is returned by a client built with WithDisabled, which never
	// contacts the server because the configuration turned Agent Mail off.
	ErrDisabled = errors.New("agent mail disabled by configuration")

	// ErrAgentNotRegistered is returned when trying to use an unregistered agent.
	ErrAgentNotRegistered = errors.New("agent not registered")

	// ErrMessageNotFound is returned when a message ID doesn't exist.
	ErrMessageNotFound = errors.New("message not found")

	// ErrReservationConflict is returned when a file reservation conflicts with existing ones.
	ErrReservationConflict = errors.New("file reservation conflict")

	// ErrContactBlocked is returned when a contact policy blocks communication.
	ErrContactBlocked = errors.New("contact blocked")

	// ErrTransientBusy is returned when the server reports a temporary busy
	// condition (e.g. non-atomic create_agent_identity partially completed).
	// Callers should retry with backoff rather than treating this as a hard failure.
	ErrTransientBusy = errors.New("resource temporarily busy")
)

// JSONRPCError represents a JSON-RPC 2.0 error response.
type JSONRPCError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// Error implements the error interface.
func (e *JSONRPCError) Error() string {
	if e.Data != nil {
		return fmt.Sprintf("JSON-RPC error %d: %s (data: %v)", e.Code, e.Message, e.Data)
	}
	return fmt.Sprintf("JSON-RPC error %d: %s", e.Code, e.Message)
}

// APIError wraps errors from the Agent Mail API with additional context.
type APIError struct {
	Operation  string // The operation that failed (e.g., "send_message")
	StatusCode int    // HTTP status code (0 if not HTTP error)
	Err        error  // Underlying error
}

// Error implements the error interface.
func (e *APIError) Error() string {
	if e.StatusCode > 0 {
		return fmt.Sprintf("agentmail: %s failed (HTTP %d): %v", e.Operation, e.StatusCode, e.Err)
	}
	return fmt.Sprintf("agentmail: %s failed: %v", e.Operation, e.Err)
}

// Unwrap returns the underlying error for errors.Is/As support.
func (e *APIError) Unwrap() error {
	return e.Err
}

// NewAPIError creates a new APIError.
func NewAPIError(operation string, statusCode int, err error) *APIError {
	return &APIError{
		Operation:  operation,
		StatusCode: statusCode,
		Err:        err,
	}
}

// IsServerUnavailable returns true if the error indicates the server is unavailable.
func IsServerUnavailable(err error) bool {
	return errors.Is(err, ErrServerUnavailable)
}

// IsUnauthorized returns true if the error indicates an authentication failure.
func IsUnauthorized(err error) bool {
	return errors.Is(err, ErrUnauthorized)
}

// IsNotFound returns true if the error indicates a resource was not found.
func IsNotFound(err error) bool {
	return errors.Is(err, ErrNotFound)
}

// IsInvalidRequest returns true if the error indicates invalid request parameters.
func IsInvalidRequest(err error) bool {
	return errors.Is(err, ErrInvalidRequest)
}

// IsTimeout returns true if the error indicates a request timeout.
func IsTimeout(err error) bool {
	return errors.Is(err, ErrTimeout)
}

// IsNotImplemented returns true if the error indicates an unsupported operation.
func IsNotImplemented(err error) bool {
	return errors.Is(err, ErrNotImplemented)
}

// IsReservationConflict returns true if the error indicates a file reservation conflict.
func IsReservationConflict(err error) bool {
	return errors.Is(err, ErrReservationConflict)
}

// IsContactBlocked returns true if the error indicates a contact policy block.
func IsContactBlocked(err error) bool {
	return errors.Is(err, ErrContactBlocked)
}

// IsTransientBusy returns true if the error indicates a temporary busy condition
// that should be retried.
func IsTransientBusy(err error) bool {
	return errors.Is(err, ErrTransientBusy)
}

// authRefusalText matches the Python server's authentication refusals, which
// carry no error type on the wire: "<action> requires registration_token for
// agent 'X', ...", "... requires agent_token ...", "... requires a project
// registration token ...", "Invalid registration_token for agent 'X'." and
// "Invalid agent_token for agent 'X'." (lowercased).
var authRefusalText = regexp.MustCompile(`\b(requires|invalid) (a project )?[a-z_]*(registration_token|sender_token|agent_token|registration token)\b`)

// agentNotFoundText matches "Agent 'X' not found ..." (lowercased), which both
// servers send for an agent name the project does not have.
var agentNotFoundText = regexp.MustCompile(`\bagent '[^']*' not found\b`)

// classifyServerError returns the sentinel error an Agent Mail refusal stands
// for, or nil when it is none of them. It is the one place both servers'
// refusals are classified, for tool errors and JSON-RPC errors alike.
//
// errType is the error type the server named, if any. The Rust server names
// one for every tool error ({"error": {"type", "message", ...}} as the result
// text) and in the data of its JSON-RPC errors. The Python server sends only
// the message, so an empty or unknown type falls back to the message text.
func classifyServerError(errType, message string) error {
	msg := strings.ToLower(message)
	switch errType {
	case "AGENT_NOT_FOUND":
		return ErrAgentNotRegistered
	case "RESERVATION_ACTIVE":
		return ErrReservationConflict
	case "CONFLICT":
		// Also sent when the agent name namespace is exhausted, which is no
		// reservation conflict.
		if strings.Contains(msg, "reservation") {
			return ErrReservationConflict
		}
		return nil
	case "CONTACT_BLOCKED", "CONTACT_REQUIRED":
		return ErrContactBlocked
	case "AUTHENTICATION_REQUIRED", "SENDER_TOKEN_REQUIRED", "SENDER_TOKEN_MISMATCH":
		return ErrUnauthorized
	case "INVALID_ARGUMENT", "MISSING_FIELD", "TYPE_ERROR":
		return ErrInvalidRequest
	case "RESOURCE_BUSY", "DATABASE_POOL_EXHAUSTED":
		return ErrTransientBusy
	}
	if errType == "NOT_FOUND" || strings.HasSuffix(errType, "_NOT_FOUND") {
		if strings.Contains(msg, "message not found") {
			return ErrMessageNotFound
		}
		return ErrNotFound
	}
	if strings.HasPrefix(errType, "INVALID_") || strings.HasPrefix(errType, "EMPTY_") {
		return ErrInvalidRequest
	}

	switch {
	case authRefusalText.MatchString(msg) || strings.Contains(msg, "requires an authenticated agent") || strings.Contains(msg, "cannot be authenticated"):
		return ErrUnauthorized
	case strings.Contains(msg, "agent not registered") || agentNotFoundText.MatchString(msg):
		return ErrAgentNotRegistered
	case strings.Contains(msg, "message not found"):
		return ErrMessageNotFound
	case strings.Contains(msg, "contact_blocked") || (strings.Contains(msg, "contact") && strings.Contains(msg, "blocked")) ||
		strings.Contains(msg, "not accepting messages") || strings.Contains(msg, "contact approval required"):
		return ErrContactBlocked
	case strings.Contains(msg, "still shows recent activity") || (strings.Contains(msg, "conflict") && strings.Contains(msg, "reservation")):
		return ErrReservationConflict
	case strings.Contains(msg, "not found") && !strings.Contains(msg, "method not found"):
		return ErrNotFound
	case strings.Contains(msg, "busy") || strings.Contains(msg, "temporarily unavailable") || strings.Contains(msg, "overloaded"):
		return ErrTransientBusy
	case strings.Contains(msg, "timed out") || strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded"):
		return ErrTimeout
	case strings.Contains(msg, "validation error for call["):
		// pydantic rejecting the arguments before the Python tool runs.
		return ErrInvalidRequest
	}
	return nil
}

// rejectsArgument reports whether err is the Python server refusing a call
// because the tool has no parameter called name. Its pydantic report puts the
// argument on a line of its own with the reason indented below it:
// "1 validation error for call[fetch_inbox]\nmark_read\n  Unexpected keyword
// argument [type=unexpected_keyword_argument, ...]".
func rejectsArgument(err error, name string) bool {
	return IsInvalidRequest(err) && strings.Contains(err.Error(), "\n"+name+"\n  Unexpected keyword argument")
}

// stripPythonErrorPrefix drops the wrapping the Python server's MCP framework
// puts before a refusal: "Error calling tool '<name>': " on a tool error and
// "Error reading resource from template '<uri>': " on a resource error. The
// URI repeats the request's query, agent_token included, so the prefix must
// not reach an error message serve hands its callers.
func stripPythonErrorPrefix(text string) string {
	for _, prefix := range []string{"Error calling tool '", "Error reading resource from template '"} {
		if rest, ok := strings.CutPrefix(text, prefix); ok {
			if _, msg, found := strings.Cut(rest, "': "); found {
				return msg
			}
		}
	}
	return text
}

// classifyToolErrorText turns the text of a tool result with isError set into
// an error. The Rust server's text is {"error": {"type", "message", ...}}; the
// Python server's is "Error calling tool '<name>': <message>", or a pydantic
// validation report. The error wraps the sentinel the refusal classifies as,
// and is a plain "tool error" when it classifies as none.
func classifyToolErrorText(text string) error {
	var payload struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	errType, message := "", stripPythonErrorPrefix(text)
	if err := json.Unmarshal([]byte(text), &payload); err == nil && payload.Error.Message != "" {
		errType, message = payload.Error.Type, payload.Error.Message
	}

	described := message
	if errType != "" {
		described = errType + ": " + message
	}
	if kind := classifyServerError(errType, message); kind != nil {
		return fmt.Errorf("%w: %s", kind, described)
	}
	return fmt.Errorf("tool error: %s", described)
}

// jsonRPCErrorType returns the error type the Rust server puts in a JSON-RPC
// error's data ({"error": {"type": ...}}), or "" when there is none.
func jsonRPCErrorType(data interface{}) string {
	fields, _ := data.(map[string]interface{})
	inner, _ := fields["error"].(map[string]interface{})
	errType, _ := inner["type"].(string)
	return errType
}

// mapJSONRPCError converts JSON-RPC error codes to Go errors.
func mapJSONRPCError(rpcErr *JSONRPCError) error {
	if rpcErr == nil {
		return nil
	}

	if message := stripPythonErrorPrefix(rpcErr.Message); message != rpcErr.Message {
		rpcErr = &JSONRPCError{Code: rpcErr.Code, Message: message, Data: rpcErr.Data}
	}
	if kind := classifyServerError(jsonRPCErrorType(rpcErr.Data), rpcErr.Message); kind != nil {
		return fmt.Errorf("%w: %s", kind, rpcErr.Message)
	}

	// Map common JSON-RPC error codes
	switch rpcErr.Code {
	case -32600:
		return fmt.Errorf("%w: %s", ErrInvalidRequest, rpcErr.Message)
	case -32601:
		return fmt.Errorf("%w: %s", ErrNotImplemented, rpcErr.Message)
	case -32602:
		return fmt.Errorf("%w: %s", ErrInvalidRequest, rpcErr.Message)
	default:
		// Return the raw error for application-specific codes
		return rpcErr
	}
}
