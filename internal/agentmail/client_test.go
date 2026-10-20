package agentmail

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type deadlineObservedContext struct {
	context.Context
	observed chan struct{}
	once     sync.Once
}

type doneObservedContext struct {
	context.Context
	observed chan struct{}
	once     sync.Once
}

func (c *doneObservedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.observed) })
	return c.Context.Done()
}

type readErrorCloser struct {
	err error
}

func (r readErrorCloser) Read([]byte) (int, error) { return 0, r.err }
func (readErrorCloser) Close() error               { return nil }

func (c *deadlineObservedContext) Deadline() (time.Time, bool) {
	c.once.Do(func() { close(c.observed) })
	return c.Context.Deadline()
}

func TestNewClient(t *testing.T) {
	// Test default configuration
	c := NewClient()
	if c.baseURL != DefaultBaseURL {
		t.Errorf("expected base URL %s, got %s", DefaultBaseURL, c.baseURL)
	}
	if c.httpClient == nil {
		t.Error("expected HTTP client to be initialized")
	}

	// Test with options
	customURL := "http://custom:8080/mcp/"
	c = NewClient(WithBaseURL(customURL), WithToken("test-token"))
	if c.baseURL != customURL {
		t.Errorf("expected base URL %s, got %s", customURL, c.baseURL)
	}
	if c.bearerToken != "test-token" {
		t.Errorf("expected token 'test-token', got %s", c.bearerToken)
	}
}

func TestHealthCheck(t *testing.T) {
	// Mock MCP JSON-RPC server for health_check tool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}

		var req JSONRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("failed to decode request: %v", err)
		}

		// Verify it's a health_check tool call
		params, ok := req.Params.(map[string]interface{})
		if !ok {
			t.Fatal("expected params to be a map")
		}
		if params["name"] != "health_check" {
			t.Errorf("expected health_check tool, got %v", params["name"])
		}

		// Return health status via JSON-RPC
		healthStatus := HealthStatus{
			Status:    "ok",
			Timestamp: time.Now().Format(time.RFC3339),
		}
		statusJSON, _ := json.Marshal(healthStatus)

		resp := JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  json.RawMessage(statusJSON),
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	status, err := c.HealthCheck(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status.Status != "ok" {
		t.Errorf("expected status 'ok', got %s", status.Status)
	}
}

func TestIsAvailable(t *testing.T) {
	if NewClient().IsAvailableContext(nil) {
		t.Fatal("nil-context availability probe reported success")
	}

	// Mock MCP JSON-RPC server for health_check
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req JSONRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("failed to decode request: %v", err)
		}

		// Return healthy status
		healthStatus := HealthStatus{Status: "ok"}
		statusJSON, _ := json.Marshal(healthStatus)

		resp := JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  json.RawMessage(statusJSON),
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	if !c.IsAvailable() {
		t.Error("expected IsAvailable to return true")
	}

	// Test unavailable server
	c = NewClient(WithBaseURL("http://localhost:1/"))
	if c.IsAvailable() {
		t.Error("expected IsAvailable to return false for unreachable server")
	}
}

// TestIsAvailableContextRetriesTransientProbeFailures guards the bounded
// availability retry: a remote Agent Mail server that hiccups for one or two
// probes must not be branded unavailable (and negatively cached) off a single
// failed health check.
func TestIsAvailableContextRetriesTransientProbeFailures(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n <= 2 {
			// Transient failure: refuse the first two probes.
			http.Error(w, "temporarily overloaded", http.StatusServiceUnavailable)
			return
		}
		var req JSONRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return
		}
		statusJSON, _ := json.Marshal(HealthStatus{Status: "ok"})
		_ = json.NewEncoder(w).Encode(JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: statusJSON})
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	if !c.IsAvailableContext(context.Background()) {
		t.Fatalf("transiently failing server was branded unavailable after %d probes", calls.Load())
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("expected 3 probes (2 transient failures + 1 success), got %d", got)
	}
	if err := c.LastAvailabilityError(); err != nil {
		t.Fatalf("successful availability must clear the last probe error, got %v", err)
	}
}

// TestIsAvailableContextProbesWithPing guards bd-izmjq: the availability probe
// must not run the health_check tool, which performs full server diagnostics
// and took 11-13 s on a busy server, so a live server always looked down.
func TestIsAvailableContextProbesWithPing(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req JSONRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		methods = append(methods, req.Method)
		mu.Unlock()
		if req.Method != "ping" {
			// A diagnostic call slower than the whole probe budget.
			time.Sleep(2 * availabilityProbeBudget)
		}
		_ = json.NewEncoder(w).Encode(JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{}`)})
	}))
	defer server.Close()

	client := NewClient(WithBaseURL(server.URL + "/"))
	started := time.Now()
	if !client.IsAvailableContext(context.Background()) {
		t.Fatalf("reachable server reported unavailable: %v", client.LastAvailabilityError())
	}
	if elapsed := time.Since(started); elapsed >= availabilityProbeBudget {
		t.Fatalf("availability probe took %v, want well under the %v budget", elapsed, availabilityProbeBudget)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(methods) != 1 || methods[0] != "ping" {
		t.Fatalf("availability probe sent %v, want exactly one ping", methods)
	}
}

// TestIsAvailableContextRetriesOverloadedServer covers the JSON-RPC rejection a
// saturated Agent Mail server sends while it sheds load: it is transient, so the
// bounded probe retries instead of declaring the server down.
func TestIsAvailableContextRetriesOverloadedServer(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req JSONRPCRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if calls.Add(1) == 1 {
			_ = json.NewEncoder(w).Encode(JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Error: &JSONRPCError{
				Code:    -32603,
				Message: "Server overloaded, too many concurrent requests (limit=128, method=ping)",
			}})
			return
		}
		_ = json.NewEncoder(w).Encode(JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{}`)})
	}))
	defer server.Close()

	client := NewClient(WithBaseURL(server.URL + "/"))
	if !client.IsAvailableContext(context.Background()) {
		t.Fatalf("overloaded-then-ready server reported unavailable: %v", client.LastAvailabilityError())
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected one overloaded rejection and one successful ping, got %d calls", got)
	}
}

