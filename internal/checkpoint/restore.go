package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/agent"
	"github.com/Dicklesworthstone/ntm/internal/dispatch"
	"github.com/Dicklesworthstone/ntm/internal/tmux"
)

// Restore errors
var (
	ErrSessionExists     = errors.New("session already exists (use Force option to override)")
	ErrDirectoryNotFound = errors.New("checkpoint working directory not found")
	ErrWorkingDirInvalid = errors.New("checkpoint working directory is invalid")
	ErrNoAgentsToRestore = errors.New("checkpoint contains no agents to restore")
	ErrNilCheckpoint     = errors.New("checkpoint is nil")
)

var errWorkingDirNotDirectory = errors.New("not a directory")

const (
	// restoreAgentReadyTimeout bounds the restore's wait for the relaunched
	// agents to come up, shared by all panes and by relaunch and context
	// injection. It matches robot spawn's default ready wait.
	restoreAgentReadyTimeout = 30 * time.Second
	// restoreAgentReadyPollInterval is restart-pane's readiness poll period.
	restoreAgentReadyPollInterval = 400 * time.Millisecond
	// restoreLaunchAttempts is how many times restore launches an agent whose
	// launch fails outright.
	restoreLaunchAttempts = 3
)

// RestoreOptions configures how a checkpoint is restored.
type RestoreOptions struct {
	// Force kills any existing session with the same name
	Force bool
	// SkipGitCheck skips warning about git state mismatch
	SkipGitCheck bool
	// InjectContext sends scrollback/summary to agents after spawning
	InjectContext bool
	// DryRun shows what would be done without making changes
	DryRun bool
	// CustomDirectory overrides the checkpoint's working directory
	CustomDirectory string
	// ScrollbackLines is how many lines of scrollback to inject (0 = all captured)
	ScrollbackLines int
}

// RestoreResult contains details about what was restored.
type RestoreResult struct {
	// SessionName is the restored session name
	SessionName string
	// PanesRestored is the number of panes created
	PanesRestored int
	// ContextInjected indicates if scrollback was sent to agents
	ContextInjected bool
	// Warnings contains non-fatal issues encountered
	Warnings []string
	// DryRun indicates this was a simulation
	DryRun bool

	// Assignments contains bead-to-agent assignment state from the checkpoint (bd-32ck).
	// Empty if no assignments were captured.
	Assignments []AssignmentSnapshot
	// BVSummary contains BV triage summary from the checkpoint (bd-32ck).
	// Nil if no BV snapshot was captured.
	BVSummary *BVSnapshot
}

// Restorer handles checkpoint restoration.
type Restorer struct {
	storage *Storage
	// agentReadyTimeout overrides restoreAgentReadyTimeout when positive.
	agentReadyTimeout time.Duration
}

// NewRestorer creates a new Restorer with default storage.
func NewRestorer() *Restorer {
	return &Restorer{
		storage: NewStorage(),
	}
}

// NewRestorerWithStorage creates a Restorer with custom storage.
func NewRestorerWithStorage(storage *Storage) *Restorer {
	return &Restorer{
		storage: storage,
	}
}

// Restore restores a session from a checkpoint.
func (r *Restorer) Restore(sessionName, checkpointID string, opts RestoreOptions) (*RestoreResult, error) {
	// Load checkpoint
	cp, err := r.storage.Load(sessionName, checkpointID)
	if err != nil {
		return nil, fmt.Errorf("loading checkpoint: %w", err)
	}

	return r.RestoreFromCheckpoint(cp, opts)
}

