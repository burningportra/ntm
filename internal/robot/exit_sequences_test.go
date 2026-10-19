//go:build unix

package robot

import (
	"bufio"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/tmux"
)

// fakeTmuxCall is one invocation of the fake tmux binary and the moment the
// test received it.
type fakeTmuxCall struct {
	args string
	at   time.Time
}

// useTimedTmuxBinary points NTM_TMUX_BINARY at a script that writes each
// invocation's arguments to a FIFO and succeeds. The test reads the FIFO as
// the calls arrive and stamps each one, which times the gaps between keys as
// tmux would see them; the shell has no sub-second clock to stamp a log with.
func useTimedTmuxBinary(t *testing.T) <-chan fakeTmuxCall {
	t.Helper()
	dir := t.TempDir()
	fifoPath := filepath.Join(dir, "tmux.fifo")
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		t.Fatalf("create tmux call FIFO: %v", err)
	}
	// Opening read-write keeps a writer on the test's side, so the open does
	// not block and the reader never sees EOF between fake tmux invocations.
	fifo, err := os.OpenFile(fifoPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open tmux call FIFO: %v", err)
	}
	t.Cleanup(func() { _ = fifo.Close() })

	calls := make(chan fakeTmuxCall, 16)
	go func() {
		defer close(calls)
		scanner := bufio.NewScanner(fifo)
		for scanner.Scan() {
			calls <- fakeTmuxCall{args: scanner.Text(), at: time.Now()}
		}
	}()

	path := filepath.Join(dir, "tmux")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > " + tmux.ShellQuote(fifoPath) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write timed tmux binary: %v", err)
	}
	t.Setenv("NTM_TMUX_BINARY", path)
	return calls
}

// drainTmuxCalls collects the calls useTimedTmuxBinary received once the code
// under test has returned. Every fake tmux process has exited by then, so its
// line is already in the FIFO and the reader only needs a moment to pass it on.
func drainTmuxCalls(calls <-chan fakeTmuxCall) []fakeTmuxCall {
	var got []fakeTmuxCall
	for {
		select {
		case call, ok := <-calls:
			if !ok {
				return got
			}
			got = append(got, call)
		case <-time.After(200 * time.Millisecond):
			return got
		}
	}
}

func TestExitAgentMuseSendsVerifiedSequence(t *testing.T) {
	// bd-3l5df, verified live against muse 1.4.11: a single Ctrl+C only clears
	// the draft, and the double Ctrl+C that quits an empty composer only
	// clears and re-arms when a draft is present. Escape plus three Ctrl+Cs
	// exits an empty, tipped, drafted or mid-turn Muse.
	calls := useTimedTmuxBinary(t)
	seq := &RestartSequence{}

	if err := exitAgent(t.Context(), "proj", 3, 2, "muse", seq); err != nil {
		t.Fatalf("exitAgent(muse) error = %v", err)
	}

	want := []string{
		"send-keys -t proj:3.2 Escape",
		"send-keys -t proj:3.2 C-c",
		"send-keys -t proj:3.2 C-c",
		"send-keys -t proj:3.2 C-c",
	}
	got := drainTmuxCalls(calls)
	gotArgs := make([]string, len(got))
	for i, call := range got {
		gotArgs[i] = call.args
	}
	if !slices.Equal(gotArgs, want) {
		t.Fatalf("exitAgent(muse) tmux calls = %q, want %q", gotArgs, want)
	}
	if seq.ExitMethod != "escape_then_triple_ctrl_c" {
		t.Errorf("ExitMethod = %q, want escape_then_triple_ctrl_c", seq.ExitMethod)
	}

	// The gaps are 100ms, 100ms and 500ms, in that order. Without the 500ms
	// gap before the third Ctrl+C it can reach Muse while it tears down after
	// the second one quit it. The slack absorbs the reader goroutine waking
	// later for one call than for the next.
	const slack = 50 * time.Millisecond
	for i, minGap := range []time.Duration{100 * time.Millisecond, 100 * time.Millisecond, 500 * time.Millisecond} {
		if gap := got[i+1].at.Sub(got[i].at); gap < minGap-slack {
			t.Errorf("gap before tmux call %d (%q) = %s, want at least %s", i+1, got[i+1].args, gap, minGap)
		}
	}
}

func TestExitAgentMuseStopsAtFirstFailedKey(t *testing.T) {
	useFailingTmuxBinary(t)
	seq := &RestartSequence{}

	err := exitAgent(t.Context(), "proj", 1, 0, "muse", seq)
	if err == nil || !strings.Contains(err.Error(), "escape failed") {
		t.Fatalf("exitAgent(muse) error = %v, want escape failure", err)
	}
}
