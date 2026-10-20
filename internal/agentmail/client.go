package agentmail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

const (
	// DefaultBaseURL is the default Agent Mail server URL.
	// Canonical source: config.DefaultAgentMailURL
	DefaultBaseURL = "http://127.0.0.1:8765/mcp/"
	// NOTE: This duplicates config.DefaultAgentMailURL to avoid an import cycle
	// (agentmail ← config ← agentmail). If you change this, update config.go too.

	// DefaultTimeout is the default HTTP request timeout.
	DefaultTimeout = 10 * time.Second

	// LongTimeout is used for operations that may take longer (search, summarize).
	LongTimeout = 30 * time.Second

	// HealthCheckPath is the path for health checks.
	HealthCheckPath = "health"

	// AvailabilityCacheTTL is how long to cache IsAvailable() results.
	AvailabilityCacheTTL = 30 * time.Second

	// availabilityProbeBudget bounds all liveness attempts and backoffs. Keep the
	// probe below two seconds so optional Agent Mail discovery cannot stall CLI
	// startup when the service is unavailable.
	availabilityProbeBudget   = 1250 * time.Millisecond
	availabilityProbeAttempts = 3
	availabilityProbeBackoff  = 150 * time.Millisecond
)

// Client provides methods to interact with the Agent Mail API.
type Client struct {
	baseURL     string
	bearerToken string
	httpClient  *http.Client
	projectKey  string // Cached project path
	disabled    bool   // Refuse every call without contacting the server
	requestID   atomic.Int64

	// Per-agent registration tokens. Server-side mcp-agent-mail >=2.13
	// requires identity-scoped tool calls (fetch_inbox, send_message,
	// acknowledge_message, …) to include the agent's `registration_token`
	// in the call args. Tokens are returned by `create_agent_identity` /
	// `register_agent` and we cache them here so callers don't have to
	// thread the token through every call site. Keyed by
	// `<project_key>\x1f<agent_name>` (\x1f is unit separator — safe
	// because neither component contains it).
	registrationTokensMu sync.RWMutex
	registrationTokens   map[string]string

	// Availability cache (30s TTL)
	healthCheckMu      sync.Mutex
	availableCache     atomic.Bool
	availableCacheTime atomic.Int64 // Unix timestamp in seconds

	// lastAvailabilityErr stores the terminal error of the most recent
	// completed availability probe (nil-equivalent cleared on success) so
	// health surfaces can explain WHY Agent Mail looks unavailable instead
	// of collapsing the reason into a bare bool.
	lastAvailabilityErr atomic.Value // of availabilityErrBox

	// inboxPeekSlots holds one token per PeekInboxes fetch in flight, so
	// every caller sharing this client shares InboxPeekConcurrency.
	inboxPeekSlotsOnce sync.Once
	inboxPeekSlots     chan struct{}
}

// availabilityErrBox wraps an error for atomic.Value storage (atomic.Value
// requires consistent concrete types; a nil error is stored as a box with a
// nil field).
type availabilityErrBox struct{ err error }

// tokenKey builds the cache key for the registration-token map. Using
// a unit-separator (\x1f) between components avoids any chance of
// `project:agent` ambiguity from paths that happen to contain colons.
func tokenKey(projectKey, agentName string) string {
	return projectKey + "\x1f" + agentName
}

// SetRegistrationToken stores the registration token for an agent so
// later identity-scoped MCP calls can include it. Callers that load
// agent metadata from disk (e.g. agent_registry.json) should populate
// the cache via this method at startup. Empty token clears the entry.
func (c *Client) SetRegistrationToken(projectKey, agentName, token string) {
	if c == nil {
		return
	}
	c.registrationTokensMu.Lock()
	defer c.registrationTokensMu.Unlock()
	if c.registrationTokens == nil {
		c.registrationTokens = make(map[string]string)
	}
	if token == "" {
		delete(c.registrationTokens, tokenKey(projectKey, agentName))
		return
	}
	c.registrationTokens[tokenKey(projectKey, agentName)] = token
}

