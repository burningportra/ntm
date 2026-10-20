package workflow

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

func TestTriggerRegistryCreatesStandardTriggers(t *testing.T) {
	registry := NewTriggerRegistry()
	cases := []Trigger{
		{Type: TriggerFileCreated, Pattern: "*_test.go"},
		{Type: TriggerFileModified, Pattern: "*.go"},
		{Type: TriggerCommandSuccess, Command: "true"},
		{Type: TriggerCommandFailure, Command: "false"},
		{Type: TriggerAgentSays, Pattern: "done"},
		{Type: TriggerAllAgentsIdle, IdleMinutes: 1},
		{Type: TriggerManual},
		{Type: TriggerTimeElapsed, Minutes: 1},
	}
	for _, config := range cases {
		t.Run(string(config.Type), func(t *testing.T) {
			trigger, err := registry.Create(config)
			if err != nil {
				t.Fatalf("Create(%s): %v", config.Type, err)
			}
			if trigger.Type() != config.Type {
				t.Errorf("Type() = %q, want %q", trigger.Type(), config.Type)
			}
		})
	}
}

func TestTriggerRegistryRejectsMissingFactory(t *testing.T) {
	registry := NewTriggerRegistry()
	registry.Register(TriggerManual, nil)
	if _, err := registry.Create(Trigger{Type: TriggerManual}); err == nil {
		t.Fatal("Create() succeeded without a registered factory")
	}
}

func TestFileCreatedTrigger(t *testing.T) {
	dir := t.TempDir()
	trigger, err := NewTriggerRegistry().Create(Trigger{Type: TriggerFileCreated, Pattern: "*_test.go"})
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}
	t.Cleanup(func() {
		if err := trigger.Stop(); err != nil {
			t.Errorf("Stop(): %v", err)
		}
	})
	ctx := &TriggerContext{ProjectRoot: dir}
	if err := trigger.Start(ctx); err != nil {
		t.Fatalf("Start(): %v", err)
	}
	if fired, err := trigger.Check(ctx); err != nil || fired {
		t.Fatalf("Check before create = (%v, %v), want (false, nil)", fired, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "created_test.go"), []byte("package example\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(): %v", err)
	}
	waitForTrigger(t, trigger, ctx)
}

func TestFileModifiedTrigger(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "existing.go")
	if err := os.WriteFile(path, []byte("package example\n"), 0o644); err != nil {
		t.Fatalf("initial WriteFile(): %v", err)
	}
	trigger, err := NewTriggerRegistry().Create(Trigger{Type: TriggerFileModified, Pattern: "*.go"})
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}
	t.Cleanup(func() {
		if err := trigger.Stop(); err != nil {
			t.Errorf("Stop(): %v", err)
		}
	})
	ctx := &TriggerContext{ProjectRoot: dir}
	if err := trigger.Start(ctx); err != nil {
		t.Fatalf("Start(): %v", err)
	}
	if err := os.WriteFile(path, []byte("package example\n// updated\n"), 0o644); err != nil {
		t.Fatalf("updated WriteFile(): %v", err)
	}
	waitForTrigger(t, trigger, ctx)
}

func TestFileCreatedTriggerWatchesExistingNestedDirectories(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "internal", "workflow")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("create nested directory: %v", err)
	}
	trigger, err := NewTriggerRegistry().Create(Trigger{Type: TriggerFileCreated, Pattern: "internal/**/*.go"})
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}
	t.Cleanup(func() {
		if err := trigger.Stop(); err != nil {
			t.Errorf("Stop(): %v", err)
		}
	})
	ctx := &TriggerContext{ProjectRoot: dir}
	if err := trigger.Start(ctx); err != nil {
		t.Fatalf("Start(): %v", err)
	}
	if err := os.WriteFile(filepath.Join(nested, "trigger.go"), []byte("package workflow\n"), 0o644); err != nil {
		t.Fatalf("write nested file: %v", err)
	}
	waitForTrigger(t, trigger, ctx)
}

