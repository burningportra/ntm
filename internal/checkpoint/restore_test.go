package checkpoint

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/agent"
	"github.com/Dicklesworthstone/ntm/internal/tmux"
	"github.com/Dicklesworthstone/ntm/tests/testutil"
)

func TestRestoreOptions_Defaults(t *testing.T) {
	opts := RestoreOptions{}

	// All options should be false by default
	if opts.Force {
		t.Error("Force should be false by default")
	}
	if opts.SkipGitCheck {
		t.Error("SkipGitCheck should be false by default")
	}
	if opts.InjectContext {
		t.Error("InjectContext should be false by default")
	}
	if opts.DryRun {
		t.Error("DryRun should be false by default")
	}
	if opts.CustomDirectory != "" {
		t.Error("CustomDirectory should be empty by default")
	}
}

func TestRestoreResult_Fields(t *testing.T) {
	result := &RestoreResult{
		SessionName:     "test-session",
		PanesRestored:   3,
		ContextInjected: true,
		Warnings:        []string{"warning1", "warning2"},
		DryRun:          false,
	}

	if result.SessionName != "test-session" {
		t.Errorf("SessionName = %q, want %q", result.SessionName, "test-session")
	}
	if result.PanesRestored != 3 {
		t.Errorf("PanesRestored = %d, want %d", result.PanesRestored, 3)
	}
	if !result.ContextInjected {
		t.Error("ContextInjected should be true")
	}
	if len(result.Warnings) != 2 {
		t.Errorf("len(Warnings) = %d, want %d", len(result.Warnings), 2)
	}
}

func TestNewRestorer(t *testing.T) {
	r := NewRestorer()
	if r == nil {
		t.Fatal("NewRestorer() returned nil")
	}
	if r.storage == nil {
		t.Error("Restorer.storage should not be nil")
	}
}

func TestNewRestorerWithStorage(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "ntm-restore-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	storage := NewStorageWithDir(tmpDir)
	r := NewRestorerWithStorage(storage)

	if r.storage != storage {
		t.Error("Restorer should use provided storage")
	}
}

func TestRestorer_RestoreFromCheckpoint_DirectoryNotFound(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "ntm-restore-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	r := NewRestorerWithStorage(NewStorageWithDir(tmpDir))

	cp := &Checkpoint{
		ID:          "test-checkpoint",
		SessionName: "test-session",
		WorkingDir:  "/nonexistent/path/that/does/not/exist",
		Session: SessionState{
			Panes: []PaneState{{Index: 0, ID: "%0"}},
		},
	}

	_, err = r.RestoreFromCheckpoint(cp, RestoreOptions{})
	if err == nil {
		t.Error("RestoreFromCheckpoint should fail for nonexistent directory")
	}
}

func TestRestorer_RestoreFromCheckpoint_NoPanes(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "ntm-restore-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	r := NewRestorerWithStorage(NewStorageWithDir(tmpDir))

	cp := &Checkpoint{
		ID:          "test-checkpoint",
		SessionName: "test-session",
		WorkingDir:  tmpDir,
		Session: SessionState{
			Panes: []PaneState{}, // Empty panes
		},
	}

	_, err = r.RestoreFromCheckpoint(cp, RestoreOptions{})
	if err != ErrNoAgentsToRestore {
		t.Errorf("Expected ErrNoAgentsToRestore, got: %v", err)
	}
}

