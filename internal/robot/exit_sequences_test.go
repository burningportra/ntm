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

// TestExitAgentPiAndKimiSendTheirExitSequences covers bd-7po31. Pi and Kimi
// were not restart types, so smart restart exited both with the unknown-agent
// single Ctrl+C, which only clears Pi's editor and only arms Kimi's quit.
func TestExitAgentPiAndKimiSendTheirExitSequences(t *testing.T) {
	for _, tc := range []struct {
		agentType  string
		wantKeys   []string
		wantMethod string
		// minGaps are the least time before each key after the first;
		// maxGap bounds every gap when a CLI only honors a quick second press.
		minGaps []time.Duration
		maxGap  time.Duration
	}{
		{
			// Pi quits on a second Ctrl+C within 500ms of the first, with or
			// without a draft (verified against pi 1.3.0), so the gap must
			// stay well under 500ms.
			agentType:  "pi",
			wantKeys:   []string{"C-c", "C-c"},
			wantMethod: "double_ctrl_c",
			minGaps:    []time.Duration{100 * time.Millisecond},
			maxGap:     450 * time.Millisecond,
		},
		{
			// Kimi Code documents Muse's key model: Escape interrupts a turn,
			// Ctrl+C clears a draft, and a second Ctrl+C on an empty input box
			// quits.
			agentType:  "kimi",
			wantKeys:   []string{"Escape", "C-c", "C-c", "C-c"},
			wantMethod: "escape_then_triple_ctrl_c",
			minGaps:    []time.Duration{100 * time.Millisecond, 100 * time.Millisecond, 500 * time.Millisecond},
		},
		{
			// bd-3sbjp: agents without a sequence of their own get the
			// double Ctrl+C --robot-exit-cli always sent them; the single
			// Ctrl+C smart restart sent only cleared or armed most of them.
			agentType:  "agy",
			wantKeys:   []string{"C-c", "C-c"},
			wantMethod: "double_ctrl_c_fallback",
			minGaps:    []time.Duration{100 * time.Millisecond},
			maxGap:     450 * time.Millisecond,
		},
	} {
		t.Run(tc.agentType, func(t *testing.T) {
			calls := useTimedTmuxBinary(t)
			seq := &RestartSequence{}

			if err := exitAgent(t.Context(), "proj", 3, 2, tc.agentType, seq); err != nil {
				t.Fatalf("exitAgent(%s) error = %v", tc.agentType, err)
			}

			want := make([]string, len(tc.wantKeys))
			for i, key := range tc.wantKeys {
				want[i] = "send-keys -t proj:3.2 " + key
			}
			got := drainTmuxCalls(calls)
			gotArgs := make([]string, len(got))
			for i, call := range got {
				gotArgs[i] = call.args
			}
			if !slices.Equal(gotArgs, want) {
				t.Fatalf("exitAgent(%s) tmux calls = %q, want %q", tc.agentType, gotArgs, want)
			}
			if seq.ExitMethod != tc.wantMethod {
				t.Errorf("ExitMethod = %q, want %q", seq.ExitMethod, tc.wantMethod)
			}

			const slack = 50 * time.Millisecond
			for i, minGap := range tc.minGaps {
				gap := got[i+1].at.Sub(got[i].at)
				if gap < minGap-slack {
					t.Errorf("gap before tmux call %d (%q) = %s, want at least %s", i+1, got[i+1].args, gap, minGap)
				}
				if tc.maxGap > 0 && gap > tc.maxGap {
					t.Errorf("gap before tmux call %d (%q) = %s, want at most %s", i+1, got[i+1].args, gap, tc.maxGap)
				}
			}
		})
	}
}

// TestSendExitChoreographyUsesEachAgentsExitSequence covers --robot-exit-cli.
// It sends the same per-agent keys smart restart does, addressed by pane ID.
// A plain double Ctrl+C on a working Muse pane interrupts the turn and then
// clears the restored draft, so Muse only quit on the second attempt, about
// 15 s later (bd-fkuad); Kimi has the same key model and still got the plain
// double Ctrl+C until bd-3sbjp gave exit-cli the exit_sequences.go keys.
func TestSendExitChoreographyUsesEachAgentsExitSequence(t *testing.T) {
	museKeys := func(id string) []string {
		return []string{"send-keys -t " + id + " Escape", "send-keys -t " + id + " C-c", "send-keys -t " + id + " C-c", "send-keys -t " + id + " C-c"}
	}
	doubleCtrlC := func(id string) []string {
		return []string{"send-keys -t " + id + " C-c", "send-keys -t " + id + " C-c"}
	}
	for _, tc := range []struct {
		name string
		pane tmux.Pane
		want []string
	}{
		{
			name: "muse pane type",
			pane: tmux.Pane{ID: "%7", Type: tmux.AgentMuse, Title: "proj__muse_1"},
			want: museKeys("%7"),
		},
		{
			name: "muse title with model variant",
			pane: tmux.Pane{ID: "%8", Title: "proj__muse_2_wonder-full@xhigh"},
			want: museKeys("%8"),
		},
		{
			name: "kimi gets the muse keys",
			pane: tmux.Pane{ID: "%10", Type: tmux.AgentKimi, Title: "proj__kimi_1"},
			want: museKeys("%10"),
		},
		{
			name: "claude keeps the double ctrl-c",
			pane: tmux.Pane{ID: "%9", Type: tmux.AgentClaude, Title: "proj__cc_1"},
			want: doubleCtrlC("%9"),
		},
		{
			name: "pi gets the double ctrl-c",
			pane: tmux.Pane{ID: "%11", Type: tmux.AgentPi, Title: "proj__pi_1"},
			want: doubleCtrlC("%11"),
		},
		{
			// The typed "/exit\n" ends the fake tmux's log line early,
			// which leaves an empty line after it.
			name: "codex gets the exit command",
			pane: tmux.Pane{ID: "%12", Type: tmux.AgentCodex, Title: "proj__cod_1"},
			want: []string{"send-keys -t %12 -l /exit", ""},
		},
		{
			name: "gemini gets escape then the exit command",
			pane: tmux.Pane{ID: "%13", Type: tmux.AgentGemini, Title: "proj__gmi_1"},
			want: []string{"send-keys -t %13 Escape", "send-keys -t %13 -l /exit", ""},
		},
		{
			name: "other agents get the double ctrl-c fallback",
			pane: tmux.Pane{ID: "%14", Type: tmux.AgentAntigravity, Title: "proj__agy_1"},
			want: doubleCtrlC("%14"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := useTimedTmuxBinary(t)
			if err := sendExitChoreography(t.Context(), tc.pane); err != nil {
				t.Fatalf("sendExitChoreography() error = %v", err)
			}
			got := drainTmuxCalls(calls)
			gotArgs := make([]string, len(got))
			for i, call := range got {
				gotArgs[i] = call.args
			}
			if !slices.Equal(gotArgs, tc.want) {
				t.Fatalf("sendExitChoreography() tmux calls = %q, want %q", gotArgs, tc.want)
			}
		})
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