func TestFileModifiedTriggerWatchesNewNestedDirectories(t *testing.T) {
	dir := t.TempDir()
	trigger, err := NewTriggerRegistry().Create(Trigger{Type: TriggerFileModified, Pattern: "internal/**/*.go"})
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}
	t.Cleanup(func() {
		if err := trigger.Stop(); err != nil {
			t.Errorf("Stop(): %v", err)
		}
	})
	ctx := &TriggerContext{ProjectRoot: dir}
	if err := trigger.Start(ctx); err != nil {
		t.Fatalf("Start(): %v", err)
	}
	nested := filepath.Join(dir, "internal", "workflow")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("create nested directory: %v", err)
	}
	path := filepath.Join(nested, "trigger.go")
	if err := os.WriteFile(path, []byte("package workflow\n"), 0o644); err != nil {
		t.Fatalf("create nested file: %v", err)
	}
	// On kqueue platforms the brand-new file's own watch registers
	// asynchronously after its Create event, so a single immediate Write can
	// land before the watch exists and never produce an event (load-dependent
	// race). Keep modifying until the watcher observes one; the assertion —
	// modifications inside a newly created nested directory fire the trigger
	// — is unchanged.
	deadline := time.Now().Add(triggerWaitDeadline)
	for revision := 0; ; revision++ {
		content := fmt.Sprintf("package workflow\n// modified %d\n", revision)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("modify nested file: %v", err)
		}
		fired, err := trigger.Check(ctx)
		if err != nil {
			t.Fatalf("Check(): %v", err)
		}
		if fired {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("trigger did not fire before deadline")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The red-green pattern: a file_modified target is created and written once
// in quick succession. On kqueue platforms the write notification can be lost
// (the new file's own watch registers asynchronously after its Create event),
// so the trigger's stat fallback must still fire — with NO further writes.
func TestFileModifiedTriggerFiresOnCreateThenSingleWrite(t *testing.T) {
	dir := t.TempDir()
	trigger, err := NewTriggerRegistry().Create(Trigger{Type: TriggerFileModified, Pattern: "*.go"})
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}
	t.Cleanup(func() {
		if err := trigger.Stop(); err != nil {
			t.Errorf("Stop(): %v", err)
		}
	})
	ctx := &TriggerContext{ProjectRoot: dir}
	if err := trigger.Start(ctx); err != nil {
		t.Fatalf("Start(): %v", err)
	}
	// One create+write: os.WriteFile creates the file and writes its content
	// immediately, before fsnotify can have watched the new file itself.
	if err := os.WriteFile(filepath.Join(dir, "fresh.go"), []byte("package example\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(): %v", err)
	}
	waitForTrigger(t, trigger, ctx)
}