func TestRestorer_RestoreFromCheckpointRejectsGrokBeforeMutation(t *testing.T) {
	tests := []struct {
		name  string
		panes []PaneState
	}{
		{
			name:  "grok only",
			panes: []PaneState{{Index: 0, AgentType: "grok", Command: "grok"}},
		},
		{
			name: "mixed batch",
			panes: []PaneState{
				{Index: 0, AgentType: "cc", Command: "claude"},
				{Index: 1, AgentType: "grok-build", Command: "grok"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cp := &Checkpoint{
				SessionName: "definitely-not-an-existing-ntm-checkpoint-session",
				WorkingDir:  "/definitely/not/a/real/checkpoint/directory",
				Session:     SessionState{Panes: tt.panes},
			}
			result, err := NewRestorerWithStorage(NewStorageWithDir(t.TempDir())).RestoreFromCheckpoint(cp, RestoreOptions{Force: true})
			if !errors.Is(err, agent.ErrAutomatedRelaunchNotImplemented) {
				t.Fatalf("RestoreFromCheckpoint() error = %v, want Grok relaunch sentinel", err)
			}
			if result != nil {
				t.Fatalf("RestoreFromCheckpoint() result = %+v, want nil before restore planning", result)
			}
		})
	}
}

func TestRestorableAgentCommandRejectsGrokEvenWithCapturedCommand(t *testing.T) {
	if got := restorableAgentCommand(PaneState{AgentType: "grok", Command: "claude"}); got != "" {
		t.Fatalf("restorableAgentCommand(grok) = %q, want empty unsupported command", got)
	}
}

func TestRestorer_RestoreFromCheckpoint_DryRun(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "ntm-restore-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	r := NewRestorerWithStorage(NewStorageWithDir(tmpDir))

	cp := &Checkpoint{
		ID:          "test-checkpoint",
		SessionName: "test-dryrun-session",
		WorkingDir:  tmpDir,
		Session: SessionState{
			Panes: []PaneState{
				{Index: 0, ID: "%0", Title: "pane1"},
				{Index: 1, ID: "%1", Title: "pane2"},
			},
		},
	}

	result, err := r.RestoreFromCheckpoint(cp, RestoreOptions{DryRun: true})
	if err != nil {
		t.Fatalf("RestoreFromCheckpoint(DryRun) failed: %v", err)
	}

	if !result.DryRun {
		t.Error("Result.DryRun should be true")
	}
	if result.PanesRestored != 2 {
		t.Errorf("PanesRestored = %d, want 2", result.PanesRestored)
	}
}

func TestRestorer_RestoreFromCheckpoint_DryRun_CustomDirectory(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "ntm-restore-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	r := NewRestorerWithStorage(NewStorageWithDir(tmpDir))

	cp := &Checkpoint{
		ID:          "test-checkpoint",
		SessionName: "test-session",
		WorkingDir:  "/original/nonexistent/path",
		Session: SessionState{
			Panes: []PaneState{{Index: 0, ID: "%0"}},
		},
	}

	// Should succeed with custom directory override
	result, err := r.RestoreFromCheckpoint(cp, RestoreOptions{
		DryRun:          true,
		CustomDirectory: tmpDir,
	})
	if err != nil {
		t.Fatalf("RestoreFromCheckpoint with CustomDirectory failed: %v", err)
	}

	if result.PanesRestored != 1 {
		t.Errorf("PanesRestored = %d, want 1", result.PanesRestored)
	}
}

func TestRestorer_RestoreFromCheckpoint_WorkingDirNotDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	r := NewRestorerWithStorage(NewStorageWithDir(tmpDir))

	filePath := filepath.Join(tmpDir, "not-a-dir")
	if err := os.WriteFile(filePath, []byte("data"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	cp := &Checkpoint{
		ID:          "test-checkpoint",
		SessionName: "test-session",
		WorkingDir:  filePath,
		Session: SessionState{
			Panes: []PaneState{{Index: 0, ID: "%0"}},
		},
	}

	_, err := r.RestoreFromCheckpoint(cp, RestoreOptions{})
	if !errors.Is(err, ErrWorkingDirInvalid) {
		t.Fatalf("RestoreFromCheckpoint should fail with ErrWorkingDirInvalid, got %v", err)
	}
}

func TestRestorer_ValidateCheckpoint_DirectoryNotFound(t *testing.T) {
	r := NewRestorer()

	cp := &Checkpoint{
		WorkingDir: "/nonexistent/path",
		Session: SessionState{
			Panes: []PaneState{{Index: 0}},
		},
	}

	issues := r.ValidateCheckpoint(cp, RestoreOptions{})

	found := false
	for _, issue := range issues {
		if containsSubstr(issue, "directory not found") {
			found = true
			break
		}
	}
	if !found {
		t.Error("Expected directory not found issue")
	}
}

func TestRestorer_ValidateCheckpoint_NoPanes(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "ntm-restore-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	r := NewRestorer()

	cp := &Checkpoint{
		WorkingDir: tmpDir,
		Session: SessionState{
			Panes: []PaneState{}, // Empty
		},
	}

	issues := r.ValidateCheckpoint(cp, RestoreOptions{})

	found := false
	for _, issue := range issues {
		if containsSubstr(issue, "no panes") {
			found = true
			break
		}
	}
	if !found {
		t.Error("Expected 'no panes' issue")
	}
}

func TestRestorer_ValidateCheckpoint_WorkingDirNotDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "not-a-dir")
	if err := os.WriteFile(filePath, []byte("data"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	r := NewRestorerWithStorage(NewStorageWithDir(tmpDir))
	cp := &Checkpoint{
		WorkingDir: filePath,
		Session: SessionState{
			Panes: []PaneState{{Index: 0}},
		},
	}

	issues := r.ValidateCheckpoint(cp, RestoreOptions{})

	found := false
	for _, issue := range issues {
		if containsSubstr(issue, "not a directory") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected not-a-directory issue, got %v", issues)
	}
}

func TestRestorer_ValidateCheckpoint_MissingScrollback(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "ntm-restore-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	storage := NewStorageWithDir(tmpDir)
	r := NewRestorerWithStorage(storage)

	// Create a checkpoint directory without scrollback files
	cp := &Checkpoint{
		ID:          "test-checkpoint",
		SessionName: "test-session",
		WorkingDir:  tmpDir,
		Session: SessionState{
			Panes: []PaneState{
				{
					Index:          0,
					ID:             "%0",
					ScrollbackFile: "panes/pane_0.txt", // File doesn't exist
				},
			},
		},
	}

	// Create the checkpoint directory
	cpDir := storage.CheckpointDir(cp.SessionName, cp.ID)
	if err := os.MkdirAll(cpDir, 0755); err != nil {
		t.Fatalf("Failed to create checkpoint dir: %v", err)
	}

	issues := r.ValidateCheckpoint(cp, RestoreOptions{InjectContext: true})

	found := false
	for _, issue := range issues {
		if containsSubstr(issue, "scrollback file missing") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Expected 'scrollback file missing' issue, got: %v", issues)
	}
}

func TestRestorer_ValidateCheckpoint_WindowLayoutReferencesMissingWindow(t *testing.T) {
	r := NewRestorer()

	cp := &Checkpoint{
		WorkingDir: t.TempDir(),
		Session: SessionState{
			Panes: []PaneState{
				{Index: 0, WindowIndex: 0},
				{Index: 0, WindowIndex: 1},
			},
			WindowLayouts: []WindowLayoutState{
				{WindowIndex: 0, Layout: "even-horizontal"},
				{WindowIndex: 9, Layout: "main-vertical"},
			},
		},
	}

	issues := r.ValidateCheckpoint(cp, RestoreOptions{})

	found := false
	for _, issue := range issues {
		if containsSubstr(issue, "references missing window 9") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected missing-window layout issue, got %v", issues)
	}
}

func TestRestorer_ValidateCheckpoint_WindowLayoutMissingForExistingWindow(t *testing.T) {
	r := NewRestorer()

	cp := &Checkpoint{
		WorkingDir: t.TempDir(),
		Session: SessionState{
			Panes: []PaneState{
				{Index: 0, WindowIndex: 0},
				{Index: 0, WindowIndex: 1},
			},
			WindowLayouts: []WindowLayoutState{
				{WindowIndex: 0, Layout: "even-horizontal"},
			},
		},
	}

	issues := r.ValidateCheckpoint(cp, RestoreOptions{})

	found := false
	for _, issue := range issues {
		if containsSubstr(issue, "window layout missing for window 1") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected missing-layout issue, got %v", issues)
	}
}

func TestRestorer_ValidateCheckpoint_LegacyLayoutWarnsForMultiWindowSession(t *testing.T) {
	r := NewRestorer()

	cp := &Checkpoint{
		WorkingDir: t.TempDir(),
		Session: SessionState{
			Panes: []PaneState{
				{Index: 0, WindowIndex: 0},
				{Index: 0, WindowIndex: 1},
			},
			Layout: "even-horizontal",
		},
	}

	issues := r.ValidateCheckpoint(cp, RestoreOptions{})

	found := false
	for _, issue := range issues {
		if containsSubstr(issue, "legacy single layout string") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected legacy-layout warning, got %v", issues)
	}
}

func TestRestorer_RestoreFromCheckpoint_SkipsLegacyLayoutForMultiWindowSession(t *testing.T) {
	testutil.RequireTmuxThrottled(t)

	workDir := t.TempDir()
	r := NewRestorerWithStorage(NewStorageWithDir(t.TempDir()))
	// The sleep stand-ins never draw a ready screen, so the restore waits out
	// its ready deadline; a short one keeps the test fast.
	r.agentReadyTimeout = time.Second
	sessionName := "cplegacy-layout-" + time.Now().Format("150405000000")

	cp := &Checkpoint{
		ID:          "legacy-layout-checkpoint",
		SessionName: sessionName,
		WorkingDir:  workDir,
		Session: SessionState{
			Panes: []PaneState{
				{
					Index:       0,
					WindowIndex: 0,
					Title:       sessionName + "__cc_1",
					AgentType:   "cc",
					Command:     "sleep 30",
				},
				{
					Index:       0,
					WindowIndex: 1,
					Title:       sessionName + "__cod_1",
					AgentType:   "cod",
					Command:     "sleep 30",
				},
			},
			Layout: "even-horizontal",
		},
	}

	t.Cleanup(func() {
		if tmux.SessionExists(sessionName) {
			_ = tmux.KillSession(sessionName)
		}
	})

	result, err := r.RestoreFromCheckpoint(cp, RestoreOptions{})
	if err != nil {
		t.Fatalf("RestoreFromCheckpoint failed: %v", err)
	}

	found := false
	for _, warning := range result.Warnings {
		if containsSubstr(warning, "skipping legacy single layout for multi-window checkpoint") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected legacy-layout skip warning, got %v", result.Warnings)
	}
}

// TestRestorer_InjectContextSubmitsToAgentPanesOnly restores a checkpoint with
// context injection into panes whose "agent" draws an idle composer, turns on
// bracketed paste and records its raw input, plus a user pane. Each agent's
// context message has to arrive as one paste followed by the shared
// protocol's two Enters; the old path pasted it and pressed Enter once, 100 ms
// later, whatever the agent. The user pane is a shell, so it must get nothing:
// its scrollback is a command that creates a file if the shell runs it.
func TestRestorer_InjectContextSubmitsToAgentPanesOnly(t *testing.T) {
	testutil.RequireTmuxThrottled(t)

	workDir := t.TempDir()
	storage := NewStorageWithDir(t.TempDir())
	r := NewRestorerWithStorage(storage)
	sessionName := "cpinject-" + time.Now().Format("150405000000")
	const checkpointID = "inject-context-checkpoint"
	marker := filepath.Join(workDir, "user-pane-ran-its-scrollback")

	cp := &Checkpoint{
		ID:          checkpointID,
		SessionName: sessionName,
		WorkingDir:  workDir,
		CreatedAt:   time.Now(),
	}
	addPane := func(window int, agentType, command, scrollback string) {
		paneID := fmt.Sprintf("%%%d", window)
		rel, err := storage.SaveScrollback(sessionName, checkpointID, paneID, scrollback)
		if err != nil {
			t.Fatalf("SaveScrollback(%s): %v", agentType, err)
		}
		cp.Session.Panes = append(cp.Session.Panes, PaneState{
			WindowIndex:    window,
			ID:             paneID,
			Title:          fmt.Sprintf("%s__%s_1", sessionName, agentType),
			AgentType:      agentType,
			Command:        command,
			ScrollbackFile: rel,
		})
		cp.Session.WindowLayouts = append(cp.Session.WindowLayouts, WindowLayoutState{WindowIndex: window, Layout: "tiled"})
	}

	agentTypes := []string{"cc", "cod", "muse"}
	received := make(map[string]string, len(agentTypes))
	for window, agentType := range agentTypes {
		dir := filepath.Join(workDir, agentType)
		if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		received[agentType] = filepath.Join(dir, "received.txt")
		recorder := writeRestoreInputRecorder(t, dir, received[agentType], restoreTestReadyScreens[agentType], "")
		addPane(window, agentType, recorder, agentType+" line one\n"+agentType+" line two")
	}
	addPane(len(agentTypes), "user", "", "touch "+marker)

	t.Cleanup(func() {
		if tmux.SessionExists(sessionName) {
			_ = tmux.KillSession(sessionName)
		}
	})

	result, err := r.RestoreFromCheckpoint(cp, RestoreOptions{InjectContext: true})
	if err != nil {
		t.Fatalf("RestoreFromCheckpoint failed: %v", err)
	}
	if !result.ContextInjected {
		t.Fatalf("context was not injected; warnings: %v", result.Warnings)
	}

	for _, agentType := range agentTypes {
		want := restoreTestPastedContext(agentType+" line one", agentType+" line two")
		if got := waitForRestoreInput(received[agentType], want, 5*time.Second); got != want {
			t.Errorf("%s pane input = %q, want %q", agentType, got, want)
		}
	}

	// Give a shell that got the paste time to start and run it.
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("user pane ran its pasted scrollback as a shell command (stat err = %v)", err)
		}
	}
}