func TestIsAvailableContextTreatsUnknownPingAsReachable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req JSONRPCRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.NewEncoder(w).Encode(JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Error: &JSONRPCError{
			Code:    -32601,
			Message: "Method not found: ping",
		}})
	}))
	defer server.Close()

	client := NewClient(WithBaseURL(server.URL + "/"))
	if !client.IsAvailableContext(context.Background()) {
		t.Fatalf("server answering JSON-RPC without ping support reported unavailable: %v", client.LastAvailabilityError())
	}
}

func TestIsAvailableContextRetriesResponseBodyTransportFailure(t *testing.T) {
	var calls atomic.Int32
	client := NewClient(
		WithBaseURL("http://agentmail.invalid/"),
		WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			call := calls.Add(1)
			body := io.ReadCloser(readErrorCloser{err: io.ErrUnexpectedEOF})
			if call > 1 {
				body = io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":2,"result":{"status":"ok"}}`))
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Body:       body,
				Header:     make(http.Header),
				Request:    req,
			}, nil
		})}),
	)

	if !client.IsAvailableContext(context.Background()) {
		t.Fatalf("availability did not recover after a transient response-body read failure: %v", client.LastAvailabilityError())
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("transient response-body read failure made %d calls, want one retry and one success", got)
	}
	if err := client.LastAvailabilityError(); err != nil {
		t.Fatalf("successful retry did not clear response-body diagnostic: %v", err)
	}
}

func TestAvailabilityProbeBudgetBoundsAllAttempts(t *testing.T) {
	if availabilityProbeBudget <= 0 || availabilityProbeBudget >= 2*time.Second {
		t.Fatalf("availability probe budget must be positive and below 2s, got %v", availabilityProbeBudget)
	}

	var calls atomic.Int32
	client := NewClient(
		WithBaseURL("http://agentmail.invalid/"),
		WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls.Add(1)
			<-req.Context().Done()
			return nil, req.Context().Err()
		})}),
	)

	started := time.Now()
	if client.IsAvailableContext(context.Background()) {
		t.Fatal("deadline-bound availability probe reported success")
	}
	elapsed := time.Since(started)
	if elapsed < availabilityProbeBudget-100*time.Millisecond {
		t.Fatalf("availability probe returned before its budget: elapsed=%v budget=%v", elapsed, availabilityProbeBudget)
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("availability probe exceeded the strict 2s bound: %v", elapsed)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("a hung attempt must consume the one overall budget; got %d calls", got)
	}
	if err := client.LastAvailabilityError(); !errors.Is(err, ErrTimeout) {
		t.Fatalf("budget exhaustion must retain the terminal timeout, got %v", err)
	}
}

func TestAvailabilityRetryClassification(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		retryable bool
	}{
		{name: "server unavailable", err: ErrServerUnavailable, retryable: true},
		{name: "timeout", err: ErrTimeout, retryable: true},
		{name: "transient busy", err: ErrTransientBusy, retryable: true},
		{name: "HTTP 408", err: NewAPIError("health_check", http.StatusRequestTimeout, errors.New("status")), retryable: true},
		{name: "HTTP 425", err: NewAPIError("health_check", http.StatusTooEarly, errors.New("status")), retryable: true},
		{name: "HTTP 429", err: NewAPIError("health_check", http.StatusTooManyRequests, errors.New("status")), retryable: true},
		{name: "HTTP 500", err: NewAPIError("health_check", http.StatusInternalServerError, errors.New("status")), retryable: true},
		{name: "HTTP 599", err: NewAPIError("health_check", 599, errors.New("status")), retryable: true},
		{name: "unauthorized", err: NewAPIError("health_check", http.StatusUnauthorized, ErrUnauthorized)},
		{name: "other HTTP 4xx", err: NewAPIError("health_check", http.StatusBadRequest, errors.New("status"))},
		{name: "permanent JSON-RPC", err: NewAPIError("health_check", 0, &JSONRPCError{Code: -32602, Message: "invalid params"})},
		{name: "malformed decode", err: NewAPIError("health_check", 0, errors.New("invalid character"))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetryableAvailabilityError(tt.err); got != tt.retryable {
				t.Fatalf("isRetryableAvailabilityError() = %t, want %t for %v", got, tt.retryable, tt.err)
			}
		})
	}
}

func TestIsAvailableContextPermanentFailuresAreOneShot(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
	}{
		{name: "unauthorized", statusCode: http.StatusUnauthorized},
		{name: "other HTTP 4xx", statusCode: http.StatusBadRequest},
		{name: "permanent JSON-RPC", statusCode: http.StatusOK, body: `{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"invalid params"}}`},
		{name: "malformed decode", statusCode: http.StatusOK, body: `{`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			client := NewClient(WithBaseURL(server.URL + "/"))
			if client.IsAvailableContext(context.Background()) {
				t.Fatal("permanent health failure reported available")
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("permanent failure must not be retried; got %d calls", got)
			}
			if err := client.LastAvailabilityError(); err == nil {
				t.Fatal("permanent health failure did not retain its diagnostic")
			}
		})
	}
}

func TestIsAvailableContextTransientExhaustionRetainsTerminalError(t *testing.T) {
	statuses := []int{http.StatusServiceUnavailable, http.StatusTooManyRequests, http.StatusTooEarly}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		call := int(calls.Add(1))
		http.Error(w, "transient", statuses[call-1])
	}))
	defer server.Close()

	client := NewClient(WithBaseURL(server.URL + "/"))
	if client.IsAvailableContext(context.Background()) {
		t.Fatal("transiently failing server reported available after retry exhaustion")
	}
	if got := calls.Load(); got != int32(availabilityProbeAttempts) {
		t.Fatalf("expected %d attempts, got %d", availabilityProbeAttempts, got)
	}
	var apiErr *APIError
	if err := client.LastAvailabilityError(); !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTooEarly {
		t.Fatalf("retry exhaustion must retain the terminal HTTP 425 error, got %v", err)
	}
}