// A file created empty and written later must fire via the stat fallback even
// if the write notification never arrives.
func TestFileModifiedTriggerStatFallbackDetectsLaterWrite(t *testing.T) {
	dir := t.TempDir()
	raw, err := NewTriggerRegistry().Create(Trigger{Type: TriggerFileModified, Pattern: "*.go"})
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}
	trigger := raw.(*fileTrigger)
	t.Cleanup(func() {
		if err := trigger.Stop(); err != nil {
			t.Errorf("Stop(): %v", err)
		}
	})
	ctx := &TriggerContext{ProjectRoot: dir}
	if err := trigger.Start(ctx); err != nil {
		t.Fatalf("Start(): %v", err)
	}
	path := filepath.Join(dir, "empty.go")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("create empty file: %v", err)
	}
	// Wait until the Create event has been processed (pending or watch-based
	// firing are both acceptable observations of the event).
	deadline := time.Now().Add(triggerWaitDeadline)
	for {
		trigger.mu.Lock()
		_, tracked := trigger.pending[path]
		fired := trigger.fired
		trigger.mu.Unlock()
		if tracked || fired {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Create event was not observed before deadline")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if fired, err := trigger.Check(ctx); err != nil {
		t.Fatalf("Check(): %v", err)
	} else if fired {
		return // platform delivered a Write for the empty create; fine
	}
	if err := os.WriteFile(path, []byte("package example\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	waitForTrigger(t, trigger, ctx)
}

// A directory moved into the project already holds its files, so no watch
// can see them being created or written. The triggers have to find them when
// they start watching the new directory.
func TestFileTriggersReportFilesInMovedInDirectories(t *testing.T) {
	for _, kind := range []TriggerType{TriggerFileCreated, TriggerFileModified} {
		t.Run(string(kind), func(t *testing.T) {
			staging := t.TempDir()
			if err := os.MkdirAll(filepath.Join(staging, "internal", "workflow"), 0o755); err != nil {
				t.Fatalf("create staged directory: %v", err)
			}
			if err := os.WriteFile(filepath.Join(staging, "internal", "workflow", "trigger.go"), []byte("package workflow\n"), 0o644); err != nil {
				t.Fatalf("write staged file: %v", err)
			}
			dir := t.TempDir()
			trigger, err := NewTriggerRegistry().Create(Trigger{Type: kind, Pattern: "internal/**/*.go"})
			if err != nil {
				t.Fatalf("Create(): %v", err)
			}
			t.Cleanup(func() {
				if err := trigger.Stop(); err != nil {
					t.Errorf("Stop(): %v", err)
				}
			})
			ctx := &TriggerContext{ProjectRoot: dir}
			if err := trigger.Start(ctx); err != nil {
				t.Fatalf("Start(): %v", err)
			}
			if err := os.Rename(filepath.Join(staging, "internal"), filepath.Join(dir, "internal")); err != nil {
				t.Fatalf("move staged tree into project: %v", err)
			}
			waitForTrigger(t, trigger, ctx)
		})
	}
}

// Build tools create and delete scratch directories all the time. One that is
// removed while the walk runs has nothing left to watch, so the walk skips it
// and carries on.
func TestAddDirectoryTreeSkipsDirectoryRemovedDuringWalk(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a"), 0o755); err != nil {
		t.Fatalf("create a: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "a", "x.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatalf("write a/x.go: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "b", "c"), 0o755); err != nil {
		t.Fatalf("create b/c: %v", err)
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatalf("NewWatcher(): %v", err)
	}
	// fsnotify needs its channels read, as fileTrigger.run does. Closing an
	// unread kqueue watcher under load made TempDir cleanup fail with EBADF.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for range watcher.Events {
		}
	}()
	go func() {
		for range watcher.Errors {
		}
	}()
	t.Cleanup(func() {
		_ = watcher.Close()
		<-drained
	})
	var found []string
	// The walk lists root before visiting a/, so b/ is still on its list
	// when the callback for a/x.go removes it.
	err = addDirectoryTree(watcher, root, func(path string, _ os.FileInfo) {
		found = append(found, path)
		if err := os.RemoveAll(filepath.Join(root, "b")); err != nil {
			t.Errorf("remove b: %v", err)
		}
	})
	if err != nil {
		t.Fatalf("addDirectoryTree() = %v, want nil", err)
	}
	if want := []string{filepath.Join(root, "a", "x.go")}; !slices.Equal(found, want) {
		t.Fatalf("found = %v, want %v", found, want)
	}
	watched := watcher.WatchList()
	for _, dir := range []string{root, filepath.Join(root, "a")} {
		if !slices.Contains(watched, dir) {
			t.Errorf("WatchList() = %v, missing %q", watched, dir)
		}
	}
}

// A directory the watcher cannot open must not disable the trigger: Check
// reports the failure once, naming the directory, and the trigger keeps
// watching everything else.
func TestFileTriggerReportsWalkFailureOnceAndKeepsWatching(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can open a directory that has no permissions")
	}
	staging := t.TempDir()
	locked := filepath.Join(staging, "vendor", "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatalf("create locked directory: %v", err)
	}
	dir := t.TempDir()
	movedLocked := filepath.Join(dir, "vendor", "locked")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatalf("lock directory: %v", err)
	}
	t.Cleanup(func() {
		// TempDir cleanup cannot remove a directory it cannot read.
		_ = os.Chmod(movedLocked, 0o755)
		_ = os.Chmod(locked, 0o755)
	})
	trigger, err := NewTriggerRegistry().Create(Trigger{Type: TriggerFileCreated, Pattern: "*.go"})
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}
	t.Cleanup(func() {
		if err := trigger.Stop(); err != nil {
			t.Errorf("Stop(): %v", err)
		}
	})
	ctx := &TriggerContext{ProjectRoot: dir}
	if err := trigger.Start(ctx); err != nil {
		t.Fatalf("Start(): %v", err)
	}
	if err := os.Rename(filepath.Join(staging, "vendor"), filepath.Join(dir, "vendor")); err != nil {
		t.Fatalf("move vendor into project: %v", err)
	}
	err = waitForCheckError(t, trigger, ctx)
	if !strings.Contains(err.Error(), movedLocked) {
		t.Fatalf("Check() error = %v, want it to name %q", err, movedLocked)
	}
	if fired, err := trigger.Check(ctx); err != nil || fired {
		t.Fatalf("Check after the failure was reported = (%v, %v), want (false, nil)", fired, err)
	}
	// Not in vendor/: fsnotify's kqueue backend stops listing a changed
	// directory at its first unreadable entry, so on macOS a new file that
	// sorts after locked/ is never reported.
	if err := os.WriteFile(filepath.Join(dir, "new.go"), []byte("package example\n"), 0o644); err != nil {
		t.Fatalf("write new.go: %v", err)
	}
	waitForTrigger(t, trigger, ctx)
}