// RestoreFromCheckpoint restores a session from a loaded checkpoint.
func (r *Restorer) RestoreFromCheckpoint(cp *Checkpoint, opts RestoreOptions) (*RestoreResult, error) {
	if cp == nil {
		return nil, ErrNilCheckpoint
	}
	if err := validateCheckpointAutomatedRelaunch(cp); err != nil {
		return nil, err
	}

	result := &RestoreResult{
		SessionName: cp.SessionName,
		DryRun:      opts.DryRun,
		Assignments: cp.Assignments,
		BVSummary:   cp.BVSummary,
	}

	// Surface assignment and BV summary from checkpoint (bd-32ck)
	if len(cp.Assignments) > 0 {
		slog.Info("checkpoint contains assignments",
			"session", cp.SessionName,
			"assignment_count", len(cp.Assignments))
	}
	if cp.BVSummary != nil {
		slog.Info("checkpoint contains BV summary",
			"session", cp.SessionName,
			"actionable", cp.BVSummary.ActionableCount,
			"blocked", cp.BVSummary.BlockedCount,
			"in_progress", cp.BVSummary.InProgressCount)
	}

	// Determine working directory
	workDir := cp.WorkingDir
	if opts.CustomDirectory != "" {
		workDir = opts.CustomDirectory
	}

	// Validate working directory exists and is usable.
	if workDir != "" {
		if err := validateWorkingDirectory(workDir); err != nil {
			if opts.DryRun {
				result.Warnings = append(result.Warnings, workingDirectoryIssue(workDir, err))
			} else {
				if errors.Is(err, os.ErrNotExist) {
					return nil, fmt.Errorf("%w: %s", ErrDirectoryNotFound, workDir)
				}
				return nil, fmt.Errorf("%w: %s: %v", ErrWorkingDirInvalid, workDir, err)
			}
		}
	}
	// Check for existing session
	if tmux.SessionExists(cp.SessionName) {
		if !opts.Force {
			return nil, ErrSessionExists
		}
		if !opts.DryRun {
			if err := tmux.KillSession(cp.SessionName); err != nil {
				return nil, fmt.Errorf("killing existing session: %w", err)
			}
			// Wait for session to be fully killed
			time.Sleep(100 * time.Millisecond)
		} else {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("would kill existing session %q", cp.SessionName))
		}
	}

	// Validate we have panes to restore
	if len(cp.Session.Panes) == 0 {
		return nil, ErrNoAgentsToRestore
	}

	// Check git state if requested
	if !opts.SkipGitCheck && cp.Git.Commit != "" && workDir != "" {
		if warning := r.checkGitState(cp, workDir); warning != "" {
			result.Warnings = append(result.Warnings, warning)
		}
	}
	restoreDir := effectiveRestoreDir(workDir)

	if opts.DryRun {
		// Simulate what would happen
		result.PanesRestored = len(cp.Session.Panes)
		result.ContextInjected = opts.InjectContext
		return result, nil
	}

	// Create the session
	if err := r.createSession(cp, restoreDir); err != nil {
		return nil, fmt.Errorf("creating session: %w", err)
	}

	// Create additional panes to match checkpoint layout
	panesCreated, err := r.restoreLayout(cp, restoreDir)
	if err != nil {
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("layout restoration incomplete: %v", err))
	}
	result.PanesRestored = panesCreated

	// Relaunch and context injection wait on one deadline, so an agent that
	// is slow to come up costs the restore its wait once.
	readyTimeout := r.readyTimeout()
	readyDeadline := time.Now().Add(readyTimeout)
	for _, err := range r.restoreAgents(cp, restoreDir, readyDeadline, readyTimeout) {
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("agent restoration incomplete: %v", err))
	}

	// Inject context if requested
	if opts.InjectContext {
		failures := r.injectContext(cp, opts.ScrollbackLines, readyDeadline, readyTimeout)
		for _, err := range failures {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("context injection failed: %v", err))
		}
		result.ContextInjected = len(failures) == 0
	}

	if err := r.restoreActivePane(cp); err != nil {
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("active pane restoration failed: %v", err))
	}

	return result, nil
}

func validateCheckpointAutomatedRelaunch(cp *Checkpoint) error {
	for _, pane := range cp.Session.Panes {
		if err := agent.AgentType(pane.AgentType).ValidateAutomatedRelaunch(); err != nil {
			return fmt.Errorf("checkpoint pane %d (%s): %w", pane.Index, pane.AgentType, err)
		}
	}
	return nil
}

// createSession creates the initial tmux session.
func (r *Restorer) createSession(cp *Checkpoint, workDir string) error {
	if err := tmux.CreateSession(cp.SessionName, workDir); err != nil {
		return err
	}

	// Wait for session to be ready
	time.Sleep(100 * time.Millisecond)

	// Set the title of the first pane if we have pane info
	panes := sortedCheckpointPanes(cp.Session.Panes)
	if len(panes) > 0 {
		if err := moveInitialWindow(cp.SessionName, panes[0].WindowIndex); err != nil {
			return err
		}
		firstPane := panes[0]
		if firstPane.Title != "" {
			panes, err := tmux.GetPanes(cp.SessionName)
			if err == nil && len(panes) > 0 {
				_ = tmux.SetPaneTitle(panes[0].ID, firstPane.Title)
			}
		}
	}

	return nil
}

// restoreLayout creates additional panes to match the checkpoint layout.
func (r *Restorer) restoreLayout(cp *Checkpoint, workDir string) (int, error) {
	paneStates := sortedCheckpointPanes(cp.Session.Panes)
	if len(paneStates) == 0 {
		return 0, nil
	}

	// First pane was created with the session, so we start at 1.
	panesCreated := 1
	lastWindowIndex := paneStates[0].WindowIndex

	for i := 1; i < len(paneStates); i++ {
		paneState := paneStates[i]
		windowTarget := fmt.Sprintf("%s:%d", cp.SessionName, paneState.WindowIndex)

		var (
			paneID string
			err    error
		)

		if paneState.WindowIndex != lastWindowIndex {
			paneID, err = tmux.DefaultClient.Run(
				"new-window",
				"-P",
				"-F",
				"#{pane_id}",
				"-t",
				windowTarget,
				"-c",
				workDir,
			)
			lastWindowIndex = paneState.WindowIndex
		} else {
			paneID, err = tmux.DefaultClient.Run(
				"split-window",
				"-t",
				windowTarget,
				"-c",
				workDir,
				"-P",
				"-F",
				"#{pane_id}",
			)
		}
		if err != nil {
			return panesCreated, fmt.Errorf("creating pane %d: %w", i, err)
		}

		// Set pane title to match checkpoint
		if paneState.Title != "" {
			_ = tmux.SetPaneTitle(paneID, paneState.Title)
		}

		panesCreated++

		// Small delay between pane creations for stability
		time.Sleep(50 * time.Millisecond)
	}

	// Apply captured layouts after panes exist.
	if len(cp.Session.WindowLayouts) > 0 {
		if err := r.applyWindowLayouts(cp.SessionName, cp.Session.WindowLayouts); err != nil {
			// Non-fatal - layout is a best-effort feature
			return panesCreated, fmt.Errorf("applying window layouts: %w", err)
		}
	} else if cp.Session.Layout != "" {
		if !canRestoreLegacySessionLayout(cp.Session) {
			return panesCreated, fmt.Errorf("skipping legacy single layout for multi-window checkpoint; per-window layouts are missing")
		}
		if err := r.applyLayout(cp.SessionName, cp.Session.Layout); err != nil {
			// Non-fatal - layout is a best-effort feature
			return panesCreated, fmt.Errorf("applying layout: %w", err)
		}
	}

	return panesCreated, nil
}

