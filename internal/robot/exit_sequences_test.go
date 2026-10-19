package robot

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/tmux"
)

// useRecordingTmuxBinary points NTM_TMUX_BINARY at a script that appends each
// invocation's arguments to a log and succeeds. It returns the log path.
func useRecordingTmuxBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "tmux.log")
	path := filepath.Join(dir, "tmux")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + tmux.ShellQuote(logPath) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write recording tmux binary: %v", err)
	}
	t.Setenv("NTM_TMUX_BINARY", path)
	return logPath
}

func readTmuxLog(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read tmux log: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func TestExitAgentMuseSendsVerifiedSequence(t *testing.T) {
	// bd-3l5df, verified live against muse 1.4.11: a single Ctrl+C only clears
	// the draft, and the double Ctrl+C that quits an empty composer only
	// clears and re-arms when a draft is present. Escape plus three Ctrl+Cs
	// exits an empty, tipped, drafted or mid-turn Muse.
	logPath := useRecordingTmuxBinary(t)
	seq := &RestartSequence{}

	start := time.Now()
	if err := exitAgent(t.Context(), "proj", 3, 2, "muse", seq); err != nil {
		t.Fatalf("exitAgent(muse) error = %v", err)
	}
	elapsed := time.Since(start)

	want := []string{
		"send-keys -t proj:3.2 Escape",
		"send-keys -t proj:3.2 C-c",
		"send-keys -t proj:3.2 C-c",
		"send-keys -t proj:3.2 C-c",
	}
	if got := readTmuxLog(t, logPath); !slices.Equal(got, want) {
		t.Fatalf("exitAgent(muse) tmux calls = %q, want %q", got, want)
	}
	if seq.ExitMethod != "escape_then_triple_ctrl_c" {
		t.Errorf("ExitMethod = %q, want escape_then_triple_ctrl_c", seq.ExitMethod)
	}
	// The gaps are 100ms, 100ms and 500ms. Without the 500ms gap the third
	// Ctrl+C can reach Muse while it tears down after the second one quit it.
	if elapsed < 700*time.Millisecond {
		t.Errorf("exitAgent(muse) took %s, want at least 700ms of key gaps", elapsed)
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