// kqueue reports a new file's Create but starts watching the file only after
// that, so content written straight after the create can arrive with no Write
// event at all. Delivering only the Create reproduces that deterministically.
func TestFileModifiedTriggerCountsWritesMissedAfterCreate(t *testing.T) {
	dir := t.TempDir()
	runtimeTrigger, err := NewTriggerRegistry().Create(Trigger{Type: TriggerFileModified, Pattern: "*.go"})
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}
	trigger, ok := runtimeTrigger.(*fileTrigger)
	if !ok {
		t.Fatalf("trigger type = %T, want *fileTrigger", runtimeTrigger)
	}
	trigger.root = dir
	path := filepath.Join(dir, "new.go")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("create empty file: %v", err)
	}
	trigger.observe(nil, fsnotify.Event{Name: path, Op: fsnotify.Create})
	if fired, err := trigger.Check(nil); err != nil || fired {
		t.Fatalf("Check after creating an empty file = (%v, %v), want (false, nil)", fired, err)
	}
	if err := os.WriteFile(path, []byte("package example\n"), 0o644); err != nil {
		t.Fatalf("write new file: %v", err)
	}
	if fired, err := trigger.Check(nil); err != nil || !fired {
		t.Fatalf("Check after an unreported write = (%v, %v), want (true, nil)", fired, err)
	}
}

func TestCommandTriggers(t *testing.T) {
	registry := NewTriggerRegistry()
	ctx := &TriggerContext{Context: context.Background(), ProjectRoot: t.TempDir()}
	success, err := registry.Create(Trigger{Type: TriggerCommandSuccess, Command: "true"})
	if err != nil {
		t.Fatalf("Create success: %v", err)
	}
	if fired, err := success.Check(ctx); err != nil || !fired {
		t.Fatalf("success trigger = (%v, %v), want (true, nil)", fired, err)
	}

	failure, err := registry.Create(Trigger{Type: TriggerCommandFailure, Command: "false"})
	if err != nil {
		t.Fatalf("Create failure: %v", err)
	}
	if fired, err := failure.Check(ctx); err != nil || !fired {
		t.Fatalf("failure trigger = (%v, %v), want (true, nil)", fired, err)
	}
	if fired, err := success.Check(&TriggerContext{ProjectRoot: ""}); err == nil || fired {
		t.Fatalf("missing project root = (%v, %v), want (false, error)", fired, err)
	}
}

func TestCommandFailureTriggerRejectsCommandLaunchErrors(t *testing.T) {
	trigger, err := NewTriggerRegistry().Create(Trigger{Type: TriggerCommandFailure, Command: "ntm-command-that-does-not-exist"})
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}
	fired, err := trigger.Check(&TriggerContext{Context: context.Background(), ProjectRoot: t.TempDir()})
	if err == nil || fired {
		t.Fatalf("Check() for unavailable executable = (%v, %v), want (false, error)", fired, err)
	}
}

func TestAgentSaysTriggerHonorsRole(t *testing.T) {
	trigger, err := NewTriggerRegistry().Create(Trigger{Type: TriggerAgentSays, Pattern: "(?i)approved", Role: "reviewer"})
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}
	ctx := &TriggerContext{Outputs: []AgentOutput{
		{Role: "author", Text: "approved"},
		{Role: "reviewer", Text: "needs work"},
	}}
	if fired, err := trigger.Check(ctx); err != nil || fired {
		t.Fatalf("Check non-matching role output = (%v, %v), want (false, nil)", fired, err)
	}
	ctx.Outputs[1].Text = "Approved after review"
	if fired, err := trigger.Check(ctx); err != nil || !fired {
		t.Fatalf("Check matching role output = (%v, %v), want (true, nil)", fired, err)
	}
}