// readyTimeout is how long the restore waits for its relaunched agents.
func (r *Restorer) readyTimeout() time.Duration {
	if r.agentReadyTimeout > 0 {
		return r.agentReadyTimeout
	}
	return restoreAgentReadyTimeout
}

// restoreAgents relaunches the agent in each agent pane and waits, until
// deadline, for every agent to be up (see relaunchRestoredPanes). It returns
// one error per agent pane whose agent is not up.
func (r *Restorer) restoreAgents(cp *Checkpoint, workDir string, deadline time.Time, timeout time.Duration) []error {
	panes, err := tmux.GetPanes(cp.SessionName)
	if err != nil {
		return []error{fmt.Errorf("getting panes: %w", err)}
	}

	sortedStates := sortedCheckpointPanes(cp.Session.Panes)
	sortedPanes := sortedTmuxPanes(panes)
	var failures []error
	var launches []*restoredLaunch

	for i, paneState := range sortedStates {
		if i >= len(sortedPanes) {
			break
		}

		agentCmd := restorableAgentCommand(paneState)
		if agentCmd == "" {
			continue
		}

		paneID := sortedPanes[i].ID
		safeCommand, err := tmux.SanitizePaneCommand(agentCmd)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", restoredPaneLabel(paneID, paneState.Title), err))
			continue
		}
		// remain-on-exit keeps the pane when its launcher exits, so the
		// restore sees the exit and can launch the agent again. Without it
		// tmux closes the pane, and the session along with its last pane.
		_ = tmux.DefaultClient.RunSilent("set-option", "-p", "-t", paneID, "remain-on-exit", "on")
		launches = append(launches, &restoredLaunch{pane: paneState, paneID: paneID, command: safeCommand})
	}

	// Respawn each pane directly into its command instead of typing into a
	// shell prompt, which loses input typed while the pane initializes.
	relaunchRestoredPanes(launches, deadline, timeout, restoreAgentReadyPollInterval, restoredPaneOps{
		respawn: func(paneID, command string) error {
			return tmux.DefaultClient.RunSilent("respawn-pane", "-k", "-c", workDir, "-t", paneID, command)
		},
		observe: observeRestoredPane,
	})

	for _, l := range launches {
		// A dead pane keeps remain-on-exit, so it stays in the layout showing
		// tmux's "Pane is dead" line with the exit status; the others close
		// when their agent exits, as before.
		if !l.last.dead {
			_ = tmux.DefaultClient.RunSilent("set-option", "-p", "-u", "-t", l.paneID, "remain-on-exit")
		}
		if l.err == nil {
			continue
		}
		slog.Warn("checkpoint restore: relaunched agent is not up",
			"session", cp.SessionName,
			"pane_id", l.paneID,
			"pane_index", l.pane.Index,
			"window_index", l.pane.WindowIndex,
			"agent_type", l.pane.AgentType,
			"command", l.command,
			"attempts", l.attempts,
			"error", l.err)
		failures = append(failures, fmt.Errorf("%s: %w", restoredPaneLabel(l.paneID, l.pane.Title), l.err))
	}
	return failures
}

// restoredPaneLabel names a restored pane in warnings: its tmux ID and, when
// the checkpoint has one, its title.
func restoredPaneLabel(paneID, title string) string {
	if title == "" {
		return "pane " + paneID
	}
	return "pane " + paneID + " (" + title + ")"
}

// restoredPaneOps are the tmux operations a relaunch takes, swapped out in
// tests.
type restoredPaneOps struct {
	// respawn kills the pane's process and starts command in its place.
	respawn func(paneID, command string) error
	// observe looks at the pane once.
	observe func(paneID string) (restoredPaneState, error)
}

// restoredLaunch is one agent pane's relaunch.
type restoredLaunch struct {
	pane     PaneState
	paneID   string
	command  string
	attempts int
	// started is set once the pane has shown the same non-shell process on
	// two polls in a row since its last launch. One sighting does not count:
	// a launcher run through a wrapper shows the wrapper for an instant (a
	// `#!/usr/bin/env bash` script is env until env execs bash).
	started     bool
	lastCommand string
	// last is the latest observation and observeErr the latest observation's
	// error.
	last       restoredPaneState
	observeErr error
	// err says why the agent is not up once the relaunch is over; nil when
	// it is up.
	err error
}