// RegistrationToken returns the cached token for (project, agent), or
// the empty string if none is known. Safe for nil receiver.
func (c *Client) RegistrationToken(projectKey, agentName string) string {
	if c == nil {
		return ""
	}
	c.registrationTokensMu.RLock()
	defer c.registrationTokensMu.RUnlock()
	return c.registrationTokens[tokenKey(projectKey, agentName)]
}

// rememberRegistrationToken caches the token returned by an identity
// creation / registration call so subsequent identity-scoped MCP calls
// for the same agent can succeed without the caller threading the
// token through.
func (c *Client) rememberRegistrationToken(projectKey string, agent *Agent) {
	if c == nil || agent == nil || agent.RegistrationToken == "" || agent.Name == "" {
		return
	}
	c.SetRegistrationToken(projectKey, agent.Name, agent.RegistrationToken)
}

// Option configures the Client.
type Option func(*Client)

// WithBaseURL sets the Agent Mail server base URL.
func WithBaseURL(url string) Option {
	return func(c *Client) {
		if !strings.HasSuffix(url, "/") {
			url += "/"
		}
		c.baseURL = url
	}
}

// WithToken sets the bearer token for authentication.
func WithToken(token string) Option {
	return func(c *Client) {
		c.bearerToken = token
	}
}

// WithHTTPClient sets a custom HTTP client.
func WithHTTPClient(client *http.Client) Option {
	return func(c *Client) {
		if client != nil {
			c.httpClient = client
		}
	}
}

// WithProjectKey sets the default project key (working directory path).
func WithProjectKey(key string) Option {
	return func(c *Client) {
		c.projectKey = key
	}
}

// WithDisabled makes the client answer every call with ErrDisabled without
// contacting the server, for configurations that turn Agent Mail off. Its
// availability probe reports unavailable with ErrDisabled as the reason.
func WithDisabled() Option {
	return func(c *Client) {
		c.disabled = true
	}
}

// WithTimeout sets the default timeout for HTTP requests.
func WithTimeout(timeout time.Duration) Option {
	return func(c *Client) {
		if c.httpClient != nil {
			c.httpClient.Timeout = timeout
		}
	}
}