func TestAllAgentsIdleTrigger(t *testing.T) {
	now := time.Date(2026, time.August, 5, 12, 0, 0, 0, time.UTC)
	trigger, err := NewTriggerRegistry().Create(Trigger{Type: TriggerAllAgentsIdle, Role: "reviewer", IdleMinutes: 5})
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}
	ctx := &TriggerContext{Now: func() time.Time { return now }, Activities: []AgentActivity{
		{Role: "reviewer", LastActivity: now.Add(-6 * time.Minute)},
		{Role: "reviewer", LastActivity: now.Add(-5 * time.Minute)},
		{Role: "author", LastActivity: now},
	}}
	if fired, err := trigger.Check(ctx); err != nil || !fired {
		t.Fatalf("all reviewers idle = (%v, %v), want (true, nil)", fired, err)
	}
	ctx.Activities[1].LastActivity = now.Add(-time.Minute)
	if fired, err := trigger.Check(ctx); err != nil || fired {
		t.Fatalf("active reviewer = (%v, %v), want (false, nil)", fired, err)
	}
}

func TestManualTriggerCoalescesAndConsumes(t *testing.T) {
	runtimeTrigger, err := NewTriggerRegistry().Create(Trigger{Type: TriggerManual, Label: "Approve"})
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}
	trigger, ok := runtimeTrigger.(*ManualTrigger)
	if !ok {
		t.Fatalf("manual trigger type = %T", runtimeTrigger)
	}
	if trigger.Label() != "Approve" {
		t.Errorf("Label() = %q, want Approve", trigger.Label())
	}
	if !trigger.Fire() || trigger.Fire() {
		t.Fatal("Fire() should enqueue one request and coalesce the next")
	}
	if fired, err := trigger.Check(nil); err != nil || !fired {
		t.Fatalf("first Check() = (%v, %v), want (true, nil)", fired, err)
	}
	if fired, err := trigger.Check(nil); err != nil || fired {
		t.Fatalf("second Check() = (%v, %v), want (false, nil)", fired, err)
	}
}

func TestTimeElapsedTrigger(t *testing.T) {
	now := time.Date(2026, time.August, 5, 12, 0, 0, 0, time.UTC)
	trigger, err := NewTriggerRegistry().Create(Trigger{Type: TriggerTimeElapsed, Minutes: 2})
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}
	ctx := &TriggerContext{Now: func() time.Time { return now }}
	if fired, err := trigger.Check(ctx); err == nil || fired {
		t.Fatalf("Check before Start() = (%v, %v), want (false, error)", fired, err)
	}
	if err := trigger.Start(ctx); err != nil {
		t.Fatalf("Start(): %v", err)
	}
	now = now.Add(2 * time.Minute)
	if fired, err := trigger.Check(ctx); err != nil || !fired {
		t.Fatalf("Check at duration = (%v, %v), want (true, nil)", fired, err)
	}
}

// triggerWaitDeadline bounds the fsnotify-backed waits below. Filesystem
// notification latency is load-dependent (multi-agent swarm hosts routinely
// run at load averages where 2s was not enough); polling returns on the
// first observation, so a generous bound only costs time on real failures.
const triggerWaitDeadline = 15 * time.Second

func waitForTrigger(t *testing.T, trigger RuntimeTrigger, ctx *TriggerContext) {
	t.Helper()
	deadline := time.Now().Add(triggerWaitDeadline)
	for time.Now().Before(deadline) {
		fired, err := trigger.Check(ctx)
		if err != nil {
			t.Fatalf("Check(): %v", err)
		}
		if fired {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("trigger did not fire before deadline")
}

func waitForCheckError(t *testing.T, trigger RuntimeTrigger, ctx *TriggerContext) error {
	t.Helper()
	deadline := time.Now().Add(triggerWaitDeadline)
	for time.Now().Before(deadline) {
		fired, err := trigger.Check(ctx)
		if err != nil {
			return err
		}
		if fired {
			t.Fatal("trigger fired while waiting for a Check error")
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Check did not report an error before deadline")
	return nil
}