// TestRestorer_InjectContextWaitsForAgentReadiness restores two agents whose
// processes start at once but whose TUIs differ: the Muse pane draws its idle
// screen and turns on bracketed paste two seconds later, as a real TUI takes a
// moment after its process starts, and the Codex pane never draws anything.
// The Muse pane must get its context only once it is up, so the paste arrives
// bracketed; the old restore submitted right after relaunch, while the TUI was
// still starting, and the paste arrived bare. The Codex pane must get nothing
// and be reported.
func TestRestorer_InjectContextWaitsForAgentReadiness(t *testing.T) {
	testutil.RequireTmuxThrottled(t)

	workDir := t.TempDir()
	storage := NewStorageWithDir(t.TempDir())
	r := NewRestorerWithStorage(storage)
	r.agentReadyTimeout = 4 * time.Second
	sessionName := "cpready-" + time.Now().Format("150405000000")
	const checkpointID = "inject-ready-checkpoint"

	cp := &Checkpoint{
		ID:          checkpointID,
		SessionName: sessionName,
		WorkingDir:  workDir,
		CreatedAt:   time.Now(),
	}
	received := map[string]string{}
	for window, agentType := range []string{"muse", "cod"} {
		dir := filepath.Join(workDir, agentType)
		if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		received[agentType] = filepath.Join(dir, "received.txt")
		screen, delay := "", ""
		if agentType == "muse" {
			screen, delay = restoreTestReadyScreens["muse"], "2"
		}
		recorder := writeRestoreInputRecorder(t, dir, received[agentType], screen, delay)

		paneID := fmt.Sprintf("%%%d", window)
		rel, err := storage.SaveScrollback(sessionName, checkpointID, paneID, agentType+" scrollback")
		if err != nil {
			t.Fatalf("SaveScrollback(%s): %v", agentType, err)
		}
		cp.Session.Panes = append(cp.Session.Panes, PaneState{
			WindowIndex:    window,
			ID:             paneID,
			Title:          fmt.Sprintf("%s__%s_1", sessionName, agentType),
			AgentType:      agentType,
			Command:        recorder,
			ScrollbackFile: rel,
		})
		cp.Session.WindowLayouts = append(cp.Session.WindowLayouts, WindowLayoutState{WindowIndex: window, Layout: "tiled"})
	}

	t.Cleanup(func() {
		if tmux.SessionExists(sessionName) {
			_ = tmux.KillSession(sessionName)
		}
	})

	result, err := r.RestoreFromCheckpoint(cp, RestoreOptions{InjectContext: true})
	if err != nil {
		t.Fatalf("RestoreFromCheckpoint failed: %v", err)
	}
	if result.ContextInjected {
		t.Error("ContextInjected = true although the Codex pane never became ready")
	}
	// Relaunch reports the Codex agent started but not ready, and injection
	// reports its context not injected.
	var notReady []string
	for _, warning := range result.Warnings {
		if strings.Contains(warning, "not ready") {
			notReady = append(notReady, warning)
		}
	}
	wantWarnings := []string{
		sessionName + `__cod_1): agent "cat" started but not ready within 4s`,
		sessionName + "__cod_1): agent not ready within 4s; context not injected",
	}
	if len(notReady) != len(wantWarnings) {
		t.Errorf("not-ready warnings = %q, want %d ending %q (all warnings: %q)", notReady, len(wantWarnings), wantWarnings, result.Warnings)
	} else {
		for i, want := range wantWarnings {
			if !strings.HasSuffix(notReady[i], want) {
				t.Errorf("not-ready warning %d = %q, want it ending %q", i, notReady[i], want)
			}
		}
	}

	want := restoreTestPastedContext("muse scrollback")
	if got := waitForRestoreInput(received["muse"], want, 5*time.Second); got != want {
		t.Errorf("muse pane input = %q, want %q", got, want)
	}
	time.Sleep(time.Second)
	if data, _ := os.ReadFile(received["cod"]); len(data) != 0 {
		t.Errorf("codex pane that never became ready got input %q", data)
	}
}

// TestRestorer_RelaunchWaitsForSlowLaunchers restores three agent panes whose
// launchers behave like real ones: two stay in their shell for 3 s and 10 s
// (an update check, a version-manager shim) before exec'ing an agent that
// draws its idle screen, and one exits with an error at once. Each slow agent
// must be launched once and not reported; the old restore gave a pane 2 s to
// show its command, then killed the launcher and launched it again, three
// times, and reported it failed. The failing launcher must be launched three
// times and reported, and its pane kept, dead, so the failure stays visible;
// the old restore lost the pane when the launcher exited and never retried.
func TestRestorer_RelaunchWaitsForSlowLaunchers(t *testing.T) {
	testutil.RequireTmuxThrottled(t)

	workDir := t.TempDir()
	r := NewRestorerWithStorage(NewStorageWithDir(t.TempDir()))
	r.agentReadyTimeout = 20 * time.Second
	sessionName := "cpslow-" + time.Now().Format("150405000000")

	cp := &Checkpoint{
		ID:          "slow-launcher-checkpoint",
		SessionName: sessionName,
		WorkingDir:  workDir,
		CreatedAt:   time.Now(),
	}
	launchers := []struct {
		agentType string
		// then runs after the launcher records its launch; %[1]q is the agent.
		then string
	}{
		{agentType: "cc", then: "sleep 3\nexec %[1]q\n"},
		{agentType: "muse", then: "sleep 10\nexec %[1]q\n"},
		{agentType: "cod", then: "echo 'launcher: update check failed' >&2\nexit 1\n"},
	}
	launchLogs := map[string]string{}
	for window, l := range launchers {
		dir := filepath.Join(workDir, l.agentType)
		if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		agentPath := writeRestoreInputRecorder(t, dir, filepath.Join(dir, "received.txt"), restoreTestReadyScreens[l.agentType], "")
		launchLogs[l.agentType] = filepath.Join(dir, "launches")
		launcher := filepath.Join(dir, "bin", "launch")
		script := fmt.Sprintf("#!/bin/sh\necho launched >> %q\n", launchLogs[l.agentType]) + fmt.Sprintf(l.then, agentPath)
		if err := os.WriteFile(launcher, []byte(script), 0o755); err != nil {
			t.Fatalf("write launcher: %v", err)
		}
		cp.Session.Panes = append(cp.Session.Panes, PaneState{
			WindowIndex: window,
			ID:          fmt.Sprintf("%%%d", window),
			Title:       fmt.Sprintf("%s__%s_1", sessionName, l.agentType),
			AgentType:   l.agentType,
			Command:     launcher,
		})
		cp.Session.WindowLayouts = append(cp.Session.WindowLayouts, WindowLayoutState{WindowIndex: window, Layout: "tiled"})
	}

	t.Cleanup(func() {
		if tmux.SessionExists(sessionName) {
			_ = tmux.KillSession(sessionName)
		}
	})

	result, err := r.RestoreFromCheckpoint(cp, RestoreOptions{})
	if err != nil {
		t.Fatalf("RestoreFromCheckpoint failed: %v", err)
	}

	var agentWarnings []string
	for _, warning := range result.Warnings {
		if strings.HasPrefix(warning, "agent restoration incomplete") {
			agentWarnings = append(agentWarnings, warning)
		}
	}
	wantWarning := sessionName + "__cod_1): agent failed to start in 3 attempts; the last exited with status 1"
	if len(agentWarnings) != 1 || !strings.HasSuffix(agentWarnings[0], wantWarning) {
		t.Errorf("agent warnings = %q, want one ending %q", agentWarnings, wantWarning)
	}

	for agentType, want := range map[string]int{"cc": 1, "muse": 1, "cod": 3} {
		data, _ := os.ReadFile(launchLogs[agentType])
		if got := strings.Count(string(data), "launched\n"); got != want {
			t.Errorf("%s launcher ran %d times, want %d", agentType, got, want)
		}
	}

	panes, err := tmux.GetPanes(sessionName)
	if err != nil {
		t.Fatalf("GetPanes failed: %v", err)
	}
	byTitle := map[string]tmux.Pane{}
	for _, pane := range panes {
		byTitle[pane.Title] = pane
	}
	for _, agentType := range []string{"cc", "muse"} {
		if pane, ok := byTitle[sessionName+"__"+agentType+"_1"]; !ok || pane.Command != "cat" {
			t.Errorf("%s pane = %+v (found %v), want it running its agent (cat)", agentType, pane, ok)
		}
	}
	failed, ok := byTitle[sessionName+"__cod_1"]
	if !ok {
		t.Fatalf("failed launcher's pane is gone; panes: %+v", panes)
	}
	if dead, err := tmux.DefaultClient.Run("display-message", "-p", "-t", failed.ID, "#{pane_dead}"); err != nil || dead != "1" {
		t.Errorf("failed launcher's pane_dead = %q (err %v), want 1", dead, err)
	}
}

// restoreTestReadyScreens are idle screens that agent.IsReadyOutput accepts,
// as printf format strings.
var restoreTestReadyScreens = map[string]string{
	"cc":   `────────\n❯\n────────\n  ⏵⏵ bypass permissions on (shift+tab to cycle)\n`,
	"cod":  `›\n`,
	"muse": `Muse Code 1.4.11\n\n────────\n❯\n────────\n  muse-wonder-1.0 · /repo · YOLO\n`,
}

// writeRestoreInputRecorder writes a stand-in agent into dir/bin that records
// its raw input in received. It draws screen and turns on bracketed paste
// after delay seconds (at once when delay is empty, never when screen is
// empty) while the recording starts at once, as an agent process starts
// before its TUI is up. The recorder is named cat so the restore's wait for
// the relaunched command sees it once the script execs cat. stty -echo keeps
// the input off the screen, where the Claude and Codex submission checks
// would read it as a stranded composer.
func writeRestoreInputRecorder(t *testing.T, dir, received, screen, delay string) string {
	t.Helper()
	draw := ""
	switch {
	case screen != "" && delay == "":
		draw = fmt.Sprintf("printf '\\033[?2004h%s'\n", screen)
	case screen != "":
		draw = fmt.Sprintf("(sleep %s; printf '\\033[?2004h%s') &\n", delay, screen)
	}
	recorder := filepath.Join(dir, "bin", "cat")
	script := fmt.Sprintf("#!/bin/sh\nstty -echo\n%sexec /bin/cat -uv > %q\n", draw, received)
	if err := os.WriteFile(recorder, []byte(script), 0o755); err != nil {
		t.Fatalf("write recorder: %v", err)
	}
	return recorder
}