// NewClient creates a new Agent Mail client with the given options.
func NewClient(opts ...Option) *Client {
	c := &Client{
		baseURL: DefaultBaseURL,
		httpClient: &http.Client{
			Timeout: DefaultTimeout,
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				MaxIdleConns:        10,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}

	// Check environment variables
	if token := os.Getenv("AGENT_MAIL_TOKEN"); token != "" {
		c.bearerToken = token
	}
	if baseURL := os.Getenv("AGENT_MAIL_URL"); baseURL != "" {
		c.baseURL = baseURL
	}

	// Ensure base URL ends with /
	if !strings.HasSuffix(c.baseURL, "/") {
		c.baseURL += "/"
	}

	// Apply options
	for _, opt := range opts {
		opt(c)
	}

	return c
}

// IsAvailable checks if the Agent Mail server is reachable.
// Results are cached for 30 seconds to avoid repeated health checks.
func (c *Client) IsAvailable() bool {
	return c.IsAvailableContext(context.Background())
}

// IsAvailableContext reports whether the Agent Mail server answers an MCP ping
// at the configured endpoint, honoring caller cancellation. It deliberately
// does not call the health_check tool: that tool runs the server's full
// diagnostics (integrity, archive parity, pool pressure) and takes seconds on a
// busy server, which made a live server look unavailable to every caller.
// IsAvailable remains the entry point for callers without a lifecycle context.
func (c *Client) IsAvailableContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	if ctx.Err() != nil {
		return false
	}
	// Optimistic check (lock-free)
	cacheTime := c.availableCacheTime.Load()
	if cacheTime > 0 && time.Now().Unix()-cacheTime < int64(AvailabilityCacheTTL.Seconds()) {
		return c.availableCache.Load()
	}

	// One deadline covers lock contention, every attempt, and every backoff.
	// Caller cancellation exits without changing either the cache or the last
	// completed diagnostic.
	probeCtx, cancelProbe := context.WithTimeout(ctx, availabilityProbeBudget)
	defer cancelProbe()

	// Acquire the health-check lock without losing caller cancellation while a
	// concurrent probe is in flight. A plain Mutex.Lock would make the context
	// API wait for that unrelated probe even after its own deadline expired.
	for !c.healthCheckMu.TryLock() {
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-probeCtx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
	defer c.healthCheckMu.Unlock()
	if ctx.Err() != nil {
		return false
	}

	// Double-check after acquiring lock
	cacheTime = c.availableCacheTime.Load()
	if cacheTime > 0 && time.Now().Unix()-cacheTime < int64(AvailabilityCacheTTL.Seconds()) {
		return c.availableCache.Load()
	}
	if probeCtx.Err() != nil {
		return false
	}

	var err error
	available := false
probeLoop:
	for attempt := 1; attempt <= availabilityProbeAttempts; attempt++ {
		err = c.ping(probeCtx)
		if ctx.Err() != nil {
			return false
		}
		if err == nil {
			available = true
			break
		}
		if !isRetryableAvailabilityError(err) || attempt == availabilityProbeAttempts || probeCtx.Err() != nil {
			break
		}

		timer := time.NewTimer(availabilityProbeBackoff)
		if !waitForAvailabilityRetry(ctx, probeCtx, timer) {
			if ctx.Err() != nil {
				return false
			}
			break probeLoop
		}
	}
	if ctx.Err() != nil {
		return false
	}
	if available {
		c.lastAvailabilityErr.Store(availabilityErrBox{})
	} else {
		c.lastAvailabilityErr.Store(availabilityErrBox{err: err})
	}

	// Cache the result
	c.availableCache.Store(available)
	if available {
		c.availableCacheTime.Store(time.Now().Unix())
	} else {
		// Only cache failures for 2 seconds to allow quick recovery
		c.availableCacheTime.Store(time.Now().Unix() - int64(AvailabilityCacheTTL.Seconds()) + 2)
	}

	return available
}

func waitForAvailabilityRetry(ctx, probeCtx context.Context, timer *time.Timer) bool {
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-probeCtx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// isRetryableAvailabilityError limits availability retries to transport,
// timeout, busy, and explicitly transient HTTP failures. Protocol, decode,
// authentication, and other client errors fail immediately.
func isRetryableAvailabilityError(err error) bool {
	if errors.Is(err, ErrServerUnavailable) || errors.Is(err, ErrTimeout) || errors.Is(err, ErrTransientBusy) {
		return true
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.StatusCode {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		return true
	default:
		return apiErr.StatusCode >= http.StatusInternalServerError && apiErr.StatusCode <= 599
	}
}

func responseBodyReadError(ctx context.Context, operation string, err error) error {
	if ctx.Err() != nil {
		return NewAPIError(operation, 0, ErrTimeout)
	}
	return NewAPIError(operation, 0, fmt.Errorf("%w: reading response body: %w", ErrServerUnavailable, err))
}

// InvalidateCache clears the availability cache, forcing the next IsAvailable() call
// to perform a fresh health check.
func (c *Client) InvalidateCache() {
	c.availableCacheTime.Store(0)
}

// LastAvailabilityError returns the terminal error of the most recent
// completed availability probe, or nil when the last probe succeeded (or no
// probe has completed yet). Health and diagnostics surfaces use it to report
// WHY Agent Mail is considered unavailable.
func (c *Client) LastAvailabilityError() error {
	if c == nil {
		return nil
	}
	box, ok := c.lastAvailabilityErr.Load().(availabilityErrBox)
	if !ok {
		return nil
	}
	return box.err
}

// DefaultArchivePath is the default location for the Agent Mail archive.
const DefaultArchivePath = ".mcp_agent_mail_git_mailbox_repo"

// HasArchive checks if the Agent Mail archive directory exists.
// This provides a fallback detection method when the HTTP endpoint isn't available
// but Agent Mail is running via MCP stdio protocol.
func HasArchive() bool {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	archivePath := filepath.Join(homeDir, DefaultArchivePath)
	info, err := os.Stat(archivePath)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// HasArchiveForProject checks if the Agent Mail archive has data for a specific project.
func HasArchiveForProject(projectKey string) bool {
	if !HasArchive() {
		return false
	}
	homeDir, _ := os.UserHomeDir()
	for _, slug := range []string{ProjectSlugFromPath(projectKey), legacyProjectSlugFromPath(projectKey)} {
		if slug == "" {
			continue
		}
		projectPath := filepath.Join(homeDir, DefaultArchivePath, "projects", slug)
		info, err := os.Stat(projectPath)
		if err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

// HealthCheck runs the server's health_check tool and returns its report. The
// tool performs full diagnostics, so use IsAvailableContext for reachability.
func (c *Client) HealthCheck(ctx context.Context) (*HealthStatus, error) {
	result, err := c.callTool(ctx, "health_check", nil)
	if err != nil {
		return nil, err
	}

	var status HealthStatus
	if err := json.Unmarshal(result, &status); err != nil {
		return nil, NewAPIError("health_check", 0, err)
	}

	return &status, nil
}

// ProjectKey returns the configured project key.
func (c *Client) ProjectKey() string {
	return c.projectKey
}

// SetProjectKey sets the project key.
func (c *Client) SetProjectKey(key string) {
	c.projectKey = key
}

// BaseURL returns the configured Agent Mail base URL.
func (c *Client) BaseURL() string {
	return c.baseURL
}

// JSONRPCRequest represents a JSON-RPC 2.0 request.
type JSONRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
}

// JSONRPCResponse represents a JSON-RPC 2.0 response.
type JSONRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *JSONRPCError   `json:"error,omitempty"`
}

// MCPToolResult represents the MCP tools/call result envelope.
// MCP wraps tool outputs in this structure rather than returning raw data.
type MCPToolResult struct {
	// Content is an array of content blocks (text, image, etc.)
	Content []MCPContentBlock `json:"content,omitempty"`
	// StructuredContent contains the parsed tool output (preferred)
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	// IsError indicates if the tool returned an error
	IsError bool `json:"isError,omitempty"`
}

// MCPContentBlock represents a single content block in an MCP response.
type MCPContentBlock struct {
	Type string `json:"type"` // "text", "image", etc.
	Text string `json:"text,omitempty"`
}

// extractMCPContent extracts the actual tool output from an MCP envelope.
// It prefers structuredContent (already parsed), falls back to content[0].text
// (JSON string), and finally returns the raw result if not an MCP envelope.
func extractMCPContent(result json.RawMessage) (json.RawMessage, error) {
	if len(result) == 0 {
		return result, nil
	}

	// Try to parse as MCP envelope
	var envelope MCPToolResult
	if err := json.Unmarshal(result, &envelope); err != nil {
		// Not an MCP envelope, return raw result (backward compatibility)
		return result, nil
	}

	// Check for tool-level error
	if envelope.IsError {
		// Extract error message from content if available
		if len(envelope.Content) > 0 && envelope.Content[0].Text != "" {
			errMsg := envelope.Content[0].Text
			msgLower := strings.ToLower(errMsg)
			// Detect transient busy errors so callers can retry
			if strings.Contains(msgLower, "busy") || strings.Contains(msgLower, "temporarily unavailable") {
				return nil, fmt.Errorf("%w: %s", ErrTransientBusy, errMsg)
			}
			return nil, fmt.Errorf("tool error: %s", errMsg)
		}
		return nil, fmt.Errorf("tool returned error")
	}

	// Prefer structuredContent (already parsed JSON)
	if len(envelope.StructuredContent) > 0 {
		return envelope.StructuredContent, nil
	}

	// Fall back to content[0].text (JSON string that needs no re-parsing)
	if len(envelope.Content) > 0 && envelope.Content[0].Type == "text" && envelope.Content[0].Text != "" {
		// The text field contains JSON, return it as RawMessage
		return json.RawMessage(envelope.Content[0].Text), nil
	}

	// No structured content and no text content - might not be an MCP envelope
	// Check if it looks like raw tool output (has expected fields)
	// Return raw result for backward compatibility
	return result, nil
}

// ToolCallParams represents the params for a tools/call request.
type ToolCallParams struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

// callTool makes a JSON-RPC call to the Agent Mail server.
func (c *Client) callTool(ctx context.Context, toolName string, args map[string]interface{}) (json.RawMessage, error) {
	result, err := c.rpc(ctx, toolName, "tools/call", ToolCallParams{
		Name:      toolName,
		Arguments: args,
	})
	if err != nil {
		return nil, err
	}

	// Extract actual content from MCP envelope
	content, err := extractMCPContent(result)
	if err != nil {
		return nil, NewAPIError(toolName, 0, err)
	}

	return content, nil
}

// ping sends the MCP ping request, which servers answer without touching their
// database. A server that answers with "method not found" is up but predates
// ping, so it counts as reachable too.
func (c *Client) ping(ctx context.Context) error {
	_, err := c.rpc(ctx, "ping", "ping", map[string]interface{}{})
	if errors.Is(err, ErrNotImplemented) {
		return nil
	}
	return err
}

// rpc posts one JSON-RPC request to the MCP endpoint and returns its raw
// result. operation names the call in returned errors.
func (c *Client) rpc(ctx context.Context, operation, method string, params interface{}) (json.RawMessage, error) {
	if c.disabled {
		return nil, NewAPIError(operation, 0, ErrDisabled)
	}
	rpcReq := JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      c.requestID.Add(1),
		Method:  method,
		Params:  params,
	}

	body, err := json.Marshal(rpcReq)
	if err != nil {
		return nil, NewAPIError(operation, 0, err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL, bytes.NewReader(body))
	if err != nil {
		return nil, NewAPIError(operation, 0, err)
	}

	req.Header.Set("Content-Type", "application/json")
	if c.bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearerToken)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, NewAPIError(operation, 0, ErrTimeout)
		}
		return nil, NewAPIError(operation, 0, ErrServerUnavailable)
	}
	defer resp.Body.Close()

	// Read response body (limit to 10MB to prevent DoS/OOM)
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, responseBodyReadError(ctx, operation, err)
	}

	// Check HTTP status
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, NewAPIError(operation, resp.StatusCode, ErrUnauthorized)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, NewAPIError(operation, resp.StatusCode, fmt.Errorf("unexpected status: %s", resp.Status))
	}

	// Parse JSON-RPC response
	var rpcResp JSONRPCResponse
	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		return nil, NewAPIError(operation, 0, err)
	}

	// Check for JSON-RPC error
	if rpcResp.Error != nil {
		return nil, NewAPIError(operation, 0, mapJSONRPCError(rpcResp.Error))
	}

	return rpcResp.Result, nil
}