// relaunchRestoredPanes launches each pane's agent and polls the panes
// together until every agent is up, has failed outright, or deadline passes.
// An agent is up when restoredPaneState.ready holds, the same check context
// injection waits on. A pane is launched again, up to restoreLaunchAttempts
// launches, only after a definite failure: the pane died (its launcher or
// agent exited), or it is back in a shell after its agent had started. A
// launcher still in its shell is never killed however long it takes (a
// wrapper script, an update check, an nvm or asdf shim), and neither is an
// agent that started but is not ready: a pane in either state at the deadline
// is reported as such and left running. Each launch's err records the outcome.
func relaunchRestoredPanes(launches []*restoredLaunch, deadline time.Time, timeout, pollInterval time.Duration, ops restoredPaneOps) {
	pending := make([]*restoredLaunch, 0, len(launches))
	for _, l := range launches {
		if l.launch(ops) {
			pending = append(pending, l)
		}
	}
	for len(pending) > 0 {
		open := pending[:0]
		for _, l := range pending {
			if !l.poll(ops) {
				open = append(open, l)
			}
		}
		pending = open
		if len(pending) == 0 {
			return
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			for _, l := range pending {
				l.err = l.notUpError(timeout)
			}
			return
		}
		time.Sleep(min(pollInterval, remaining))
	}
}

// launch starts the pane's agent command and reports whether it did.
func (l *restoredLaunch) launch(ops restoredPaneOps) bool {
	l.attempts++
	l.started, l.lastCommand, l.last = false, "", restoredPaneState{}
	if err := ops.respawn(l.paneID, l.command); err != nil {
		l.err = fmt.Errorf("launching agent: %w", err)
		return false
	}
	return true
}

// poll observes the pane once and reports whether its relaunch is over: the
// agent is up, or its launch failed outright with no attempts left. A launch
// that failed outright with attempts left is launched again.
func (l *restoredLaunch) poll(ops restoredPaneOps) bool {
	state, err := ops.observe(l.paneID)
	l.observeErr = err
	if err != nil {
		return false
	}
	l.last = state
	switch {
	case state.gone:
		l.err = errors.New("pane closed while its agent was starting")
		return true
	case state.ready():
		return true
	case state.dead || (l.started && tmux.PaneCommandIsShell(state.command)):
		if l.attempts < restoreLaunchAttempts {
			return !l.launch(ops)
		}
		l.err = fmt.Errorf("agent failed to start in %d attempts; the last %s", l.attempts, state.failure())
		return true
	case state.agentRunning():
		l.started = l.started || state.command == l.lastCommand
		l.lastCommand = state.command
	default:
		l.lastCommand = ""
	}
	return false
}

// notUpError says why an agent whose relaunch is still open at the deadline
// is not up.
func (l *restoredLaunch) notUpError(timeout time.Duration) error {
	switch {
	case l.observeErr != nil:
		return fmt.Errorf("agent not ready within %s: %w", timeout, l.observeErr)
	case l.last.agentRunning():
		return fmt.Errorf("agent %q started but not ready within %s", l.last.command, timeout)
	case l.last.command != "":
		return fmt.Errorf("agent not started within %s; its launcher (%s) is still running", timeout, l.last.command)
	default:
		return fmt.Errorf("agent not started within %s; its launcher is still running", timeout)
	}
}

// restoredPaneState is one look at a relaunched pane.
type restoredPaneState struct {
	// gone is set when tmux no longer has the pane.
	gone bool
	// dead is set when the pane's process exited and remain-on-exit kept the
	// pane; exitStatus is the process's exit status.
	dead       bool
	exitStatus string
	// command is the pane's foreground process, which for a dead pane is the
	// one that exited.
	command string
	// output is the pane's last 50 lines, captured only while a non-shell
	// process runs in it.
	output string
}

// agentRunning reports whether a non-shell process, the agent or a launcher
// that is not a shell script, runs in the pane.
func (s restoredPaneState) agentRunning() bool {
	return !s.gone && !s.dead && s.command != "" && !tmux.PaneCommandIsShell(s.command)
}

// ready reports whether the pane's agent is up: its output passes
// agent.IsReadyOutput, the check robot spawn and restart-pane wait on, while
// a non-shell process runs in the pane. A shell prompt passes the text check
// too, so the process corroborates it as spawn's and restart-pane's process
// checks do.
func (s restoredPaneState) ready() bool {
	return s.agentRunning() && agent.IsReadyOutput(s.output)
}

// failure describes a launch that failed outright.
func (s restoredPaneState) failure() string {
	switch {
	case s.dead && s.exitStatus != "":
		return "exited with status " + s.exitStatus
	case s.dead:
		return "exited"
	default:
		return fmt.Sprintf("returned to a shell (%s) after its agent started", s.command)
	}
}