func TestIsAvailableContextCancellationDuringBackoffPreservesState(t *testing.T) {
	firstResponse := make(chan struct{})
	var firstResponseOnce sync.Once
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "transient", http.StatusServiceUnavailable)
		firstResponseOnce.Do(func() { close(firstResponse) })
	}))
	defer server.Close()

	client := NewClient(WithBaseURL(server.URL + "/"))
	previousErr := NewAPIError("previous_probe", http.StatusBadGateway, ErrServerUnavailable)
	client.lastAvailabilityErr.Store(availabilityErrBox{err: previousErr})
	client.availableCache.Store(true)
	client.availableCacheTime.Store(0)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan bool, 1)
	go func() { result <- client.IsAvailableContext(ctx) }()
	select {
	case <-firstResponse:
	case <-time.After(time.Second):
		t.Fatal("first availability response did not complete")
	}
	cancel()

	select {
	case available := <-result:
		if available {
			t.Fatal("canceled availability probe reported success")
		}
	case <-time.After(time.Second):
		t.Fatal("availability probe did not return after backoff cancellation")
	}
	if got := calls.Load(); got < 1 || got > int32(availabilityProbeAttempts) {
		t.Fatalf("backoff cancellation made an invalid number of calls: %d", got)
	}
	if got := client.LastAvailabilityError(); got != previousErr {
		t.Fatalf("backoff cancellation overwrote the previous diagnostic: got %v want %v", got, previousErr)
	}
	if got := client.availableCacheTime.Load(); got != 0 {
		t.Fatalf("backoff cancellation populated the cache timestamp: %d", got)
	}
	if !client.availableCache.Load() {
		t.Fatal("backoff cancellation overwrote the previous cached value")
	}
}

func TestWaitForAvailabilityRetryHonorsCallerCancellation(t *testing.T) {
	baseCtx, cancel := context.WithCancel(context.Background())
	observed := make(chan struct{})
	ctx := &doneObservedContext{Context: baseCtx, observed: observed}
	result := make(chan bool, 1)
	go func() {
		result <- waitForAvailabilityRetry(ctx, context.Background(), time.NewTimer(time.Second))
	}()

	select {
	case <-observed:
	case <-time.After(time.Second):
		t.Fatal("availability retry did not begin waiting on caller cancellation")
	}
	cancel()

	select {
	case retry := <-result:
		if retry {
			t.Fatal("caller cancellation allowed another availability retry")
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("availability retry did not stop after caller cancellation")
	}
}

func TestIsAvailableContextInvalidatedFailureThenSuccessClearsDiagnostic(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		statusJSON, _ := json.Marshal(HealthStatus{Status: "ok"})
		_ = json.NewEncoder(w).Encode(JSONRPCResponse{JSONRPC: "2.0", ID: 2, Result: statusJSON})
	}))
	defer server.Close()

	client := NewClient(WithBaseURL(server.URL + "/"))
	if client.IsAvailableContext(context.Background()) {
		t.Fatal("initial real failure reported available")
	}
	if err := client.LastAvailabilityError(); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("initial failure diagnostic = %v, want unauthorized", err)
	}

	client.InvalidateCache()
	if !client.IsAvailableContext(context.Background()) {
		t.Fatal("fresh successful probe after cache invalidation reported unavailable")
	}
	if err := client.LastAvailabilityError(); err != nil {
		t.Fatalf("successful probe did not clear the previous diagnostic: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected one failed and one successful probe, got %d calls", got)
	}
}

// TestIsAvailableContextSurfacesTerminalProbeError guards the diagnostics
// side: a hard-down server yields unavailable AND a retrievable reason.
func TestIsAvailableContextSurfacesTerminalProbeError(t *testing.T) {
	c := NewClient(WithBaseURL("http://localhost:1/"))
	if c.IsAvailableContext(context.Background()) {
		t.Fatal("unreachable server reported available")
	}
	if err := c.LastAvailabilityError(); err == nil {
		t.Fatal("terminal probe failure must surface a reason via LastAvailabilityError")
	}
}