// callToolWithTimeout calls a tool with a specific timeout.
func (c *Client) callToolWithTimeout(ctx context.Context, toolName string, args map[string]interface{}, timeout time.Duration) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return c.callTool(ctx, toolName, args)
}

// callToolWithBusyRetry wraps callTool with retry logic for ErrTransientBusy errors.
// It retries up to maxRetries times with exponential backoff (500ms, 1s, 2s).
func (c *Client) callToolWithBusyRetry(ctx context.Context, toolName string, args map[string]interface{}, perCallTimeout time.Duration, maxRetries int) (json.RawMessage, error) {
	backoff := 500 * time.Millisecond
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, perCallTimeout)
		result, err := c.callTool(callCtx, toolName, args)
		cancel()
		if err == nil {
			return result, nil
		}
		lastErr = err
		if !IsTransientBusy(err) {
			return nil, err
		}
		// Transient busy — wait before retrying (unless this was the last attempt)
		if attempt < maxRetries {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}
	}
	return nil, lastErr
}

// ReadResource reads a resource from the Agent Mail server.
func (c *Client) ReadResource(ctx context.Context, uri string) (json.RawMessage, error) {
	return c.rpc(ctx, "resources/read", "resources/read", map[string]string{
		"uri": uri,
	})
}

// InboxPeekConcurrency bounds how many inboxes one client's PeekInboxes calls
// read from Agent Mail at once. On the live server 45 inboxes took 57.5 s one
// at a time, 13.3 s eight at a time and 21.6 s sixteen at a time, since more
// slows the server every other agent shares (bd-jv2dq).
const InboxPeekConcurrency = 8