// observeRestoredPane looks at a relaunched pane through tmux.
func observeRestoredPane(paneID string) (restoredPaneState, error) {
	sep := tmux.FieldSeparator
	output, err := tmux.DefaultClient.Run("display-message", "-p", "-t", paneID,
		"#{pane_id}"+sep+"#{pane_dead}"+sep+"#{pane_dead_status}"+sep+"#{pane_current_command}")
	if err != nil {
		switch tmux.ClassifyCommandError(err).Kind {
		case tmux.CommandErrorPaneNotFound, tmux.CommandErrorSessionNotFound:
			return restoredPaneState{gone: true}, nil
		}
		return restoredPaneState{}, fmt.Errorf("reading pane state: %w", err)
	}
	fields := strings.Split(strings.TrimSpace(output), sep)
	if len(fields) != 4 {
		return restoredPaneState{}, fmt.Errorf("reading pane state: unexpected output %q", output)
	}
	// Newer tmux prints empty fields, not an error, for a pane it no longer
	// has.
	if fields[0] == "" {
		return restoredPaneState{gone: true}, nil
	}
	state := restoredPaneState{
		dead:       fields[1] == "1",
		exitStatus: fields[2],
		command:    strings.TrimSpace(fields[3]),
	}
	if state.agentRunning() {
		if state.output, err = tmux.CapturePaneOutput(paneID, 50); err != nil {
			return restoredPaneState{}, fmt.Errorf("capturing pane: %w", err)
		}
	}
	return state, nil
}

func moveInitialWindow(sessionName string, targetWindowIndex int) error {
	if targetWindowIndex < 0 {
		return nil
	}

	currentWindowIndex, err := tmux.GetFirstWindow(sessionName)
	if err != nil {
		return fmt.Errorf("getting initial window index: %w", err)
	}
	if currentWindowIndex == targetWindowIndex {
		return nil
	}

	source := fmt.Sprintf("%s:%d", sessionName, currentWindowIndex)
	target := fmt.Sprintf("%s:%d", sessionName, targetWindowIndex)
	if err := tmux.DefaultClient.RunSilent("move-window", "-s", source, "-t", target); err != nil {
		return fmt.Errorf("moving initial window from %s to %s: %w", source, target, err)
	}
	return nil
}

func (r *Restorer) restoreActivePane(cp *Checkpoint) error {
	if cp.Session.ActivePaneIndex < 0 || cp.Session.ActivePaneIndex >= len(cp.Session.Panes) {
		return nil
	}

	panes, err := tmux.GetPanes(cp.SessionName)
	if err != nil {
		return fmt.Errorf("getting panes: %w", err)
	}
	targetPane, ok := restoredPaneForCheckpointIndex(cp, panes, cp.Session.ActivePaneIndex)
	if !ok {
		return nil
	}
	return tmux.DefaultClient.RunSilent("select-pane", "-t", targetPane.ID)
}

// applyLayout applies a tmux layout string to a session.
func (r *Restorer) applyLayout(sessionName, layout string) error {
	if layout == "" {
		layout = "tiled"
	}

	output, err := tmux.DefaultClient.Run("list-windows", "-t", sessionName, "-F", "#{window_index}")
	if err != nil {
		return err
	}

	for _, win := range strings.Split(strings.TrimSpace(output), "\n") {
		if win == "" {
			continue
		}
		target := fmt.Sprintf("%s:%s", sessionName, win)
		if err := tmux.DefaultClient.RunSilent("select-layout", "-t", target, layout); err != nil {
			return err
		}
	}

	return nil
}

func (r *Restorer) applyWindowLayouts(sessionName string, windowLayouts []WindowLayoutState) error {
	for _, windowLayout := range cloneWindowLayouts(windowLayouts) {
		layout := strings.TrimSpace(windowLayout.Layout)
		if layout == "" {
			layout = "tiled"
		}
		target := fmt.Sprintf("%s:%d", sessionName, windowLayout.WindowIndex)
		if err := tmux.DefaultClient.RunSilent("select-layout", "-t", target, layout); err != nil {
			return err
		}
	}
	return nil
}

// injectContext submits each relaunched agent's scrollback through dispatch's
// agent-aware submit, the protocol ntm send uses, once the agent is ready. A
// paste sent while the agent is still starting lands in its launcher's shell
// or a startup screen, or is discarded when the TUI takes over the terminal,
// so a pane whose agent is not ready by readyDeadline, which relaunch waited
// on too, gets nothing. It returns one error per pane that did not get its
// context.
func (r *Restorer) injectContext(cp *Checkpoint, maxLines int, readyDeadline time.Time, readyTimeout time.Duration) []error {
	panes, err := tmux.GetPanes(cp.SessionName)
	if err != nil {
		return []error{fmt.Errorf("getting panes: %w", err)}
	}

	var failures []error
	for i, paneState := range cp.Session.Panes {
		if paneState.ScrollbackFile == "" {
			continue
		}
		// restoreAgents relaunched an agent only where this returns a
		// command. Any other pane (a user pane) is a shell, which would run
		// the pasted scrollback as commands.
		if restorableAgentCommand(paneState) == "" {
			continue
		}

		targetPane, ok := restoredPaneForCheckpointIndex(cp, panes, i)
		if !ok {
			continue
		}
		label := restoredPaneLabel(targetPane.ID, paneState.Title)

		// Load scrollback content
		content, err := r.loadPaneScrollbackForPane(cp.SessionName, cp.ID, paneState)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", label, err))
			continue
		}

		// Truncate if maxLines specified
		if maxLines > 0 {
			content = truncateToLines(content, maxLines)
		}

		if err := waitForRestoredAgentReady(targetPane.ID, readyDeadline, readyTimeout, restoreAgentReadyPollInterval, observeRestoredPane); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w; context not injected", label, err))
			continue
		}

		contextMsg := formatContextInjection(content, cp.CreatedAt)
		if err := (dispatch.TMUXDeliverer{}).SubmitPrompt(context.Background(), targetPane.ID, tmux.AgentType(paneState.AgentType), contextMsg); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", label, err))
		}
	}

	return failures
}