// restoreTestPastedContext is what a recorder receives for a restored pane's
// context message: one bracketed paste and the shared protocol's two Enters.
// The tty turns the paste's CRs and each Enter into newlines.
func restoreTestPastedContext(scrollback ...string) string {
	return "^[[200~# Context from checkpoint (just now ago)\n\n" +
		strings.Join(scrollback, "\n") + "^[[201~\n\n"
}

// waitForRestoreInput polls a recorder's input file until it holds want or
// timeout passes, and returns what it last read.
func waitForRestoreInput(path, want string, timeout time.Duration) string {
	var got string
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		data, _ := os.ReadFile(path)
		if got = string(data); got == want {
			break
		}
	}
	return got
}

// restoreTestPiScreen is a fresh Pi 1.3.0 screen (`pi --offline --no-session
// --no-extensions` in a 40-column pane): an empty editor above the footer,
// with no prompt glyph, banner or "ready" status.
var restoreTestPiScreen = strings.Repeat("─", 40) + "\n\n" + strings.Repeat("─", 40) + "\n" +
	"/private/tmp/claude-501/-Volumes-1tb-...\n" +
	"0.0%/1.0M (auto)  google/gemini-4-argon"

func TestWaitForRestoredAgentReady(t *testing.T) {
	const museScreen = "Muse Code 1.4.11\n\n❯\n  muse-wonder-1.0 · /repo · YOLO"
	tests := []struct {
		name string
		// agentType is the type the pane was relaunched as; Muse when empty.
		agentType agent.AgentType
		// screens are the observed outputs in order; the last one repeats.
		screens  []string
		command  string
		dead     bool
		gone     bool
		deadline time.Duration
		// want is the returned error's text; empty for a ready agent.
		want string
		// minObservations and maxObservations bound how many observations
		// the wait takes; zero is unbounded.
		minObservations, maxObservations int
	}{
		{name: "ready once the TUI draws", screens: []string{"", "", museScreen}, command: "muse-bin-1.4.11", deadline: 2 * time.Second, minObservations: 3},
		{name: "never draws", screens: []string{""}, command: "cat", deadline: 50 * time.Millisecond, want: "agent not ready within 50ms"},
		{name: "shell prompt under a shell", screens: []string{"% "}, command: "zsh", deadline: 50 * time.Millisecond, want: "agent not ready within 50ms"},
		{name: "ready text while the launcher shell still runs", screens: []string{"Update ready"}, command: "bash", deadline: 50 * time.Millisecond, want: "agent not ready within 50ms"},
		{name: "dead pane keeps its agent's screen and name, and stops the wait", screens: []string{museScreen}, command: "muse-bin-1.4.11", dead: true, deadline: 5 * time.Second, want: "agent exited", maxObservations: 1},
		{name: "closed pane stops the wait", screens: []string{""}, gone: true, deadline: 5 * time.Second, want: "pane closed", maxObservations: 1},
		{name: "checked once after the deadline", screens: []string{museScreen}, command: "muse-bin-1.4.11", deadline: -time.Second, minObservations: 1},
		// bd-1zadt: Pi's screen carries none of the generic indicators, so
		// only its own editor recognizer sees it come up.
		{name: "pi ready once its editor draws", agentType: agent.AgentTypePi, screens: []string{"", restoreTestPiScreen}, command: "node", deadline: 2 * time.Second, minObservations: 2},
		{name: "pi shell prompt is not ready", agentType: agent.AgentTypePi, screens: []string{"$ "}, command: "node", deadline: 50 * time.Millisecond, want: "agent not ready within 50ms"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			agentType := tc.agentType
			if agentType == "" {
				agentType = agent.AgentTypeMuse
			}
			observations := 0
			observe := func(paneID string) (restoredPaneState, error) {
				if paneID != "%7" {
					t.Errorf("observe(%q), want observe(%%7)", paneID)
				}
				screen := tc.screens[min(observations, len(tc.screens)-1)]
				observations++
				return restoredPaneState{gone: tc.gone, dead: tc.dead, command: tc.command, output: screen}, nil
			}

			err := waitForRestoredAgentReady("%7", agentType, time.Now().Add(tc.deadline), max(tc.deadline, 0), time.Millisecond, observe)
			got := ""
			if err != nil {
				got = err.Error()
			}
			if got != tc.want {
				t.Fatalf("waitForRestoredAgentReady() = %q, want %q", got, tc.want)
			}
			if observations < tc.minObservations || (tc.maxObservations > 0 && observations > tc.maxObservations) {
				t.Fatalf("took %d observations, want between %d and %d", observations, tc.minObservations, tc.maxObservations)
			}
		})
	}
}

// TestRelaunchRestoredPanes drives one pane's relaunch through scripted
// observations: per launch, what the pane shows on each poll.
func TestRelaunchRestoredPanes(t *testing.T) {
	const museScreen = "Muse Code 1.4.11\n\n❯\n  muse-wonder-1.0 · /repo · YOLO"
	var (
		inShell  = restoredPaneState{command: "bash"}
		wrapper  = restoredPaneState{command: "env"}
		started  = restoredPaneState{command: "muse-bin-1.4.11"}
		up       = restoredPaneState{command: "muse-bin-1.4.11", output: museScreen}
		exited   = restoredPaneState{dead: true, exitStatus: "1", command: "bash"}
		killed   = restoredPaneState{dead: true, command: "muse-bin-1.4.11"}
		gone     = restoredPaneState{gone: true}
		promptUp = restoredPaneState{command: "bash", output: "% "}
	)
	tests := []struct {
		name string
		// launches lists, per launch, the pane's observations in order; the
		// last one repeats.
		launches   [][]restoredPaneState
		respawnErr error
		deadline   time.Duration
		// wantLaunches is how many times the agent must be launched.
		wantLaunches int
		// wantErr is the launch's outcome; empty for an agent that came up.
		wantErr string
	}{
		{
			name:         "launcher stays in its shell for many polls, then the agent comes up",
			launches:     [][]restoredPaneState{{inShell, inShell, inShell, inShell, inShell, inShell, started, started, up}},
			deadline:     5 * time.Second,
			wantLaunches: 1,
		},
		{
			name:         "launcher still in its shell at the deadline is left running",
			launches:     [][]restoredPaneState{{inShell}},
			deadline:     50 * time.Millisecond,
			wantLaunches: 1,
			wantErr:      "agent not started within 50ms; its launcher (bash) is still running",
		},
		{
			name:         "shell prompt is not an agent coming up",
			launches:     [][]restoredPaneState{{promptUp}},
			deadline:     50 * time.Millisecond,
			wantLaunches: 1,
			wantErr:      "agent not started within 50ms; its launcher (bash) is still running",
		},
		{
			name:         "agent started but not ready at the deadline is left running",
			launches:     [][]restoredPaneState{{started}},
			deadline:     50 * time.Millisecond,
			wantLaunches: 1,
			wantErr:      `agent "muse-bin-1.4.11" started but not ready within 50ms`,
		},
		{
			name:         "launcher exits, the second launch comes up",
			launches:     [][]restoredPaneState{{inShell, exited}, {inShell, up}},
			deadline:     5 * time.Second,
			wantLaunches: 2,
		},
		{
			name:         "launcher exits on every launch",
			launches:     [][]restoredPaneState{{exited}},
			deadline:     5 * time.Second,
			wantLaunches: 3,
			wantErr:      "agent failed to start in 3 attempts; the last exited with status 1",
		},
		{
			name:         "agent killed by a signal on every launch",
			launches:     [][]restoredPaneState{{started, killed}},
			deadline:     5 * time.Second,
			wantLaunches: 3,
			wantErr:      "agent failed to start in 3 attempts; the last exited",
		},
		{
			name:         "agent back in a shell after it started is launched again",
			launches:     [][]restoredPaneState{{started, started, inShell}, {up}},
			deadline:     5 * time.Second,
			wantLaunches: 2,
		},
		{
			name:         "agent back in a shell after it started on every launch",
			launches:     [][]restoredPaneState{{started, started, inShell}},
			deadline:     5 * time.Second,
			wantLaunches: 3,
			wantErr:      "agent failed to start in 3 attempts; the last returned to a shell (bash) after its agent started",
		},
		{
			name:         "wrapper seen once before the launcher's shell is not an agent that started",
			launches:     [][]restoredPaneState{{wrapper, inShell, inShell, up}},
			deadline:     5 * time.Second,
			wantLaunches: 1,
		},
		{
			name:         "pane closed",
			launches:     [][]restoredPaneState{{inShell, gone}},
			deadline:     5 * time.Second,
			wantLaunches: 1,
			wantErr:      "pane closed while its agent was starting",
		},
		{
			name:         "respawn fails",
			launches:     [][]restoredPaneState{{up}},
			respawnErr:   errors.New("can't find pane: %3"),
			deadline:     5 * time.Second,
			wantLaunches: 1,
			wantErr:      "launching agent: can't find pane: %3",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			launches, polls := 0, 0
			ops := restoredPaneOps{
				respawn: func(paneID, command string) error {
					if paneID != "%3" || command != "muse --yolo" {
						t.Errorf("respawn(%q, %q), want respawn(%%3, muse --yolo)", paneID, command)
					}
					launches++
					polls = 0
					return tc.respawnErr
				},
				observe: func(paneID string) (restoredPaneState, error) {
					script := tc.launches[min(launches, len(tc.launches))-1]
					state := script[min(polls, len(script)-1)]
					polls++
					return state, nil
				},
			}
			launch := &restoredLaunch{paneID: "%3", command: "muse --yolo"}

			relaunchRestoredPanes([]*restoredLaunch{launch}, time.Now().Add(tc.deadline), tc.deadline, time.Millisecond, ops)

			if launches != tc.wantLaunches || launch.attempts != tc.wantLaunches {
				t.Errorf("launched %d times (attempts %d), want %d", launches, launch.attempts, tc.wantLaunches)
			}
			gotErr := ""
			if launch.err != nil {
				gotErr = launch.err.Error()
			}
			if gotErr != tc.wantErr {
				t.Errorf("outcome = %q, want %q", gotErr, tc.wantErr)
			}
		})
	}
}