// InboxPeek is one agent's inbox as PeekInboxes read it.
type InboxPeek struct {
	Messages []InboxMessage
	// Err is why the inbox was not read: the fetch_inbox failure, or ctx's
	// error when ctx ended before the read started.
	Err error
}

// PeekInboxes reads each named agent's inbox without marking anything read,
// at most InboxPeekConcurrency at a time across every caller sharing this
// client. Observers peek so they never consume other agents' mail
// (bd-ntu8w). opts supplies every field but AgentName and Peek. The result
// holds an entry for every name, so a caller can tell an inbox it could not
// read from an empty one.
func (c *Client) PeekInboxes(ctx context.Context, opts FetchInboxOptions, agentNames []string) map[string]InboxPeek {
	c.inboxPeekSlotsOnce.Do(func() {
		c.inboxPeekSlots = make(chan struct{}, InboxPeekConcurrency)
	})

	peeks := make([]InboxPeek, len(agentNames))
	var wg sync.WaitGroup
	for i, name := range agentNames {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case c.inboxPeekSlots <- struct{}{}:
			case <-ctx.Done():
				peeks[i].Err = ctx.Err()
				return
			}
			defer func() { <-c.inboxPeekSlots }()
			agentOpts := opts
			agentOpts.AgentName = name
			agentOpts.Peek = true
			peeks[i].Messages, peeks[i].Err = c.FetchInbox(ctx, agentOpts)
		}()
	}
	wg.Wait()

	byAgent := make(map[string]InboxPeek, len(agentNames))
	for i, name := range agentNames {
		byAgent[name] = peeks[i]
	}
	return byAgent
}