// waitForRestoredAgentReady polls a relaunched pane until its agent is up
// (restoredPaneState.ready, the check relaunch waits on) or deadline, timeout
// after the restore's relaunch began, passes. It returns nil once the agent
// is up, and otherwise why it is not. A dead or closed pane cannot come up,
// so it stops there at once. The pane is checked at least once, so a ready
// pane still gets its context after another pane used up the deadline.
func waitForRestoredAgentReady(
	paneID string,
	deadline time.Time,
	timeout time.Duration,
	pollInterval time.Duration,
	observe func(string) (restoredPaneState, error),
) error {
	for {
		state, err := observe(paneID)
		switch {
		case err != nil:
			// A failed look is retried until the deadline.
		case state.ready():
			return nil
		case state.gone:
			return errors.New("pane closed")
		case state.dead:
			return errors.New("agent " + state.failure())
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("agent not ready within %s", timeout)
		}
		time.Sleep(min(pollInterval, remaining))
	}
}

func (r *Restorer) loadPaneScrollback(sessionName, checkpointID, paneID string) (string, error) {
	return r.storage.LoadCompressedScrollback(sessionName, checkpointID, paneID)
}

func (r *Restorer) loadPaneScrollbackForPane(sessionName, checkpointID string, pane PaneState) (string, error) {
	return r.storage.LoadPaneScrollback(sessionName, checkpointID, pane)
}

// checkGitState compares current git state with checkpoint and returns a warning if different.
func (r *Restorer) checkGitState(cp *Checkpoint, workDir string) string {
	// Check if current branch matches
	branch, err := gitCommand(workDir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "could not determine current git branch"
	}

	currentBranch := trimSpace(branch)
	if currentBranch != cp.Git.Branch {
		return fmt.Sprintf("git branch mismatch: current=%s, checkpoint=%s",
			currentBranch, cp.Git.Branch)
	}

	// Check if commit matches
	commit, err := gitCommand(workDir, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}

	currentCommit := trimSpace(commit)
	if currentCommit != cp.Git.Commit {
		return fmt.Sprintf("git commit mismatch: current=%s, checkpoint=%s",
			shortHash(currentCommit), shortHash(cp.Git.Commit))
	}

	return ""
}

// truncateToLines returns the last N lines of content.
func truncateToLines(content string, maxLines int) string {
	lines := splitLines(content)
	if len(lines) <= maxLines {
		return content
	}
	return joinLines(lines[len(lines)-maxLines:])
}

// splitLines splits content into lines.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

// joinLines joins lines back together.
func joinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n")
}