func TestIsAvailableContextCancelsWhileAnotherHealthCheckOwnsLock(t *testing.T) {
	probeStarted := make(chan struct{}, 1)
	releaseProbe := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probeStarted <- struct{}{}
		<-releaseProbe
		var req JSONRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return
		}
		statusJSON, _ := json.Marshal(HealthStatus{Status: "ok"})
		_ = json.NewEncoder(w).Encode(JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: statusJSON})
	}))
	defer server.Close()

	client := NewClient(WithBaseURL(server.URL + "/"))
	firstDone := make(chan bool, 1)
	go func() { firstDone <- client.IsAvailableContext(context.Background()) }()
	select {
	case <-probeStarted:
	case <-time.After(time.Second):
		t.Fatal("first availability probe did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	started := time.Now()
	if client.IsAvailableContext(ctx) {
		t.Fatal("canceled availability waiter reported success")
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("availability waiter ignored cancellation for %v", elapsed)
	}

	close(releaseProbe)
	select {
	case available := <-firstDone:
		if !available {
			t.Fatal("owning availability probe reported unavailable")
		}
	case <-time.After(time.Second):
		t.Fatal("owning availability probe did not finish")
	}
}

func TestAvailabilityProbeBudgetIncludesLockContention(t *testing.T) {
	firstProbeStarted := make(chan struct{}, 1)
	var calls atomic.Int32
	client := NewClient(
		WithBaseURL("http://agentmail.invalid/"),
		WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if calls.Add(1) == 1 {
				firstProbeStarted <- struct{}{}
			}
			<-req.Context().Done()
			return nil, req.Context().Err()
		})}),
	)

	ownerCtx, cancelOwner := context.WithCancel(context.Background())
	ownerDone := make(chan bool, 1)
	go func() { ownerDone <- client.IsAvailableContext(ownerCtx) }()
	select {
	case <-firstProbeStarted:
	case <-time.After(time.Second):
		t.Fatal("owning availability probe did not start")
	}

	deadlineObserved := make(chan struct{})
	waiterCtx := &deadlineObservedContext{Context: context.Background(), observed: deadlineObserved}
	waiterDone := make(chan bool, 1)
	go func() { waiterDone <- client.IsAvailableContext(waiterCtx) }()
	select {
	case <-deadlineObserved:
	case <-time.After(500 * time.Millisecond):
		cancelOwner()
		t.Fatal("availability waiter did not establish its overall deadline before lock acquisition")
	}
	waiterStarted := time.Now()

	// Leave the waiter less than a fresh probe budget after the owner releases
	// the lock. A per-attempt deadline would therefore exceed the total bound.
	time.Sleep(availabilityProbeBudget - 350*time.Millisecond)
	cancelOwner()
	select {
	case available := <-ownerDone:
		if available {
			t.Fatal("canceled owning availability probe reported success")
		}
	case <-time.After(time.Second):
		t.Fatal("owning availability probe did not return after cancellation")
	}

	select {
	case available := <-waiterDone:
		if available {
			t.Fatal("deadline-bound availability waiter reported success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("availability waiter exceeded the strict two-second total budget")
	}
	if elapsed := time.Since(waiterStarted); elapsed >= 2*time.Second {
		t.Fatalf("availability waiter exceeded the strict two-second total budget: %v", elapsed)
	}
	if got := calls.Load(); got < 1 || got > 2 {
		t.Fatalf("availability contention made %d probes, want the owner plus at most one bounded waiter probe", got)
	}
}

func TestIsAvailableContextDoesNotCacheCanceledProbe(t *testing.T) {
	probeStarted := make(chan struct{}, 1)
	releaseProbe := make(chan struct{})
	var probes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if probes.Add(1) == 1 {
			probeStarted <- struct{}{}
			<-releaseProbe
			return
		}
		statusJSON, _ := json.Marshal(HealthStatus{Status: "ok"})
		_ = json.NewEncoder(w).Encode(JSONRPCResponse{JSONRPC: "2.0", ID: 1, Result: statusJSON})
	}))
	defer server.Close()
	defer close(releaseProbe)

	client := NewClient(WithBaseURL(server.URL + "/"))
	previousErr := NewAPIError("previous_probe", http.StatusBadGateway, ErrServerUnavailable)
	client.lastAvailabilityErr.Store(availabilityErrBox{err: previousErr})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan bool, 1)
	go func() { result <- client.IsAvailableContext(ctx) }()
	select {
	case <-probeStarted:
	case <-time.After(time.Second):
		t.Fatal("availability probe did not start")
	}
	cancel()
	select {
	case available := <-result:
		if available {
			t.Fatal("canceled availability probe reported success")
		}
	case <-time.After(time.Second):
		t.Fatal("availability probe did not return after cancellation")
	}
	if cacheTime := client.availableCacheTime.Load(); cacheTime != 0 {
		t.Fatalf("canceled availability probe populated cache timestamp %d", cacheTime)
	}
	if got := client.LastAvailabilityError(); got != previousErr {
		t.Fatalf("probe cancellation overwrote the previous diagnostic: got %v want %v", got, previousErr)
	}
	if !client.IsAvailableContext(t.Context()) {
		t.Fatal("live probe after cancellation was poisoned by canceled availability result")
	}
}