// TestRelaunchRestoredPanes_ReadsReadinessAsThePaneType is the bd-1zadt
// regression: a restored Pi pane waited out the whole deadline and was
// reported as started but not ready, because its idle screen matched none of
// the indicators the readiness check knew without the pane's type.
func TestRelaunchRestoredPanes_ReadsReadinessAsThePaneType(t *testing.T) {
	ops := restoredPaneOps{
		respawn: func(string, string) error { return nil },
		observe: func(string) (restoredPaneState, error) {
			return restoredPaneState{command: "node", output: restoreTestPiScreen}, nil
		},
	}
	launch := &restoredLaunch{pane: PaneState{AgentType: "pi"}, paneID: "%3", command: "pi"}

	relaunchRestoredPanes([]*restoredLaunch{launch}, time.Now().Add(time.Second), time.Second, time.Millisecond, ops)

	if launch.err != nil || launch.attempts != 1 {
		t.Fatalf("relaunch err = %v after %d launches, want the Pi pane up after one", launch.err, launch.attempts)
	}
}

func TestRestorer_loadPaneScrollback_Compressed(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "ntm-restore-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	storage := NewStorageWithDir(tmpDir)
	r := NewRestorerWithStorage(storage)

	const (
		sessionName  = "test-session"
		checkpointID = "test-checkpoint"
		paneID       = "%0"
		content      = "compressed scrollback\nline 2\n"
	)

	compressed, err := gzipCompress([]byte(content))
	if err != nil {
		t.Fatalf("gzipCompress failed: %v", err)
	}
	if _, err := storage.SaveCompressedScrollback(sessionName, checkpointID, paneID, compressed); err != nil {
		t.Fatalf("SaveCompressedScrollback failed: %v", err)
	}

	got, err := r.loadPaneScrollback(sessionName, checkpointID, paneID)
	if err != nil {
		t.Fatalf("loadPaneScrollback failed: %v", err)
	}
	if got != content {
		t.Fatalf("loadPaneScrollback = %q, want %q", got, content)
	}
}

func TestRestorer_loadPaneScrollbackForPane_UsesRecordedScrollbackFile(t *testing.T) {
	tmpDir := t.TempDir()
	storage := NewStorageWithDir(tmpDir)
	r := NewRestorerWithStorage(storage)

	const (
		sessionName  = "test-session"
		checkpointID = "test-checkpoint"
		content      = "recorded scrollback\nline 2\n"
	)

	cpDir := storage.CheckpointDir(sessionName, checkpointID)
	if err := os.MkdirAll(filepath.Join(cpDir, PanesDir), 0755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	compressed, err := gzipCompress([]byte(content))
	if err != nil {
		t.Fatalf("gzipCompress failed: %v", err)
	}
	scrollbackRelPath := filepath.Join(PanesDir, "pane_from_file.txt.gz")
	scrollbackAbsPath := filepath.Join(cpDir, scrollbackRelPath)
	if err := os.WriteFile(scrollbackAbsPath, compressed, 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	got, err := r.loadPaneScrollbackForPane(sessionName, checkpointID, PaneState{
		ID:             "%stale",
		ScrollbackFile: scrollbackRelPath,
	})
	if err != nil {
		t.Fatalf("loadPaneScrollbackForPane failed: %v", err)
	}
	if got != content {
		t.Fatalf("loadPaneScrollbackForPane = %q, want %q", got, content)
	}
}

func TestRestorer_loadPaneScrollbackForPane_FallsBackToPaneID(t *testing.T) {
	tmpDir := t.TempDir()
	storage := NewStorageWithDir(tmpDir)
	r := NewRestorerWithStorage(storage)

	const (
		sessionName  = "test-session"
		checkpointID = "test-checkpoint"
		paneID       = "%0"
		content      = "fallback scrollback\nline 2\n"
	)

	compressed, err := gzipCompress([]byte(content))
	if err != nil {
		t.Fatalf("gzipCompress failed: %v", err)
	}
	if _, err := storage.SaveCompressedScrollback(sessionName, checkpointID, paneID, compressed); err != nil {
		t.Fatalf("SaveCompressedScrollback failed: %v", err)
	}

	got, err := r.loadPaneScrollbackForPane(sessionName, checkpointID, PaneState{ID: paneID})
	if err != nil {
		t.Fatalf("loadPaneScrollbackForPane failed: %v", err)
	}
	if got != content {
		t.Fatalf("loadPaneScrollbackForPane = %q, want %q", got, content)
	}
}

func TestTruncateToLines(t *testing.T) {
	tests := []struct {
		content  string
		maxLines int
		want     string
	}{
		{"", 5, ""},
		{"one", 5, "one"},
		{"one\ntwo\nthree", 5, "one\ntwo\nthree"},
		{"one\ntwo\nthree", 2, "two\nthree"},
		{"one\ntwo\nthree\nfour\nfive", 3, "three\nfour\nfive"},
		{"one\ntwo\nthree", 1, "three"},
	}

	for _, tt := range tests {
		got := truncateToLines(tt.content, tt.maxLines)
		if got != tt.want {
			t.Errorf("truncateToLines(%q, %d) = %q, want %q",
				tt.content, tt.maxLines, got, tt.want)
		}
	}
}

func TestValidateCheckpoint_RejectsScrollbackTraversal(t *testing.T) {
	storage := NewStorageWithDir(t.TempDir())
	r := NewRestorerWithStorage(storage)

	cp := &Checkpoint{
		ID:          "test-checkpoint",
		SessionName: "test-session",
		WorkingDir:  t.TempDir(),
		Session: SessionState{
			Panes: []PaneState{
				{
					Index:          0,
					ID:             "%0",
					ScrollbackFile: "../../etc/passwd",
				},
			},
		},
	}

	issues := r.ValidateCheckpoint(cp, RestoreOptions{InjectContext: true})
	if len(issues) != 1 {
		t.Fatalf("expected 1 issue, got %d: %v", len(issues), issues)
	}
	if !containsSubstr(issues[0], "invalid scrollback path") {
		t.Fatalf("expected invalid scrollback path issue, got %v", issues)
	}
}

func TestSplitLines(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"", 0},
		{"one", 1},
		{"one\n", 1}, // trailing newline doesn't add extra line
		{"one\ntwo", 2},
		{"one\ntwo\n", 2}, // trailing newline doesn't add extra line
		{"one\ntwo\nthree", 3},
	}

	for _, tt := range tests {
		got := splitLines(tt.input)
		if len(got) != tt.want {
			t.Errorf("len(splitLines(%q)) = %d, want %d", tt.input, len(got), tt.want)
		}
	}
}

func TestJoinLines(t *testing.T) {
	tests := []struct {
		lines []string
		want  string
	}{
		{nil, ""},
		{[]string{}, ""},
		{[]string{"one"}, "one"},
		{[]string{"one", "two"}, "one\ntwo"},
		{[]string{"one", "two", "three"}, "one\ntwo\nthree"},
	}

	for _, tt := range tests {
		got := joinLines(tt.lines)
		if got != tt.want {
			t.Errorf("joinLines(%v) = %q, want %q", tt.lines, got, tt.want)
		}
	}
}