// trimSpace removes leading/trailing whitespace.
func trimSpace(s string) string {
	start := 0
	end := len(s)
	for start < end && (s[start] == ' ' || s[start] == '\n' || s[start] == '\r' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\n' || s[end-1] == '\r' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

// formatContextInjection formats scrollback for injection.
func formatContextInjection(content string, checkpointTime time.Time) string {
	header := fmt.Sprintf("# Context from checkpoint (%s ago)\n\n",
		formatDuration(time.Since(checkpointTime)))
	return header + content
}

// shortHash returns the first 8 characters of a hash, or the whole string if shorter.
func shortHash(h string) string {
	if len(h) <= 8 {
		return h
	}
	return h[:8]
}

// formatDuration returns a human-readable duration.
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func validateWorkingDirectory(workDir string) error {
	info, err := os.Stat(workDir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errWorkingDirNotDirectory
	}
	return nil
}

func workingDirectoryIssue(workDir string, err error) string {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fmt.Sprintf("working directory not found: %s", workDir)
	case errors.Is(err, errWorkingDirNotDirectory):
		return fmt.Sprintf("working directory is not a directory: %s", workDir)
	default:
		return fmt.Sprintf("working directory inaccessible: %s (%v)", workDir, err)
	}
}

func effectiveRestoreDir(workDir string) string {
	if strings.TrimSpace(workDir) == "" {
		return os.TempDir()
	}
	return workDir
}

func canRestoreLegacySessionLayout(session SessionState) bool {
	return len(sessionWindowIndexesFromPanes(session.Panes)) <= 1
}

func validateSessionWindowLayouts(session SessionState) ([]string, []string) {
	windowIndexes := sessionWindowIndexesFromPanes(session.Panes)
	if len(windowIndexes) == 0 {
		return nil, nil
	}

	if len(session.WindowLayouts) == 0 {
		if session.Layout != "" && len(windowIndexes) > 1 {
			return nil, []string{"multi-window checkpoint only has a legacy single layout string; per-window layout fidelity will be lost"}
		}
		return nil, nil
	}

	validWindows := make(map[int]struct{}, len(windowIndexes))
	for _, windowIndex := range windowIndexes {
		validWindows[windowIndex] = struct{}{}
	}

	seen := make(map[int]struct{}, len(session.WindowLayouts))
	var issues []string
	for _, windowLayout := range session.WindowLayouts {
		if _, ok := seen[windowLayout.WindowIndex]; ok {
			issues = append(issues, fmt.Sprintf("duplicate window layout entry for window %d", windowLayout.WindowIndex))
			continue
		}
		seen[windowLayout.WindowIndex] = struct{}{}
		if _, ok := validWindows[windowLayout.WindowIndex]; !ok {
			issues = append(issues, fmt.Sprintf("window layout references missing window %d", windowLayout.WindowIndex))
		}
	}

	if len(windowIndexes) <= 1 {
		return issues, nil
	}
	for _, windowIndex := range windowIndexes {
		if _, ok := seen[windowIndex]; !ok {
			issues = append(issues, fmt.Sprintf("window layout missing for window %d", windowIndex))
		}
	}

	return issues, nil
}

func sortedCheckpointPanes(panes []PaneState) []PaneState {
	sorted := make([]PaneState, len(panes))
	copy(sorted, panes)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].WindowIndex != sorted[j].WindowIndex {
			return sorted[i].WindowIndex < sorted[j].WindowIndex
		}
		return sorted[i].Index < sorted[j].Index
	})
	return sorted
}

func sortedTmuxPanes(panes []tmux.Pane) []tmux.Pane {
	sorted := make([]tmux.Pane, len(panes))
	copy(sorted, panes)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].WindowIndex != sorted[j].WindowIndex {
			return sorted[i].WindowIndex < sorted[j].WindowIndex
		}
		return sorted[i].Index < sorted[j].Index
	})
	return sorted
}

func restoredPaneForCheckpointIndex(cp *Checkpoint, panes []tmux.Pane, checkpointIndex int) (tmux.Pane, bool) {
	if checkpointIndex < 0 || checkpointIndex >= len(cp.Session.Panes) {
		return tmux.Pane{}, false
	}

	sortedPanes := sortedTmuxPanes(panes)
	restoredIndex := restoredPaneIndexForCheckpointIndex(cp.Session.Panes, checkpointIndex)
	if restoredIndex < 0 || restoredIndex >= len(sortedPanes) {
		return tmux.Pane{}, false
	}
	return sortedPanes[restoredIndex], true
}

func restoredPaneIndexForCheckpointIndex(checkpointPanes []PaneState, checkpointIndex int) int {
	if checkpointIndex < 0 || checkpointIndex >= len(checkpointPanes) {
		return -1
	}

	target := checkpointPanes[checkpointIndex]
	sorted := sortedCheckpointPanes(checkpointPanes)
	for i, candidate := range sorted {
		if sameCheckpointPane(target, candidate) {
			return i
		}
	}
	return -1
}

func sameCheckpointPane(a, b PaneState) bool {
	if a.ID != "" && b.ID != "" {
		return a.ID == b.ID
	}
	return a.WindowIndex == b.WindowIndex &&
		a.Index == b.Index &&
		a.Title == b.Title &&
		a.AgentType == b.AgentType
}

func restorableAgentCommand(pane PaneState) string {
	agentType := agent.AgentType(pane.AgentType).Canonical()
	if !agentType.IsValid() || agentType == agent.AgentTypeUser || agentType == agent.AgentTypeUnknown {
		return ""
	}
	if agentType == agent.AgentTypeGrok {
		return ""
	}

	// A Muse pane records either the `muse` launcher or the binary it execs;
	// neither name carries --yolo, so both relaunch through the default below.
	// A pane recorded as its runtime (`node` for Pi) relaunches the agent too,
	// since replaying the runtime's name opens its REPL.
	command := strings.TrimSpace(pane.Command)
	bareMuseLauncher := agentType == agent.AgentTypeMuse && strings.EqualFold(command, "muse")
	if command != "" && !looksLikeShellCommand(command) && !looksLikeMuseBinary(command) && !bareMuseLauncher && !looksLikeAgentRuntime(command) {
		return command
	}

	switch agentType {
	case agent.AgentTypeClaudeCode:
		return "claude"
	case agent.AgentTypeCodex:
		return "codex"
	case agent.AgentTypeGemini:
		return "gemini"
	case agent.AgentTypeMuse:
		// A bare `muse` stops at "Do you trust this workspace?" in any
		// directory the user has not trusted by hand (--yolo does not record
		// trust), and runs with approvals on where it has been trusted.
		// --yolo, as in the spawn template, avoids both.
		return "muse --yolo"
	case agent.AgentTypePi:
		return "pi"
	case agent.AgentTypeKimi:
		return "kimi"
	case agent.AgentTypeAntigravity:
		// agy's launch binary is "agy" (distinct from the gemini CLI).
		return "agy"
	case agent.AgentTypeCursor:
		return "cursor"
	case agent.AgentTypeWindsurf:
		return "windsurf"
	case agent.AgentTypeAider:
		return "aider"
	case agent.AgentTypeOpencode:
		return "opencode"
	case agent.AgentTypeOllama:
		return "ollama"
	default:
		return ""
	}
}