func TestCallTool(t *testing.T) {
	// Mock JSON-RPC server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}

		var req JSONRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("failed to decode request: %v", err)
		}

		if req.JSONRPC != "2.0" {
			t.Errorf("expected jsonrpc 2.0, got %s", req.JSONRPC)
		}
		if req.Method != "tools/call" {
			t.Errorf("expected method tools/call, got %s", req.Method)
		}

		// Return success response
		resp := JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  json.RawMessage(`{"id": 1, "name": "TestAgent"}`),
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	result, err := c.callTool(context.Background(), "test_tool", map[string]interface{}{"key": "value"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var data struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(result, &data); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if data.ID != 1 || data.Name != "TestAgent" {
		t.Errorf("unexpected result: %+v", data)
	}
}

func TestCallToolError(t *testing.T) {
	// Mock server that returns JSON-RPC error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      1,
			Error: &JSONRPCError{
				Code:    -32600,
				Message: "Invalid Request",
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	_, err := c.callTool(context.Background(), "test_tool", nil)
	if err == nil {
		t.Error("expected error, got nil")
	}
}

func TestUnauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	_, err := c.callTool(context.Background(), "test_tool", nil)
	if err == nil {
		t.Error("expected error, got nil")
	}
	if !IsUnauthorized(err) {
		t.Errorf("expected unauthorized error, got: %v", err)
	}
}

func TestProjectKey(t *testing.T) {
	c := NewClient(WithProjectKey("/test/project"))
	if c.ProjectKey() != "/test/project" {
		t.Errorf("expected /test/project, got %s", c.ProjectKey())
	}

	c.SetProjectKey("/new/project")
	if c.ProjectKey() != "/new/project" {
		t.Errorf("expected /new/project, got %s", c.ProjectKey())
	}
}

func TestJSONRPCError(t *testing.T) {
	err := &JSONRPCError{
		Code:    -32600,
		Message: "Invalid Request",
	}

	expected := "JSON-RPC error -32600: Invalid Request"
	if err.Error() != expected {
		t.Errorf("expected %q, got %q", expected, err.Error())
	}

	// With data
	err.Data = map[string]string{"field": "value"}
	if err.Error() == expected {
		t.Error("expected error message to include data")
	}
}

func TestAPIError(t *testing.T) {
	innerErr := ErrServerUnavailable
	err := NewAPIError("test_op", 503, innerErr)

	if err.Operation != "test_op" {
		t.Errorf("expected operation 'test_op', got %s", err.Operation)
	}
	if err.StatusCode != 503 {
		t.Errorf("expected status 503, got %d", err.StatusCode)
	}
	if err.Unwrap() != innerErr {
		t.Error("Unwrap should return the inner error")
	}
	if !IsServerUnavailable(err) {
		t.Error("expected IsServerUnavailable to return true")
	}
}

func TestErrorHelpers(t *testing.T) {
	tests := []struct {
		err    error
		check  func(error) bool
		expect bool
	}{
		{ErrServerUnavailable, IsServerUnavailable, true},
		{ErrUnauthorized, IsUnauthorized, true},
		{ErrNotFound, IsNotFound, true},
		{ErrInvalidRequest, IsInvalidRequest, true},
		{ErrTimeout, IsTimeout, true},
		{ErrNotImplemented, IsNotImplemented, true},
		{ErrReservationConflict, IsReservationConflict, true},
		{ErrContactBlocked, IsContactBlocked, true},
		{ErrServerUnavailable, IsUnauthorized, false},
		{NewAPIError("test", 0, ErrNotFound), IsNotFound, true},
		{NewAPIError("test", 0, ErrNotImplemented), IsNotImplemented, true},
	}

	for _, tt := range tests {
		result := tt.check(tt.err)
		if result != tt.expect {
			t.Errorf("for %v, expected %v, got %v", tt.err, tt.expect, result)
		}
	}
}

func TestExtractMCPContent(t *testing.T) {
	tests := []struct {
		name        string
		input       json.RawMessage
		wantErr     bool
		errContains string
		validate    func(t *testing.T, result json.RawMessage)
	}{
		{
			name:  "raw result (no envelope) - backward compatibility",
			input: json.RawMessage(`{"id": 1, "name": "TestAgent"}`),
			validate: func(t *testing.T, result json.RawMessage) {
				var data struct {
					ID   int    `json:"id"`
					Name string `json:"name"`
				}
				if err := json.Unmarshal(result, &data); err != nil {
					t.Fatalf("failed to unmarshal: %v", err)
				}
				if data.ID != 1 || data.Name != "TestAgent" {
					t.Errorf("unexpected data: %+v", data)
				}
			},
		},
		{
			name: "MCP envelope with structuredContent (preferred)",
			input: json.RawMessage(`{
				"content": [{"type": "text", "text": "{\"id\":99,\"name\":\"Ignored\"}"}],
				"structuredContent": {"id": 115, "name": "BrownOtter"},
				"isError": false
			}`),
			validate: func(t *testing.T, result json.RawMessage) {
				var data struct {
					ID   int    `json:"id"`
					Name string `json:"name"`
				}
				if err := json.Unmarshal(result, &data); err != nil {
					t.Fatalf("failed to unmarshal: %v", err)
				}
				if data.ID != 115 || data.Name != "BrownOtter" {
					t.Errorf("expected {115, BrownOtter}, got %+v", data)
				}
			},
		},
		{
			name: "MCP envelope with content text (fallback)",
			input: json.RawMessage(`{
				"content": [{"type": "text", "text": "{\"id\":42,\"name\":\"GreenLake\"}"}],
				"isError": false
			}`),
			validate: func(t *testing.T, result json.RawMessage) {
				var data struct {
					ID   int    `json:"id"`
					Name string `json:"name"`
				}
				if err := json.Unmarshal(result, &data); err != nil {
					t.Fatalf("failed to unmarshal: %v", err)
				}
				if data.ID != 42 || data.Name != "GreenLake" {
					t.Errorf("expected {42, GreenLake}, got %+v", data)
				}
			},
		},
		{
			name: "MCP envelope with isError=true and message",
			input: json.RawMessage(`{
				"content": [{"type": "text", "text": "Agent name already in use"}],
				"isError": true
			}`),
			wantErr:     true,
			errContains: "Agent name already in use",
		},
		{
			name: "MCP envelope with isError=true no message",
			input: json.RawMessage(`{
				"content": [],
				"isError": true
			}`),
			wantErr:     true,
			errContains: "tool returned error",
		},
		{
			name:  "empty result",
			input: json.RawMessage(``),
			validate: func(t *testing.T, result json.RawMessage) {
				if len(result) != 0 {
					t.Errorf("expected empty result, got %s", string(result))
				}
			},
		},
		{
			name:  "null result",
			input: json.RawMessage(`null`),
			validate: func(t *testing.T, result json.RawMessage) {
				// null is valid JSON, should pass through
				if string(result) != "null" {
					t.Errorf("expected null, got %s", string(result))
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := extractMCPContent(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("error %q should contain %q", err.Error(), tt.errContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.validate != nil {
				tt.validate(t, result)
			}
		})
	}
}

func TestCallToolWithMCPEnvelope(t *testing.T) {
	// Mock server that returns MCP envelope format
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      1,
			// MCP envelope with structuredContent
			Result: json.RawMessage(`{
				"content": [{"type": "text", "text": "{\"id\":115,\"name\":\"BrownOtter\"}"}],
				"structuredContent": {"id": 115, "name": "BrownOtter", "program": "ntm", "model": "coordinator"},
				"isError": false
			}`),
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL + "/"))
	result, err := c.callTool(context.Background(), "register_agent", map[string]interface{}{
		"project_key": "/test/project",
		"program":     "ntm",
		"model":       "coordinator",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify we got the extracted structuredContent, not the envelope
	var agent struct {
		ID      int    `json:"id"`
		Name    string `json:"name"`
		Program string `json:"program"`
		Model   string `json:"model"`
	}
	if err := json.Unmarshal(result, &agent); err != nil {
		t.Fatalf("failed to unmarshal agent: %v", err)
	}
	if agent.ID != 115 {
		t.Errorf("expected ID 115, got %d", agent.ID)
	}
	if agent.Name != "BrownOtter" {
		t.Errorf("expected name BrownOtter, got %s", agent.Name)
	}
	if agent.Program != "ntm" {
		t.Errorf("expected program ntm, got %s", agent.Program)
	}
}

func TestFlexTime_UnmarshalJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		input       string
		want        time.Time
		wantErr     bool
		wantUTCOnly bool
	}{
		{
			name:        "rfc3339",
			input:       `"2026-01-31T01:24:27Z"`,
			want:        time.Date(2026, 1, 31, 1, 24, 27, 0, time.UTC),
			wantUTCOnly: true,
		},
		{
			name:        "rfc3339_nano",
			input:       `"2026-01-31T01:24:27.123456789Z"`,
			want:        time.Date(2026, 1, 31, 1, 24, 27, 123456789, time.UTC),
			wantUTCOnly: true,
		},
		{
			name:        "bare_seconds_assume_utc",
			input:       `"2026-01-31T01:24:27"`,
			want:        time.Date(2026, 1, 31, 1, 24, 27, 0, time.UTC),
			wantUTCOnly: true,
		},
		{
			name:        "bare_millis_assume_utc",
			input:       `"2026-01-31T01:24:27.123"`,
			want:        time.Date(2026, 1, 31, 1, 24, 27, 123000000, time.UTC),
			wantUTCOnly: true,
		},
		{
			name:  "empty_string_sets_zero",
			input: `""`,
			want:  time.Time{},
		},
		{
			name:    "invalid_format",
			input:   `"not-a-timestamp"`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ft FlexTime
			err := json.Unmarshal([]byte(tt.input), &ft)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if !ft.Time.Equal(tt.want) {
				t.Fatalf("expected %s, got %s", tt.want.Format(time.RFC3339Nano), ft.Time.Format(time.RFC3339Nano))
			}
			if tt.wantUTCOnly && ft.Time.Location() != time.UTC {
				t.Fatalf("expected UTC, got %s", ft.Time.Location())
			}
		})
	}
}

func TestFlexTime_MarshalJSON(t *testing.T) {
	t.Parallel()

	ft := FlexTime{Time: time.Date(2026, 1, 31, 1, 24, 27, 123456789, time.UTC)}
	got, err := json.Marshal(ft)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != `"2026-01-31T01:24:27.123456789Z"` {
		t.Fatalf("unexpected JSON: %s", string(got))
	}
}

func TestReservationConflict_UnmarshalJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		input       string
		wantPath    string
		wantHolders []string
		wantErr     bool
	}{
		{
			name:        "null_holders_becomes_empty_slice",
			input:       `{"path":"internal/agentmail/*","holders":null}`,
			wantPath:    "internal/agentmail/*",
			wantHolders: []string{},
		},
		{
			name:        "legacy_list_of_names",
			input:       `{"path":"x","holders":["BlueLake","RedStone"]}`,
			wantPath:    "x",
			wantHolders: []string{"BlueLake", "RedStone"},
		},
		{
			name:        "current_list_of_objects",
			input:       `{"path":"x","holders":[{"agent":"BlueLake"},{"agent_name":"RedStone"},{"agent":"","agent_name":"GreenCastle"}]}`,
			wantPath:    "x",
			wantHolders: []string{"BlueLake", "RedStone", "GreenCastle"},
		},
		{
			name:    "unsupported_format_errors",
			input:   `{"path":"x","holders":123}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var c ReservationConflict
			err := json.Unmarshal([]byte(tt.input), &c)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if c.Path != tt.wantPath {
				t.Fatalf("expected path %q, got %q", tt.wantPath, c.Path)
			}
			if !reflect.DeepEqual(c.Holders, tt.wantHolders) {
				t.Fatalf("expected holders %#v, got %#v", tt.wantHolders, c.Holders)
			}
		})
	}
}

// =============================================================================
// Client option and method tests for coverage
// =============================================================================

func TestInvalidateCache(t *testing.T) {
	t.Parallel()

	c := NewClient()
	// Manually set cache time to simulate a cached result
	c.availableCacheTime.Store(time.Now().Unix())
	c.availableCache.Store(true)

	// Verify cache was set
	if c.availableCacheTime.Load() == 0 {
		t.Fatal("expected cache time to be set")
	}

	// Invalidate
	c.InvalidateCache()

	// Verify cache was cleared
	if c.availableCacheTime.Load() != 0 {
		t.Error("expected cache time to be cleared after InvalidateCache")
	}
}

func TestBaseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		inputURL string
		wantURL  string
	}{
		{"default URL", "", DefaultBaseURL},
		{"custom URL with trailing slash", "http://custom:8080/mcp/", "http://custom:8080/mcp/"},
		{"custom URL without trailing slash", "http://custom:8080/mcp", "http://custom:8080/mcp/"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var c *Client
			if tc.inputURL == "" {
				c = NewClient()
			} else {
				c = NewClient(WithBaseURL(tc.inputURL))
			}
			got := c.BaseURL()
			if got != tc.wantURL {
				t.Errorf("BaseURL() = %q, want %q", got, tc.wantURL)
			}
		})
	}
}

func TestWithHTTPClient(t *testing.T) {
	t.Parallel()

	customClient := &http.Client{Timeout: 5 * time.Minute}
	c := NewClient(WithHTTPClient(customClient))

	if c.httpClient != customClient {
		t.Error("expected custom HTTP client to be set")
	}
}

func TestWithHTTPClientNilKeepsDefaultClient(t *testing.T) {
	t.Parallel()

	c := NewClient(WithHTTPClient(nil))
	if c.httpClient == nil {
		t.Fatal("WithHTTPClient(nil) left client without an HTTP transport")
	}
	if c.httpClient.Timeout != DefaultTimeout {
		t.Errorf("default HTTP timeout = %v, want %v", c.httpClient.Timeout, DefaultTimeout)
	}
}

func TestWithTimeout(t *testing.T) {
	t.Parallel()

	// WithTimeout modifies an existing httpClient's timeout
	customClient := &http.Client{Timeout: 10 * time.Second}
	c := NewClient(WithHTTPClient(customClient), WithTimeout(60*time.Second))

	if c.httpClient.Timeout != 60*time.Second {
		t.Errorf("expected timeout 60s, got %v", c.httpClient.Timeout)
	}
}

func TestHttpBaseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		baseURL string
		want    string
	}{
		{"MCP URL with trailing slash", "http://127.0.0.1:8765/mcp/", "http://127.0.0.1:8765"},
		{"MCP URL without trailing slash", "http://127.0.0.1:8765/mcp", "http://127.0.0.1:8765"},
		{"non-MCP URL with trailing slash", "http://example.com/api/", "http://example.com/api"},
		{"non-MCP URL without trailing slash", "http://example.com/api", "http://example.com/api"},
		{"root URL with trailing slash", "http://localhost:8000/", "http://localhost:8000"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := NewClient(WithBaseURL(tc.baseURL))
			got := c.httpBaseURL()
			if got != tc.want {
				t.Errorf("httpBaseURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFetchInbox_UnmarshalFormats(t *testing.T) {
	t.Parallel()

	makeServer := func(t *testing.T, rawResult json.RawMessage) *httptest.Server {
		t.Helper()
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req JSONRPCRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("failed to decode request: %v", err)
			}
			if req.Method != "tools/call" {
				t.Fatalf("expected tools/call, got %s", req.Method)
			}

			params, ok := req.Params.(map[string]interface{})
			if !ok {
				t.Fatal("expected params to be a map")
			}
			if params["name"] != "fetch_inbox" {
				t.Fatalf("expected tool fetch_inbox, got %v", params["name"])
			}

			args, _ := params["arguments"].(map[string]interface{})
			if args["project_key"] != "/test/project" {
				t.Fatalf("expected project_key /test/project, got %v", args["project_key"])
			}
			if args["agent_name"] != "BlueLake" {
				t.Fatalf("expected agent_name BlueLake, got %v", args["agent_name"])
			}
			if args["include_bodies"] != true {
				t.Fatalf("expected include_bodies true, got %v", args["include_bodies"])
			}

			resp := JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result:  rawResult,
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		}))
	}

	t.Run("wrapper_object", func(t *testing.T) {
		server := makeServer(t, json.RawMessage(`{"result":[{"id":1,"subject":"Hello","from":"alice","created_ts":"2026-01-01T00:00:00Z","importance":"normal","ack_required":false,"kind":"to","body_md":"hi"}]}`))
		defer server.Close()

		c := NewClient(WithBaseURL(server.URL + "/"))
		msgs, err := c.FetchInbox(context.Background(), FetchInboxOptions{
			ProjectKey:    "/test/project",
			AgentName:     "BlueLake",
			IncludeBodies: true,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(msgs) != 1 || msgs[0].ID != 1 || msgs[0].From != "alice" {
			t.Fatalf("unexpected messages: %+v", msgs)
		}
	})

	t.Run("direct_array", func(t *testing.T) {
		server := makeServer(t, json.RawMessage(`[{"id":2,"subject":"Yo","from":"bob","created_ts":"2026-01-01T00:00:00Z","importance":"normal","ack_required":false,"kind":"to","body_md":"hey"}]`))
		defer server.Close()

		c := NewClient(WithBaseURL(server.URL + "/"))
		msgs, err := c.FetchInbox(context.Background(), FetchInboxOptions{
			ProjectKey:    "/test/project",
			AgentName:     "BlueLake",
			IncludeBodies: true,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(msgs) != 1 || msgs[0].ID != 2 || msgs[0].From != "bob" {
			t.Fatalf("unexpected messages: %+v", msgs)
		}
	})

	t.Run("schema_drift_is_error", func(t *testing.T) {
		server := makeServer(t, json.RawMessage(`{"messages":[]}`))
		defer server.Close()

		c := NewClient(WithBaseURL(server.URL + "/"))
		if _, err := c.FetchInbox(context.Background(), FetchInboxOptions{
			ProjectKey:    "/test/project",
			AgentName:     "BlueLake",
			IncludeBodies: true,
		}); err == nil {
			t.Fatal("expected schema drift to return an error")
		}
	})
}

// peekInboxServer answers fetch_inbox with one message per agent after delay,
// fails the agent named "Broken" with a tool error, and records every call's
// arguments and the most calls it held at once.
type peekInboxServer struct {
	*httptest.Server
	mu          sync.Mutex
	inFlight    int
	maxInFlight int
	calls       []map[string]interface{}
}

func newPeekInboxServer(t *testing.T, delay time.Duration) *peekInboxServer {
	t.Helper()
	s := &peekInboxServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req JSONRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		params, _ := req.Params.(map[string]interface{})
		args, _ := params["arguments"].(map[string]interface{})
		s.mu.Lock()
		s.calls = append(s.calls, args)
		s.inFlight++
		s.maxInFlight = max(s.maxInFlight, s.inFlight)
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			s.inFlight--
			s.mu.Unlock()
		}()
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}

		resp := JSONRPCResponse{JSONRPC: "2.0", ID: req.ID}
		agentName, _ := args["agent_name"].(string)
		if agentName == "Broken" {
			resp.Error = &JSONRPCError{Code: -32000, Message: "inbox unavailable"}
		} else {
			resp.Result = json.RawMessage(`[{"id":1,"subject":"hi ` + agentName + `","created_ts":"2026-01-01T00:00:00Z","importance":"normal","kind":"to"}]`)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	return s
}

func peekAgentNames(prefix string, n int) []string {
	names := make([]string, n)
	for i := range names {
		names[i] = prefix + string(rune('A'+i))
	}
	return names
}

// bd-o4u96: snapshot, --robot-mail and the coordination source all read every
// agent's inbox through PeekInboxes. Callers sharing one client, like the
// projection refresh's coordination source and tmux enrichment, share one
// bound, and nothing is marked read.
func TestPeekInboxesSharesOneBoundAcrossCallers(t *testing.T) {
	t.Parallel()

	const delay = 40 * time.Millisecond
	server := newPeekInboxServer(t, delay)
	defer server.Close()
	c := NewClient(WithBaseURL(server.URL + "/"))

	first := peekAgentNames("First", 2*InboxPeekConcurrency)
	second := peekAgentNames("Second", 2*InboxPeekConcurrency)
	results := make([]map[string]InboxPeek, 2)
	started := time.Now()
	var wg sync.WaitGroup
	for i, names := range [][]string{first, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = c.PeekInboxes(context.Background(), FetchInboxOptions{ProjectKey: "/test/project", Limit: 25}, names)
		}()
	}
	wg.Wait()
	elapsed := time.Since(started)

	for i, names := range [][]string{first, second} {
		if len(results[i]) != len(names) {
			t.Fatalf("call %d returned %d inboxes, want %d", i, len(results[i]), len(names))
		}
		for _, name := range names {
			peek, ok := results[i][name]
			if !ok || peek.Err != nil || len(peek.Messages) != 1 || peek.Messages[0].Subject != "hi "+name {
				t.Fatalf("inbox %s = %+v (present %v), want its one message", name, peek, ok)
			}
		}
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.maxInFlight > InboxPeekConcurrency || server.maxInFlight < 2 {
		t.Fatalf("max concurrent fetch_inbox calls = %d, want 2..%d across both callers", server.maxInFlight, InboxPeekConcurrency)
	}
	if serial := time.Duration(len(first)+len(second)) * delay; elapsed >= serial {
		t.Fatalf("PeekInboxes took %v, no faster than reading serially (%v)", elapsed, serial)
	}
	for _, args := range server.calls {
		if args["mark_read"] != false || args["project_key"] != "/test/project" || args["limit"] != float64(25) {
			t.Fatalf("fetch_inbox arguments = %v, want mark_read=false with the caller's project and limit", args)
		}
	}
}

// bd-gm0d7: a projection refresh's two readers and the snapshot after it each
// peeked every inbox, so one process read each inbox three times. Callers
// sharing a client now share a read in flight and reuse a recent success.
func TestPeekInboxesReadsEachInboxOnceForEveryCaller(t *testing.T) {
	t.Parallel()

	server := newPeekInboxServer(t, 40*time.Millisecond)
	defer server.Close()
	c := NewClient(WithBaseURL(server.URL + "/"))
	opts := FetchInboxOptions{ProjectKey: "/test/project", Limit: 25}
	healthy := peekAgentNames("Agent", InboxPeekConcurrency)
	names := append([]string{"Broken"}, healthy...)
	reads := func() map[string]int {
		server.mu.Lock()
		defer server.mu.Unlock()
		counts := map[string]int{}
		for _, args := range server.calls {
			name, _ := args["agent_name"].(string)
			counts[name]++
		}
		return counts
	}

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.PeekInboxes(context.Background(), opts, names)
		}()
	}
	wg.Wait()
	later := c.PeekInboxes(context.Background(), opts, names)

	counts := reads()
	for _, name := range healthy {
		if counts[name] != 1 {
			t.Fatalf("%s was read %d times by three callers, want once", name, counts[name])
		}
		if peek := later[name]; peek.Err != nil || len(peek.Messages) != 1 {
			t.Fatalf("reused %s = %+v, want its one message", name, peek)
		}
	}
	if counts["Broken"] != 2 || later["Broken"].Err == nil {
		t.Fatalf("Broken was read %d times and last returned %+v; want the failed read shared once in flight, then read again", counts["Broken"], later["Broken"])
	}

	c.PeekInboxes(context.Background(), FetchInboxOptions{ProjectKey: "/test/project", Limit: 50}, healthy[:1])
	c.inboxPeeksMu.Lock()
	for _, read := range c.inboxPeeks {
		read.readAt = read.readAt.Add(-InboxPeekReuse)
	}
	c.inboxPeeksMu.Unlock()
	c.PeekInboxes(context.Background(), opts, healthy[1:2])
	counts = reads()
	if counts[healthy[0]] != 2 || counts[healthy[1]] != 2 {
		t.Fatalf("reads = %v; want a read with other options and a read older than %v each to read again", counts, InboxPeekReuse)
	}
}

// bd-o4u96, bd-7x7t7: an inbox that could not be read must stay visible to
// the caller instead of reading as an empty one.
func TestPeekInboxesReportsEveryInboxItCouldNotRead(t *testing.T) {
	t.Parallel()

	t.Run("tool error", func(t *testing.T) {
		t.Parallel()
		server := newPeekInboxServer(t, 0)
		defer server.Close()
		c := NewClient(WithBaseURL(server.URL + "/"))

		peeks := c.PeekInboxes(context.Background(), FetchInboxOptions{ProjectKey: "/test/project"}, []string{"Broken", "BlueLake"})
		if peek := peeks["Broken"]; peek.Err == nil || len(peek.Messages) != 0 {
			t.Fatalf("Broken = %+v, want the tool error and no messages", peek)
		}
		if peek := peeks["BlueLake"]; peek.Err != nil || len(peek.Messages) != 1 {
			t.Fatalf("BlueLake = %+v, want its one message", peek)
		}
	})

	t.Run("context ends mid fan-out", func(t *testing.T) {
		t.Parallel()
		server := newPeekInboxServer(t, 5*time.Second)
		defer server.Close()
		c := NewClient(WithBaseURL(server.URL + "/"))
		names := peekAgentNames("Slow", 3*InboxPeekConcurrency)

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		peeks := c.PeekInboxes(ctx, FetchInboxOptions{ProjectKey: "/test/project"}, names)
		if len(peeks) != len(names) {
			t.Fatalf("PeekInboxes returned %d entries, want one per agent (%d)", len(peeks), len(names))
		}
		for _, name := range names {
			if peek := peeks[name]; peek.Err == nil {
				t.Fatalf("%s = %+v, want an error once the context ended", name, peek)
			}
		}
	})

	t.Run("context already ended", func(t *testing.T) {
		t.Parallel()
		server := newPeekInboxServer(t, 0)
		defer server.Close()
		c := NewClient(WithBaseURL(server.URL + "/"))

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		peeks := c.PeekInboxes(ctx, FetchInboxOptions{ProjectKey: "/test/project"}, []string{"BlueLake", "GreenCastle"})
		for _, name := range []string{"BlueLake", "GreenCastle"} {
			if peek, ok := peeks[name]; !ok || peek.Err == nil {
				t.Fatalf("%s = %+v (present %v), want a context error", name, peek, ok)
			}
		}
	})
}

// TestDisabledClientNeverContactsServer locks bd-89xi0: a client built for an
// [agent_mail] enabled = false configuration refuses probes, tool calls,
// resource reads and the HTTP overseer fallback with ErrDisabled, and never
// sends a request.
func TestDisabledClientNeverContactsServer(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer server.Close()

	c := NewClient(WithBaseURL(server.URL+"/"), WithDisabled())
	ctx := context.Background()

	if c.IsAvailableContext(ctx) {
		t.Fatal("IsAvailableContext = true, want false for a disabled client")
	}
	if err := c.LastAvailabilityError(); !errors.Is(err, ErrDisabled) {
		t.Fatalf("LastAvailabilityError = %v, want ErrDisabled", err)
	}
	if _, err := c.EnsureProject(ctx, "/data/projects/ntm"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("EnsureProject error = %v, want ErrDisabled", err)
	}
	if _, err := c.ListProjectAgents(ctx, "/data/projects/ntm"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("ListProjectAgents error = %v, want ErrDisabled", err)
	}
	if _, err := c.HealthCheck(ctx); !errors.Is(err, ErrDisabled) {
		t.Fatalf("HealthCheck error = %v, want ErrDisabled", err)
	}
	if _, err := c.SendOverseerMessage(ctx, OverseerMessageOptions{
		ProjectKey:  "/data/projects/ntm",
		ProjectSlug: "data-projects-ntm",
		Recipients:  []string{"BlueLake"},
		Subject:     "hello",
		BodyMD:      "body",
	}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("SendOverseerMessage error = %v, want ErrDisabled", err)
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("server saw %d requests, want 0", n)
	}
}
