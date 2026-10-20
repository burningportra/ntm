// Package robot provides machine-readable output for AI agents.
// exit_sequences.go sends smart restart's agent exit keys and its hard-kill
// fallback.
package robot

import (
	"context"
	"os/exec"
	"strconv"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/process"
	"github.com/Dicklesworthstone/ntm/internal/tmux"
)

// exitAgent exits the current agent with its exit keys, the ones
// tmux.SendAgentExitKeys keeps for every agent type (omp's Escape, Ctrl+C,
// Ctrl+D included). win is the pane's tmux window index (#172) so the exit
// keys target the correct window on multi-window / window-per-agent layouts
// rather than always window 1.
func exitAgent(ctx context.Context, session string, win, pane int, agentType string, seq *RestartSequence) error {
	method, err := tmux.SendAgentExitKeys(ctx, formatTargetWin(session, win, pane), tmux.AgentType(agentType))
	seq.ExitMethod = method
	return err
}

// sendKeys sends literal keys to a tmux pane.
func sendKeys(ctx context.Context, session string, win, pane int, keys string) error {
	return runTmuxCommand(ctx, "send-keys", "-t", formatTargetWin(session, win, pane), "-l", keys)
}

// formatTargetWin creates a tmux target string for an explicit session,
// window, and pane address (#172). tmux window indexes may start at zero, so
// this helper must preserve the caller's window index exactly.
func formatTargetWin(session string, win, pane int) string {
	// Exact-match the session portion so a sibling session that shares the
	// name as a prefix (e.g. midas_edge vs midas_edge_api) can never be hit.
	return tmux.ExactTarget(session) + ":" + strconv.Itoa(win) + "." + strconv.Itoa(pane)
}

// runTmuxCommand executes a tmux command.
func runTmuxCommand(ctx context.Context, args ...string) error {
	if ctx == nil {
		return context.Canceled
	}
	cmd := exec.CommandContext(ctx, tmux.BinaryPath(), args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if len(output) > 0 {
			return wrapError(string(output), err)
		}
		return err
	}
	return nil
}

func waitForExitSequenceDelay(ctx context.Context, delay time.Duration) error {
	if ctx == nil {
		return context.Canceled
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// =============================================================================
// Hard Kill Fallback (bd-bh74z)
// =============================================================================
// When soft exit fails, we need to forcefully kill the agent process.
// This uses kill -9 to terminate the child process of the pane's shell.

// HardKillResult contains information about the hard kill operation.
type HardKillResult struct {
	ShellPID   int    `json:"shell_pid,omitempty"`
	ChildPID   int    `json:"child_pid,omitempty"`
	KillMethod string `json:"kill_method"`
	Success    bool   `json:"success"`
}

// hardKillAgent performs a forceful kill -9 on the agent process.
// It should be called when soft exit methods fail.
func hardKillAgent(ctx context.Context, session string, win, pane int, seq *RestartSequence) (*HardKillResult, error) {
	result := &HardKillResult{
		KillMethod: "kill_9",
	}

	// Step 1: Get shell PID from tmux
	shellPID, err := getShellPID(ctx, session, win, pane)
	if err != nil {
		return result, wrapError("failed to get shell PID", err)
	}
	result.ShellPID = shellPID

	// Step 2: Get child PID via pgrep
	childPID := process.GetChildPID(shellPID)
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if childPID <= 0 {
		// No child process might mean agent already exited
		result.KillMethod = "no_child_process"
		result.Success = true
		return result, nil
	}
	result.ChildPID = childPID

	// Step 3: kill -9 the child process
	if err := killProcess(ctx, childPID); err != nil {
		return result, wrapError("kill -9 failed", err)
	}

	// Update sequence info
	seq.ExitMethod = "hard_kill"
	result.Success = true
	return result, nil
}

// getShellPID retrieves the PID of the shell process in a tmux pane.
// Uses: tmux list-panes -t session:window -F '#{pane_index} #{pane_pid}'
// win is the pane's exact tmux window index (#172).
func getShellPID(ctx context.Context, session string, win, pane int) (int, error) {
	target := session + ":" + strconv.Itoa(win)
	cmd := exec.CommandContext(ctx, tmux.BinaryPath(), "list-panes", "-t", tmux.ExactTarget(target), "-F", "#{pane_index} #{pane_pid}")
	output, err := cmd.Output()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, ctxErr
		}
		return 0, wrapError("tmux list-panes failed", err)
	}

	// Parse output to find our pane
	lines := splitLines(string(output))
	for _, line := range lines {
		parts := splitBySpace(line)
		if len(parts) >= 2 {
			paneIdx, err := strconv.Atoi(parts[0])
			if err != nil {
				continue
			}
			if paneIdx == pane {
				pid, err := strconv.Atoi(parts[1])
				if err != nil {
					return 0, wrapError("invalid PID format", err)
				}
				return pid, nil
			}
		}
	}

	return 0, newError("pane not found")
}

// Note: getChildPID is now in the shared process package (internal/process)

// killProcess sends SIGKILL (kill -9) to a process.
func killProcess(ctx context.Context, pid int) error {
	cmd := exec.CommandContext(ctx, "kill", "-9", strconv.Itoa(pid))
	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if len(output) > 0 {
			return wrapError(trimSpace(string(output)), err)
		}
		return err
	}
	return nil
}

// splitBySpace splits a string by whitespace, handling multiple spaces.
func splitBySpace(s string) []string {
	var result []string
	var current string
	for _, c := range s {
		if c == ' ' || c == '\t' {
			if current != "" {
				result = append(result, current)
				current = ""
			}
		} else {
			current += string(c)
		}
	}
	if current != "" {
		result = append(result, current)
	}
	return result
}