// looksLikeMuseBinary reports whether command is the versioned binary that the
// `muse` launcher script execs (muse-bin-1.4.11-R6667.1, which tmux truncates
// to muse-bin-1.4.11 in pane_current_command). The truncated name is not on
// PATH and the launcher also handles auth and updates, so a restore relaunches
// through `muse` instead of replaying the recorded name.
func looksLikeMuseBinary(command string) bool {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return false
	}
	name := strings.ToLower(filepath.Base(fields[0]))
	return name == "muse-bin" || strings.HasPrefix(name, "muse-bin-")
}

// looksLikeAgentRuntime reports whether command is an interpreter that a
// script-based agent CLI runs under. pane_current_command names only the
// foreground process, so tmux on macOS records `node` for a running Pi (and
// for Kimi Code installed from npm) and a Python version for the legacy
// Python kimi-cli. No agent launches as the bare runtime; a runtime with
// arguments (`node agent.js`) is a real launch command and does not match.
func looksLikeAgentRuntime(command string) bool {
	fields := strings.Fields(command)
	if len(fields) != 1 {
		return false
	}
	name := strings.ToLower(filepath.Base(fields[0]))
	switch name {
	case "node", "bun", "deno", "python", "python3":
		return true
	default:
		return strings.HasPrefix(name, "python3.")
	}
}

func looksLikeShellCommand(command string) bool {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return false
	}

	name := strings.ToLower(filepath.Base(fields[0]))
	switch name {
	case "sh", "bash", "zsh", "fish", "dash", "ksh", "csh", "tcsh":
		return true
	default:
		return false
	}
}

// RestoreLatest restores the most recent checkpoint for a session.
func (r *Restorer) RestoreLatest(sessionName string, opts RestoreOptions) (*RestoreResult, error) {
	cp, err := r.storage.GetLatest(sessionName)
	if err != nil {
		return nil, fmt.Errorf("getting latest checkpoint: %w", err)
	}
	return r.RestoreFromCheckpoint(cp, opts)
}

// ValidateCheckpoint checks if a checkpoint can be restored.
func (r *Restorer) ValidateCheckpoint(cp *Checkpoint, opts RestoreOptions) []string {
	if cp == nil {
		return []string{ErrNilCheckpoint.Error()}
	}

	var issues []string

	// Check working directory
	workDir := cp.WorkingDir
	if opts.CustomDirectory != "" {
		workDir = opts.CustomDirectory
	}
	if workDir != "" {
		if err := validateWorkingDirectory(workDir); err != nil {
			issues = append(issues, workingDirectoryIssue(workDir, err))
		}
	}

	// Check session existence
	if tmux.SessionExists(cp.SessionName) {
		if opts.Force {
			issues = append(issues, fmt.Sprintf("session %q exists and will be killed", cp.SessionName))
		} else {
			issues = append(issues, fmt.Sprintf("session %q already exists", cp.SessionName))
		}
	}

	// Check panes
	if len(cp.Session.Panes) == 0 {
		issues = append(issues, "checkpoint contains no panes")
	}
	layoutErrors, layoutWarnings := validateSessionWindowLayouts(cp.Session)
	issues = append(issues, layoutErrors...)
	issues = append(issues, layoutWarnings...)

	// Check git state
	if !opts.SkipGitCheck && cp.Git.Commit != "" && workDir != "" {
		if warning := r.checkGitState(cp, workDir); warning != "" {
			issues = append(issues, warning)
		}
	}

	// Check scrollback files if context injection is requested
	if opts.InjectContext {
		baseDir, err := r.storage.safeCheckpointDir(cp.SessionName, cp.ID)
		if err != nil {
			issues = append(issues, fmt.Sprintf("invalid checkpoint path: %v", err))
		} else {
			for _, pane := range cp.Session.Panes {
				if pane.ScrollbackFile == "" {
					continue
				}
				scrollbackPath, err := resolveExistingCheckpointArtifactPath(baseDir, pane.ScrollbackFile)
				if err != nil {
					if errors.Is(err, os.ErrNotExist) {
						issues = append(issues,
							fmt.Sprintf("scrollback file missing for pane %s", pane.ID))
					} else {
						issues = append(issues,
							fmt.Sprintf("invalid scrollback path for pane %s: %v", pane.ID, err))
					}
					continue
				}
				if _, err := os.Stat(scrollbackPath); os.IsNotExist(err) {
					issues = append(issues,
						fmt.Sprintf("scrollback file missing for pane %s", pane.ID))
				}
			}
		}
	}

	return issues
}