func TestTrimSpace(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", ""},
		{"hello", "hello"},
		{"  hello", "hello"},
		{"hello  ", "hello"},
		{"  hello  ", "hello"},
		{"\n\thello\n\t", "hello"},
		{"  \n\t  ", ""},
	}

	for _, tt := range tests {
		got := trimSpace(tt.input)
		if got != tt.want {
			t.Errorf("trimSpace(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "just now"},
		{5 * time.Minute, "5m"},
		{90 * time.Minute, "1h"},
		{3 * time.Hour, "3h"},
		{36 * time.Hour, "1d"},
		{72 * time.Hour, "3d"},
	}

	for _, tt := range tests {
		got := formatDuration(tt.d)
		if got != tt.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestFormatContextInjection(t *testing.T) {
	content := "Hello\nWorld"
	checkpointTime := time.Now().Add(-2 * time.Hour)

	result := formatContextInjection(content, checkpointTime)

	if !containsSubstr(result, "Context from checkpoint") {
		t.Error("Expected header with 'Context from checkpoint'")
	}
	if !containsSubstr(result, "Hello") {
		t.Error("Expected content to be included")
	}
	if !containsSubstr(result, "World") {
		t.Error("Expected content to be included")
	}
}

func TestRestorer_Restore_CheckpointNotFound(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "ntm-restore-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	r := NewRestorerWithStorage(NewStorageWithDir(tmpDir))

	_, err = r.Restore("nonexistent-session", "nonexistent-checkpoint", RestoreOptions{})
	if err == nil {
		t.Error("Restore should fail for nonexistent checkpoint")
	}
}

func TestRestoreResult_AssignmentsAndBVSummary(t *testing.T) {
	// Test RestoreResult with assignments and BV summary (bd-32ck)
	now := time.Now()
	result := &RestoreResult{
		SessionName:   "test-session",
		PanesRestored: 2,
		DryRun:        true,
		Assignments: []AssignmentSnapshot{
			{
				BeadID:     "bd-1234",
				BeadTitle:  "Fix the widget",
				Pane:       1,
				AgentType:  "claude",
				AgentName:  "BlueLake",
				Status:     "working",
				AssignedAt: now,
			},
			{
				BeadID:     "bd-5678",
				BeadTitle:  "Add tests",
				Pane:       2,
				AgentType:  "codex",
				Status:     "assigned",
				AssignedAt: now,
			},
		},
		BVSummary: &BVSnapshot{
			OpenCount:       15,
			ActionableCount: 8,
			BlockedCount:    5,
			InProgressCount: 2,
			TopPicks:        []string{"bd-1234", "bd-5678"},
			CapturedAt:      now,
		},
	}

	if len(result.Assignments) != 2 {
		t.Fatalf("len(Assignments) = %d, want 2", len(result.Assignments))
	}
	if result.Assignments[0].BeadID != "bd-1234" {
		t.Errorf("Assignments[0].BeadID = %q, want bd-1234", result.Assignments[0].BeadID)
	}
	if result.Assignments[0].AgentType != "claude" {
		t.Errorf("Assignments[0].AgentType = %q, want claude", result.Assignments[0].AgentType)
	}
	if result.Assignments[1].Status != "assigned" {
		t.Errorf("Assignments[1].Status = %q, want assigned", result.Assignments[1].Status)
	}
	if result.BVSummary == nil {
		t.Fatal("BVSummary should not be nil")
	}
	if result.BVSummary.ActionableCount != 8 {
		t.Errorf("BVSummary.ActionableCount = %d, want 8", result.BVSummary.ActionableCount)
	}
	if result.BVSummary.BlockedCount != 5 {
		t.Errorf("BVSummary.BlockedCount = %d, want 5", result.BVSummary.BlockedCount)
	}
	if len(result.BVSummary.TopPicks) != 2 {
		t.Errorf("len(BVSummary.TopPicks) = %d, want 2", len(result.BVSummary.TopPicks))
	}
}

func TestRestoreResult_NoAssignmentsOrBVSummary(t *testing.T) {
	// Test backward compat: RestoreResult without assignments (bd-32ck)
	result := &RestoreResult{
		SessionName:   "test-session",
		PanesRestored: 1,
	}

	if len(result.Assignments) != 0 {
		t.Errorf("len(Assignments) = %d, want 0", len(result.Assignments))
	}
	if result.BVSummary != nil {
		t.Error("BVSummary should be nil when not set")
	}
}

func TestRestorer_RestoreFromCheckpoint_DryRun_SurfacesAssignments(t *testing.T) {
	// Test that DryRun restore surfaces assignment and BV data (bd-32ck)
	tmpDir, err := os.MkdirTemp("", "ntm-restore-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	r := NewRestorerWithStorage(NewStorageWithDir(tmpDir))

	now := time.Now()
	cp := &Checkpoint{
		ID:          "test-checkpoint",
		SessionName: "test-session",
		WorkingDir:  tmpDir,
		Session: SessionState{
			Panes: []PaneState{
				{Index: 0, ID: "%0", Title: "user"},
				{Index: 1, ID: "%1", Title: "cc_1"},
			},
		},
		Assignments: []AssignmentSnapshot{
			{
				BeadID:     "bd-42",
				BeadTitle:  "Implement feature X",
				Pane:       1,
				AgentType:  "claude",
				AgentName:  "GreenCastle",
				Status:     "working",
				AssignedAt: now,
			},
		},
		BVSummary: &BVSnapshot{
			OpenCount:       20,
			ActionableCount: 12,
			BlockedCount:    6,
			InProgressCount: 2,
			TopPicks:        []string{"bd-42", "bd-99"},
			CapturedAt:      now,
		},
	}

	result, err := r.RestoreFromCheckpoint(cp, RestoreOptions{DryRun: true})
	if err != nil {
		t.Fatalf("RestoreFromCheckpoint(DryRun) failed: %v", err)
	}

	// Verify assignments are surfaced in result
	if len(result.Assignments) != 1 {
		t.Fatalf("len(result.Assignments) = %d, want 1", len(result.Assignments))
	}
	if result.Assignments[0].BeadID != "bd-42" {
		t.Errorf("Assignments[0].BeadID = %q, want bd-42", result.Assignments[0].BeadID)
	}
	if result.Assignments[0].AgentName != "GreenCastle" {
		t.Errorf("Assignments[0].AgentName = %q, want GreenCastle", result.Assignments[0].AgentName)
	}

	// Verify BV summary is surfaced
	if result.BVSummary == nil {
		t.Fatal("result.BVSummary should not be nil")
	}
	if result.BVSummary.ActionableCount != 12 {
		t.Errorf("BVSummary.ActionableCount = %d, want 12", result.BVSummary.ActionableCount)
	}
	if result.BVSummary.InProgressCount != 2 {
		t.Errorf("BVSummary.InProgressCount = %d, want 2", result.BVSummary.InProgressCount)
	}
}

func TestRestorer_RestoreFromCheckpoint_DryRun_NoAssignments(t *testing.T) {
	// Test that restore without assignments still works (backward compat, bd-32ck)
	tmpDir, err := os.MkdirTemp("", "ntm-restore-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	r := NewRestorerWithStorage(NewStorageWithDir(tmpDir))

	cp := &Checkpoint{
		ID:          "test-checkpoint",
		SessionName: "test-session",
		WorkingDir:  tmpDir,
		Session: SessionState{
			Panes: []PaneState{{Index: 0, ID: "%0"}},
		},
		// No Assignments or BVSummary
	}

	result, err := r.RestoreFromCheckpoint(cp, RestoreOptions{DryRun: true})
	if err != nil {
		t.Fatalf("RestoreFromCheckpoint(DryRun) failed: %v", err)
	}

	if len(result.Assignments) != 0 {
		t.Errorf("len(result.Assignments) = %d, want 0", len(result.Assignments))
	}
	if result.BVSummary != nil {
		t.Error("result.BVSummary should be nil for checkpoint without BV data")
	}
}

func TestRestorer_RestoreLatest_NoCheckpoints(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "ntm-restore-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	r := NewRestorerWithStorage(NewStorageWithDir(tmpDir))

	_, err = r.RestoreLatest("nonexistent-session", RestoreOptions{})
	if err == nil {
		t.Error("RestoreLatest should fail when no checkpoints exist")
	}
}

func TestRestorer_checkGitState_BranchMismatch(t *testing.T) {
	repoDir, branch, commit := initGitRepo(t)

	r := NewRestorer()
	cp := &Checkpoint{
		Git: GitState{
			Branch: branch + "-other",
			Commit: commit,
		},
	}

	warning := r.checkGitState(cp, repoDir)
	if !strings.Contains(warning, "git branch mismatch") {
		t.Fatalf("expected branch mismatch warning, got %q", warning)
	}
}

func TestRestorer_checkGitState_CommitMismatch(t *testing.T) {
	repoDir, branch, commit := initGitRepo(t)

	// Create a new commit to move HEAD forward.
	updateFile := filepath.Join(repoDir, "README.md")
	if err := os.WriteFile(updateFile, []byte("updated"), 0644); err != nil {
		t.Fatalf("failed to update file: %v", err)
	}
	runGitCmd(t, repoDir, "add", ".")
	runGitCmd(t, repoDir, "commit", "-m", "Second commit")

	r := NewRestorer()
	cp := &Checkpoint{
		Git: GitState{
			Branch: branch,
			Commit: commit,
		},
	}

	warning := r.checkGitState(cp, repoDir)
	if !strings.Contains(warning, "git commit mismatch") {
		t.Fatalf("expected commit mismatch warning, got %q", warning)
	}
}

func TestFromTmuxPane_PreservesWindowIndex(t *testing.T) {
	state := FromTmuxPane(tmux.Pane{
		ID:          "%9",
		Index:       2,
		WindowIndex: 3,
		Title:       "demo__cc_1",
		Type:        tmux.AgentClaude,
		Command:     "claude",
		Width:       120,
		Height:      40,
	})

	if state.WindowIndex != 3 {
		t.Fatalf("WindowIndex = %d, want 3", state.WindowIndex)
	}
}

func TestRestorableAgentCommand(t *testing.T) {
	tests := []struct {
		name string
		pane PaneState
		want string
	}{
		{
			name: "custom non-shell command is preserved",
			pane: PaneState{AgentType: "cc", Command: "/usr/local/bin/custom-claude-wrapper --fast"},
			want: "/usr/local/bin/custom-claude-wrapper --fast",
		},
		{
			name: "shell command falls back to default agent binary",
			pane: PaneState{AgentType: "codex", Command: "zsh"},
			want: "codex",
		},
		{
			name: "missing command falls back to default agent binary",
			pane: PaneState{AgentType: "google-gemini"},
			want: "gemini",
		},
		{
			name: "user panes are skipped",
			pane: PaneState{AgentType: "user", Command: "bash"},
			want: "",
		},
		// A bare `muse` stops at the workspace trust prompt (--yolo does not
		// record trust), so every Muse fallback relaunches with --yolo.
		{
			name: "muse pane recorded at a shell relaunches muse --yolo",
			pane: PaneState{AgentType: "muse", Command: "zsh"},
			want: "muse --yolo",
		},
		{
			name: "muse alias without a command relaunches muse --yolo",
			pane: PaneState{AgentType: "muse-code"},
			want: "muse --yolo",
		},
		{
			name: "muse pane recorded as the launcher relaunches with --yolo",
			pane: PaneState{AgentType: "muse", Command: "muse"},
			want: "muse --yolo",
		},
		{
			name: "muse pane recorded as the truncated versioned binary relaunches the launcher",
			pane: PaneState{AgentType: "muse", Command: "muse-bin-1.4.11"},
			want: "muse --yolo",
		},
		{
			name: "muse pane recorded as the full versioned binary path relaunches the launcher",
			pane: PaneState{AgentType: "muse", Command: "/Users/me/.local/bin/muse-bin-1.4.11-R6667.1 --yolo"},
			want: "muse --yolo",
		},
		{
			name: "muse pane with a custom launch command keeps it",
			pane: PaneState{AgentType: "muse", Command: "muse --yolo --model muse-spark-1.3"},
			want: "muse --yolo --model muse-spark-1.3",
		},
		// bd-7po31: Pi and Kimi had no fallback, so a pane recorded at a
		// shell was skipped. Pi is a Node script, so tmux on macOS records
		// `node` for a running Pi pane; relaunching that opened a Node REPL.
		{
			name: "pi pane recorded at a shell relaunches pi",
			pane: PaneState{AgentType: "pi", Command: "zsh"},
			want: "pi",
		},
		{
			name: "pi alias without a command relaunches pi",
			pane: PaneState{AgentType: "pi-coding-agent"},
			want: "pi",
		},
		{
			name: "pi pane recorded as its node runtime relaunches pi",
			pane: PaneState{AgentType: "pi", Command: "node"},
			want: "pi",
		},
		{
			name: "pi pane with a custom launch command keeps it",
			pane: PaneState{AgentType: "pi", Command: "pi --model anthropic/claude-sonnet-4-6"},
			want: "pi --model anthropic/claude-sonnet-4-6",
		},
		{
			name: "kimi pane recorded at a shell relaunches kimi",
			pane: PaneState{AgentType: "kimi", Command: "bash"},
			want: "kimi",
		},
		{
			name: "kimi alias without a command relaunches kimi",
			pane: PaneState{AgentType: "kimi-code"},
			want: "kimi",
		},
		{
			name: "kimi pane recorded as its node runtime relaunches kimi",
			pane: PaneState{AgentType: "kimi", Command: "node"},
			want: "kimi",
		},
		{
			name: "legacy kimi pane recorded as its python runtime relaunches kimi",
			pane: PaneState{AgentType: "kimi", Command: "python3.13"},
			want: "kimi",
		},
		{
			name: "claude pane recorded as its node runtime relaunches claude",
			pane: PaneState{AgentType: "cc", Command: "node"},
			want: "claude",
		},
		{
			name: "runtime with a script argument is a real launch command",
			pane: PaneState{AgentType: "pi", Command: "node /opt/pi/cli.js --offline"},
			want: "node /opt/pi/cli.js --offline",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := restorableAgentCommand(tt.pane); got != tt.want {
				t.Fatalf("restorableAgentCommand(%+v) = %q, want %q", tt.pane, got, tt.want)
			}
		})
	}
}

func TestSortedCheckpointPanes(t *testing.T) {
	panes := []PaneState{
		{WindowIndex: 2, Index: 0, Title: "w2p0"},
		{WindowIndex: 0, Index: 1, Title: "w0p1"},
		{WindowIndex: 0, Index: 0, Title: "w0p0"},
		{WindowIndex: 1, Index: 0, Title: "w1p0"},
	}

	sorted := sortedCheckpointPanes(panes)
	gotTitles := []string{sorted[0].Title, sorted[1].Title, sorted[2].Title, sorted[3].Title}
	wantTitles := []string{"w0p0", "w0p1", "w1p0", "w2p0"}
	for i := range wantTitles {
		if gotTitles[i] != wantTitles[i] {
			t.Fatalf("sorted title at %d = %q, want %q", i, gotTitles[i], wantTitles[i])
		}
	}
}

func TestRestoredPaneIndexForCheckpointIndex_UnsortedCheckpointOrder(t *testing.T) {
	panes := []PaneState{
		{ID: "%b", WindowIndex: 1, Index: 0, Title: "w1p0"},
		{ID: "%a", WindowIndex: 0, Index: 0, Title: "w0p0"},
		{ID: "%c", WindowIndex: 1, Index: 1, Title: "w1p1"},
	}

	tests := []struct {
		checkpointIndex int
		want            int
	}{
		{checkpointIndex: 0, want: 1},
		{checkpointIndex: 1, want: 0},
		{checkpointIndex: 2, want: 2},
	}

	for _, tt := range tests {
		if got := restoredPaneIndexForCheckpointIndex(panes, tt.checkpointIndex); got != tt.want {
			t.Fatalf("restoredPaneIndexForCheckpointIndex(%d) = %d, want %d", tt.checkpointIndex, got, tt.want)
		}
	}
}

func TestRestorer_RestoreFromCheckpoint_RelaunchesCommandsAcrossWindows(t *testing.T) {
	testutil.RequireTmuxThrottled(t)

	workDir := t.TempDir()
	r := NewRestorerWithStorage(NewStorageWithDir(t.TempDir()))
	// The sleep stand-ins never draw a ready screen, so the restore waits out
	// its ready deadline; a short one keeps the test fast.
	r.agentReadyTimeout = time.Second
	sessionName := "cprestore-" + time.Now().Format("150405000000")

	cp := &Checkpoint{
		ID:          "test-checkpoint",
		SessionName: sessionName,
		WorkingDir:  workDir,
		Session: SessionState{
			Panes: []PaneState{
				{
					Index:       0,
					WindowIndex: 1,
					Title:       sessionName + "__cursor_1",
					AgentType:   "cursor",
					Command:     "sleep 30",
					ID:          "%old-1",
				},
				{
					Index:       0,
					WindowIndex: 0,
					Title:       sessionName + "__cc_1",
					AgentType:   "cc",
					Command:     "sleep 30",
					ID:          "%old-0",
				},
				{
					Index:       1,
					WindowIndex: 1,
					Title:       sessionName + "__cursor_2",
					AgentType:   "cursor",
					Command:     "sleep 30",
					ID:          "%old-2",
				},
			},
			ActivePaneIndex: 0,
		},
	}

	t.Cleanup(func() {
		if tmux.SessionExists(sessionName) {
			_ = tmux.KillSession(sessionName)
		}
	})

	result, err := r.RestoreFromCheckpoint(cp, RestoreOptions{})
	if err != nil {
		t.Fatalf("RestoreFromCheckpoint failed: %v", err)
	}
	if result.PanesRestored != 3 {
		t.Fatalf("PanesRestored = %d, want 3 (warnings=%v)", result.PanesRestored, result.Warnings)
	}

	var sorted []tmux.Pane
	deadline := time.Now().Add(5 * time.Second)
	for {
		panes, err := tmux.GetPanes(sessionName)
		if err != nil {
			t.Fatalf("GetPanes failed: %v", err)
		}
		if len(panes) != 3 {
			t.Fatalf("len(panes) = %d, want 3", len(panes))
		}

		sorted = sortedTmuxPanes(panes)
		if sorted[0].Command == "sleep" && sorted[1].Command == "sleep" && sorted[2].Command == "sleep" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf(
				"timed out waiting for restored commands; commands=[%q %q %q] active=[%v %v %v]",
				sorted[0].Command,
				sorted[1].Command,
				sorted[2].Command,
				sorted[0].Active,
				sorted[1].Active,
				sorted[2].Active,
			)
		}
		time.Sleep(100 * time.Millisecond)
	}

	if sorted[0].WindowIndex == sorted[1].WindowIndex {
		t.Fatalf("window indexes = [%d %d %d], want first pane in its own window", sorted[0].WindowIndex, sorted[1].WindowIndex, sorted[2].WindowIndex)
	}
	if sorted[1].WindowIndex != sorted[2].WindowIndex {
		t.Fatalf("window indexes = [%d %d %d], want panes 2 and 3 in the same window", sorted[0].WindowIndex, sorted[1].WindowIndex, sorted[2].WindowIndex)
	}
	if sorted[1].WindowIndex <= sorted[0].WindowIndex {
		t.Fatalf("window indexes = [%d %d %d], want ascending order", sorted[0].WindowIndex, sorted[1].WindowIndex, sorted[2].WindowIndex)
	}
	if sorted[0].Title != sessionName+"__cc_1" {
		t.Fatalf("first pane title = %q, want %q", sorted[0].Title, sessionName+"__cc_1")
	}
	if sorted[1].Title != sessionName+"__cursor_1" {
		t.Fatalf("second pane title = %q, want %q", sorted[1].Title, sessionName+"__cursor_1")
	}
	if sorted[2].Title != sessionName+"__cursor_2" {
		t.Fatalf("third pane title = %q, want %q", sorted[2].Title, sessionName+"__cursor_2")
	}

	activePaneID, err := tmux.DefaultClient.Run("display-message", "-p", "-t", sessionName, "#{pane_id}")
	if err != nil {
		t.Fatalf("display-message active pane failed: %v", err)
	}
	if activePaneID != sorted[1].ID {
		t.Fatalf("active pane id = %q, want %q", activePaneID, sorted[1].ID)
	}
}

func TestRestorer_RestoreFromCheckpoint_PreservesSparseWindowIndexes(t *testing.T) {
	testutil.RequireTmuxThrottled(t)

	workDir := t.TempDir()
	r := NewRestorerWithStorage(NewStorageWithDir(t.TempDir()))
	// The sleep stand-ins never draw a ready screen, so the restore waits out
	// its ready deadline; a short one keeps the test fast.
	r.agentReadyTimeout = time.Second
	sessionName := "cprestore-sparse-" + time.Now().Format("150405000000")

	cp := &Checkpoint{
		ID:          "test-checkpoint-sparse",
		SessionName: sessionName,
		WorkingDir:  workDir,
		Session: SessionState{
			Panes: []PaneState{
				{
					Index:       0,
					WindowIndex: 5,
					Title:       sessionName + "__cod_1",
					AgentType:   "cod",
					Command:     "sleep 30",
					ID:          "%old-5",
				},
				{
					Index:       0,
					WindowIndex: 2,
					Title:       sessionName + "__cc_1",
					AgentType:   "cc",
					Command:     "sleep 30",
					ID:          "%old-2",
				},
				{
					Index:       1,
					WindowIndex: 5,
					Title:       sessionName + "__cod_2",
					AgentType:   "cod",
					Command:     "sleep 30",
					ID:          "%old-6",
				},
			},
			ActivePaneIndex: 0,
		},
	}

	t.Cleanup(func() {
		if tmux.SessionExists(sessionName) {
			_ = tmux.KillSession(sessionName)
		}
	})

	result, err := r.RestoreFromCheckpoint(cp, RestoreOptions{})
	if err != nil {
		t.Fatalf("RestoreFromCheckpoint failed: %v", err)
	}
	if result.PanesRestored != 3 {
		t.Fatalf("PanesRestored = %d, want 3 (warnings=%v)", result.PanesRestored, result.Warnings)
	}

	var sorted []tmux.Pane
	deadline := time.Now().Add(5 * time.Second)
	for {
		panes, err := tmux.GetPanes(sessionName)
		if err != nil {
			t.Fatalf("GetPanes failed: %v", err)
		}
		if len(panes) != 3 {
			t.Fatalf("len(panes) = %d, want 3", len(panes))
		}

		sorted = sortedTmuxPanes(panes)
		if sorted[0].Command == "sleep" && sorted[1].Command == "sleep" && sorted[2].Command == "sleep" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf(
				"timed out waiting for restored commands; commands=[%q %q %q] window_indexes=[%d %d %d]",
				sorted[0].Command,
				sorted[1].Command,
				sorted[2].Command,
				sorted[0].WindowIndex,
				sorted[1].WindowIndex,
				sorted[2].WindowIndex,
			)
		}
		time.Sleep(100 * time.Millisecond)
	}

	gotWindowIndexes := []int{sorted[0].WindowIndex, sorted[1].WindowIndex, sorted[2].WindowIndex}
	wantWindowIndexes := []int{2, 5, 5}
	for i := range wantWindowIndexes {
		if gotWindowIndexes[i] != wantWindowIndexes[i] {
			t.Fatalf("window indexes = %v, want %v", gotWindowIndexes, wantWindowIndexes)
		}
	}

	activePaneID, err := tmux.DefaultClient.Run("display-message", "-p", "-t", sessionName, "#{pane_id}")
	if err != nil {
		t.Fatalf("display-message active pane failed: %v", err)
	}
	if activePaneID != sorted[1].ID {
		t.Fatalf("active pane id = %q, want %q", activePaneID, sorted[1].ID)
	}
}

func TestRestorer_RestoreFromCheckpoint_PreservesPerWindowLayouts(t *testing.T) {
	testutil.RequireTmuxThrottled(t)

	workDir := t.TempDir()
	templateSession := "cplayout-src-" + time.Now().Format("150405000000")
	restoreSession := "cplayout-dst-" + time.Now().Format("150405000000")

	if err := tmux.CreateSession(templateSession, workDir); err != nil {
		t.Fatalf("CreateSession(template) failed: %v", err)
	}
	t.Cleanup(func() {
		if tmux.SessionExists(templateSession) {
			_ = tmux.KillSession(templateSession)
		}
		if tmux.SessionExists(restoreSession) {
			_ = tmux.KillSession(restoreSession)
		}
	})

	firstWindow, err := tmux.GetFirstWindow(templateSession)
	if err != nil {
		t.Fatalf("GetFirstWindow(template) failed: %v", err)
	}
	firstTarget := fmt.Sprintf("%s:%d", templateSession, firstWindow)
	if _, err := tmux.DefaultClient.Run("split-window", "-t", firstTarget, "-c", workDir, "-P", "-F", "#{pane_id}"); err != nil {
		t.Fatalf("split-window first template window failed: %v", err)
	}
	if err := tmux.DefaultClient.RunSilent("select-layout", "-t", firstTarget, "even-horizontal"); err != nil {
		t.Fatalf("select-layout first template window failed: %v", err)
	}

	secondWindowIndex, err := tmux.DefaultClient.Run("new-window", "-d", "-t", templateSession, "-c", workDir, "-P", "-F", "#{window_index}")
	if err != nil {
		t.Fatalf("new-window template failed: %v", err)
	}
	secondTarget := fmt.Sprintf("%s:%s", templateSession, strings.TrimSpace(secondWindowIndex))
	if _, err := tmux.DefaultClient.Run("split-window", "-t", secondTarget, "-c", workDir, "-P", "-F", "#{pane_id}"); err != nil {
		t.Fatalf("split-window second template window failed: %v", err)
	}
	if err := tmux.DefaultClient.RunSilent("select-layout", "-t", secondTarget, "main-vertical"); err != nil {
		t.Fatalf("select-layout second template window failed: %v", err)
	}

	capturer := NewCapturerWithStorage(NewStorageWithDir(t.TempDir()))
	sessionState, err := capturer.captureSessionState(templateSession)
	if err != nil {
		t.Fatalf("captureSessionState(template) failed: %v", err)
	}
	if len(sessionState.WindowLayouts) != 2 {
		t.Fatalf("len(sessionState.WindowLayouts) = %d, want 2", len(sessionState.WindowLayouts))
	}

	r := NewRestorerWithStorage(NewStorageWithDir(t.TempDir()))
	cp := &Checkpoint{
		ID:          "layout-checkpoint",
		SessionName: restoreSession,
		WorkingDir:  workDir,
		Session:     sessionState,
	}

	result, err := r.RestoreFromCheckpoint(cp, RestoreOptions{})
	if err != nil {
		t.Fatalf("RestoreFromCheckpoint failed: %v", err)
	}
	if result.PanesRestored != len(sessionState.Panes) {
		t.Fatalf("PanesRestored = %d, want %d (warnings=%v)", result.PanesRestored, len(sessionState.Panes), result.Warnings)
	}

	restoredLayouts, err := getSessionWindowLayouts(restoreSession)
	if err != nil {
		t.Fatalf("getSessionWindowLayouts(restore) failed: %v", err)
	}
	if !windowLayoutsEqual(normalizeWindowLayouts(restoredLayouts), normalizeWindowLayouts(sessionState.WindowLayouts)) {
		t.Fatalf("restored layouts = %#v, want %#v", restoredLayouts, sessionState.WindowLayouts)
	}
}

var paneLayoutIDPattern = regexp.MustCompile(`(\d+x\d+,\d+,\d+),\d+`)

func normalizeWindowLayouts(layouts []WindowLayoutState) []WindowLayoutState {
	normalized := cloneWindowLayouts(layouts)
	for i := range normalized {
		normalized[i].Layout = normalizeWindowLayoutString(normalized[i].Layout)
	}
	return normalized
}

func normalizeWindowLayoutString(layout string) string {
	if comma := strings.IndexByte(layout, ','); comma >= 0 {
		layout = layout[comma+1:]
	}
	return paneLayoutIDPattern.ReplaceAllString(layout, `${1},pane`)
}

// containsSubstr checks if s contains substr (case-insensitive).
func containsSubstr(s, substr string) bool {
	return filepath.Base(s) == substr || len(s) >= len(substr) && findSubstr(s, substr)
}

func findSubstr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if matchIgnoreCase(s[i:i+len(substr)], substr) {
			return true
		}
	}
	return false
}

func matchIgnoreCase(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

func initGitRepo(t *testing.T) (string, string, string) {
	t.Helper()

	repoDir, err := os.MkdirTemp("", "ntm-restore-git-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoDir) })

	runGitCmd(t, repoDir, "init")
	runGitCmd(t, repoDir, "config", "user.email", "test@example.com")
	runGitCmd(t, repoDir, "config", "user.name", "Test User")

	readme := filepath.Join(repoDir, "README.md")
	if err := os.WriteFile(readme, []byte("initial"), 0644); err != nil {
		t.Fatalf("failed to create file: %v", err)
	}
	runGitCmd(t, repoDir, "add", ".")
	runGitCmd(t, repoDir, "commit", "-m", "Initial commit")

	branch := runGitCmd(t, repoDir, "rev-parse", "--abbrev-ref", "HEAD")
	commit := runGitCmd(t, repoDir, "rev-parse", "HEAD")

	return repoDir, strings.TrimSpace(branch), strings.TrimSpace(commit)
}

func runGitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()

	allArgs := append([]string{"-C", dir}, args...)
	cmd := exec.Command("git", allArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v (output: %s)", args, err, string(out))
	}
	return string(out)
}