// httpBaseURL returns the HTTP REST API base URL derived from the MCP base URL.
// The MCP endpoint is typically at /mcp/ while the HTTP endpoints are at the root.
// Example: "http://127.0.0.1:8765/mcp/" -> "http://127.0.0.1:8765"
func (c *Client) httpBaseURL() string {
	base := c.baseURL
	// Remove trailing /mcp/ or /mcp if present
	if strings.HasSuffix(base, "/mcp/") {
		return strings.TrimSuffix(base, "/mcp/")
	}
	if strings.HasSuffix(base, "/mcp") {
		return strings.TrimSuffix(base, "/mcp")
	}
	// Remove trailing slash
	return strings.TrimSuffix(base, "/")
}

// ProjectSlugFromPath derives a project slug from an absolute path.
// This matches the logic in the Agent Mail server.
// Example: "/Users/jemanuel/projects/ntm" -> "users-jemanuel-projects-ntm"
func ProjectSlugFromPath(path string) string {
	if path == "" {
		return ""
	}

	cleaned := filepath.Clean(path)
	if cleaned == "." || cleaned == string(filepath.Separator) {
		return "root"
	}

	var sb strings.Builder
	lastWasDash := false
	for _, r := range cleaned {
		switch {
		case r == filepath.Separator || r == '/' || r == '\\' || unicode.IsSpace(r):
			if sb.Len() > 0 && !lastWasDash {
				sb.WriteByte('-')
				lastWasDash = true
			}
		case unicode.IsLetter(r) || unicode.IsNumber(r) || r == '-' || r == '_':
			sb.WriteRune(unicode.ToLower(r))
			lastWasDash = false
		default:
			if sb.Len() > 0 && !lastWasDash {
				sb.WriteByte('-')
				lastWasDash = true
			}
		}
	}

	return strings.Trim(sb.String(), "-")
}

func legacyProjectSlugFromPath(path string) string {
	if path == "" {
		return ""
	}

	slug := filepath.Base(filepath.Clean(path))
	if slug == "." || slug == "/" {
		return "root"
	}

	var sb strings.Builder
	for _, r := range slug {
		r = unicode.ToLower(r)
		if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '-' || r == '_' {
			sb.WriteRune(r)
		} else if r == ' ' {
			sb.WriteRune('_')
		}
	}
	return sb.String()
}
