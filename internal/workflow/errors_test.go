package workflow

import (
	"context"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

type recordingActions struct {
	mu    sync.Mutex
	calls []string
}

func (a *recordingActions) add(s string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, s)
}

// recorded returns a copy of the calls so far. A timeout monitor records from
// its own goroutine, so tests must not read calls directly.
func (a *recordingActions) recorded() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.calls)
}
func (a *recordingActions) RestartAgent(context.Context, string) error { a.add("restart"); return nil }
func (a *recordingActions) Pause(context.Context, string) error        { a.add("pause"); return nil }
func (a *recordingActions) SkipStage(context.Context) error            { a.add("skip"); return nil }
func (a *recordingActions) Abort(context.Context, error) error         { a.add("abort"); return nil }
func (a *recordingActions) RetryStage(context.Context) error           { a.add("retry"); return nil }
func (a *recordingActions) Notify(context.Context, *WorkflowError, string) error {
	a.add("notify")
	return nil
}

func TestErrorHandlerActionsAndRetries(t *testing.T) {
	actions := &recordingActions{}
	h := NewErrorHandler(ErrorHandlingConfig{OnAgentCrash: ErrorActionRestartAgent, OnAgentError: ErrorActionPause, OnTriggerFailed: ErrorActionSkipStage, OnTimeout: ErrorActionRetry, MaxRetriesPerStage: 1}, actions)
	for _, err := range []*WorkflowError{{Type: ErrorAgentCrash}, {Type: ErrorAgentError}, {Type: ErrorTriggerFailed}, {Type: ErrorTimeout, Stage: "build"}, {Type: ErrorTimeout, Stage: "build"}} {
		if got := h.Handle(context.Background(), err); got != nil {
			t.Fatal(got)
		}
	}
	want := []string{"restart", "pause", "skip", "retry", "abort"}
	if got := actions.recorded(); !slices.Equal(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

func TestTimeoutMonitorFiresOnlyForCurrentStage(t *testing.T) {
	cases := []struct {
		name    string
		current string
		stop    bool
		want    []string
	}{
		{name: "stage still current", current: "build", want: []string{"notify"}},
		{name: "stage moved on", current: "review"},
		{name: "stopped before expiry", current: "build", stop: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				actions := &recordingActions{}
				h := NewErrorHandler(ErrorHandlingConfig{OnTimeout: ErrorActionNotify}, actions)
				m := NewTimeoutMonitor(time.Minute, h, func() string { return tc.current })
				m.Start(context.Background(), "build")
				if tc.stop {
					m.Stop()
				}
				// The bubble's clock moves only once every goroutine in it is
				// blocked, so this passes the timeout without waiting for real,
				// and Wait returns once the monitor goroutine has finished.
				time.Sleep(2 * time.Minute)
				synctest.Wait()
				if got := actions.recorded(); !slices.Equal(got, tc.want) {
					t.Fatalf("calls = %v, want %v", got, tc.want)
				}
			})
		})
	}
}

// Stage changes restart the monitor while shutdown stops it, and those can
// come from different goroutines. Run with -race.
func TestTimeoutMonitorStartAndStopFromDifferentGoroutines(t *testing.T) {
	h := NewErrorHandler(ErrorHandlingConfig{OnTimeout: ErrorActionNotify}, &recordingActions{})
	m := NewTimeoutMonitor(time.Hour, h, func() string { return "build" })
	t.Cleanup(m.Stop)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		m.Start(context.Background(), "build")
	}()
	go func() {
		defer wg.Done()
		m.Stop()
	}()
	wg.Wait()
}
