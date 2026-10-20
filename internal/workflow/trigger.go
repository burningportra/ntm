package workflow

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

const defaultCommandTriggerTimeout = 30 * time.Second

// RuntimeTrigger evaluates a workflow transition condition. Start is only
// needed by triggers that consume a continuous source, such as filesystem
// notifications; all implementations are safe to Stop more than once.
type RuntimeTrigger interface {
	Type() TriggerType
	Check(*TriggerContext) (bool, error)
	Start(*TriggerContext) error
	Stop() error
}

// AgentOutput is a captured agent response available to agent_says triggers.
// Role is the workflow role, rather than the underlying pane or agent ID.
type AgentOutput struct {
	Role string
	Text string
}

// AgentActivity is the last observed activity for an agent in a workflow role.
type AgentActivity struct {
	Role         string
	LastActivity time.Time
}

// TriggerContext supplies runtime observations without coupling workflow
// templates to tmux or a particular session coordinator.
type TriggerContext struct {
	Context     context.Context
	ProjectRoot string
	Session     string
	CurrentRole string
	Outputs     []AgentOutput
	Activities  []AgentActivity
	Now         func() time.Time

	// TransitionEvidence contains verified agent_says receipts for the current
	// stage visit, keyed by the template's complete Flow.Transitions indexes.
	// A nonnil map replaces raw-output matching for those transitions, allowing
	// durable receipts to survive scrollback rollover and runner restarts.
	// Nil preserves ordinary trigger evaluation against Outputs.
	TransitionEvidence map[int]bool
}

func (c *TriggerContext) now() time.Time {
	if c != nil && c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// TriggerFactory builds a runtime trigger from a validated template trigger.
type TriggerFactory func(Trigger) (RuntimeTrigger, error)

// TriggerRegistry maps template trigger types to runtime implementations.
type TriggerRegistry struct {
	mu        sync.RWMutex
	factories map[TriggerType]TriggerFactory
}

// NewTriggerRegistry returns the standard trigger registry.
func NewTriggerRegistry() *TriggerRegistry {
	r := &TriggerRegistry{factories: make(map[TriggerType]TriggerFactory)}
	r.Register(TriggerFileCreated, newFileCreatedTrigger)
	r.Register(TriggerFileModified, newFileModifiedTrigger)
	r.Register(TriggerCommandSuccess, newCommandSuccessTrigger)
	r.Register(TriggerCommandFailure, newCommandFailureTrigger)
	r.Register(TriggerAgentSays, newAgentSaysTrigger)
	r.Register(TriggerAllAgentsIdle, newAllAgentsIdleTrigger)
	r.Register(TriggerManual, newManualTrigger)
	r.Register(TriggerTimeElapsed, newTimeElapsedTrigger)
	return r
}

// Register installs a factory for a trigger type. Supplying a nil factory
// removes a factory, which is useful when tests or an embedding application
// intentionally disable a trigger type.
func (r *TriggerRegistry) Register(kind TriggerType, factory TriggerFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if factory == nil {
		delete(r.factories, kind)
		return
	}
	r.factories[kind] = factory
}

// Create validates config and constructs its runtime trigger.
func (r *TriggerRegistry) Create(config Trigger) (RuntimeTrigger, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	r.mu.RLock()
	factory := r.factories[config.Type]
	r.mu.RUnlock()
	if factory == nil {
		return nil, fmt.Errorf("no runtime trigger registered for %q", config.Type)
	}
	return factory(config)
}

// fileBaseline is the size/mtime of a just-created file, recorded so a write
// that the notification backend never reports can still be detected by stat.
type fileBaseline struct {
	size    int64
	modTime time.Time
}

type fileTrigger struct {
	kind    TriggerType
	pattern string
	op      fsnotify.Op

	mu    sync.Mutex
	root  string
	fired bool
	// err holds watch failures that Check has not returned yet. Check
	// returns each one once, because the watch keeps covering everything
	// else and a single failure must not disable the trigger for good.
	err     error
	watcher *fsnotify.Watcher
	done    chan struct{}
	wg      sync.WaitGroup
	// pending tracks pattern-matching files that appeared empty (by a Create
	// event or in the walk of a new directory) but whose first Write event
	// may never arrive: on kqueue platforms (macOS/BSD) fsnotify registers
	// the new file's own watch asynchronously after the Create event, so a
	// create-then-write-once sequence can lose the write. Check falls back to
	// stat-ing these files.
	pending map[string]fileBaseline
}

func newFileCreatedTrigger(config Trigger) (RuntimeTrigger, error) {
	return &fileTrigger{kind: TriggerFileCreated, pattern: config.Pattern, op: fsnotify.Create}, nil
}

func newFileModifiedTrigger(config Trigger) (RuntimeTrigger, error) {
	return &fileTrigger{kind: TriggerFileModified, pattern: config.Pattern, op: fsnotify.Write}, nil
}

func (t *fileTrigger) Type() TriggerType { return t.kind }

func (t *fileTrigger) Start(ctx *TriggerContext) error {
	if ctx == nil || strings.TrimSpace(ctx.ProjectRoot) == "" {
		return errors.New("file trigger requires a project root")
	}
	root, err := filepath.Abs(ctx.ProjectRoot)
	if err != nil {
		return fmt.Errorf("resolve project root: %w", err)
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create filesystem watcher: %w", err)
	}
	if err := addDirectoryTree(watcher, root, nil); err != nil {
		_ = watcher.Close()
		return fmt.Errorf("watch project tree: %w", err)
	}

	if err := t.Stop(); err != nil {
		_ = watcher.Close()
		return err
	}
	t.mu.Lock()
	t.root = root
	t.fired = false
	t.err = nil
	t.pending = make(map[string]fileBaseline)
	t.watcher = watcher
	t.done = make(chan struct{})
	done := t.done
	t.wg.Add(1)
	t.mu.Unlock()

	go t.run(watcher, done)
	return nil
}

func (t *fileTrigger) run(watcher *fsnotify.Watcher, done <-chan struct{}) {
	defer t.wg.Done()
	for {
		select {
		case <-done:
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			t.observe(watcher, event)
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			t.report(err)
		}
	}
}

// report keeps err for the next Check to return.
func (t *fileTrigger) report(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.err = errors.Join(t.err, err)
}

// observe records what one filesystem event means for the trigger.
func (t *fileTrigger) observe(watcher *fsnotify.Watcher, event fsnotify.Event) {
	if event.Op&fsnotify.Create != 0 {
		if info, err := os.Stat(event.Name); err == nil {
			switch {
			case info.IsDir():
				// Files created in a new directory before its watch exists
				// produce no events, so the walk reports the ones already
				// there. A directory that is already gone has nothing to watch.
				err := addDirectoryTree(watcher, event.Name, t.fileAppeared)
				if err != nil && !removed(event.Name) {
					t.report(fmt.Errorf("watch created directory %q: %w", event.Name, err))
				}
			case info.Mode().IsRegular():
				t.fileAppeared(event.Name, info)
			}
		}
	}
	if event.Op&t.op == 0 || !t.matches(event.Name) {
		return
	}
	t.mu.Lock()
	t.fired = true
	t.mu.Unlock()
}

// fileAppeared records a regular file that showed up after Start, whether
// fsnotify reported its Create or the walk of a new directory found it. This
// is the kqueue write-loss fallback: kqueue (macOS, BSD) watches each file
// separately and fsnotify opens a new file's watch only after reporting its
// Create, so a write landing straight after creation may produce no Write
// event at all. A matching file that already holds content counts as
// written, and an empty one waits in pending with its baseline so Check can
// detect the write by stat.
func (t *fileTrigger) fileAppeared(path string, info os.FileInfo) {
	if !t.matches(path) {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.op&fsnotify.Write == 0 || info.Size() > 0 {
		t.fired = true
		return
	}
	if t.pending == nil {
		t.pending = make(map[string]fileBaseline)
	}
	t.pending[path] = fileBaseline{size: info.Size(), modTime: info.ModTime()}
}

func (t *fileTrigger) matches(name string) bool {
	t.mu.Lock()
	root := t.root
	pattern := t.pattern
	t.mu.Unlock()
	rel, err := filepath.Rel(root, name)
	if err != nil || strings.HasPrefix(rel, "..") {
		return false
	}
	if matchPathPattern(pattern, rel) {
		return true
	}
	matched, _ := filepath.Match(pattern, filepath.Base(rel))
	return matched
}

// addDirectoryTree registers every existing directory beneath root and, when
// found is non-nil, passes it each regular file. fsnotify watches directories
// individually, so watching only the project root misses events produced in
// nested source trees. Each directory is watched before it is listed, so a
// file created during the walk is either listed or reported by the watch.
//
// A failure on root ends the walk. Below root, an entry removed while the
// walk runs is skipped, since there is nothing left to watch, and any other
// failure skips that entry and everything beneath it; the walk carries on
// and returns those failures together, each naming its path.
func addDirectoryTree(watcher *fsnotify.Watcher, root string, found func(string, os.FileInfo)) error {
	var failures []error
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			err = watcher.Add(path)
		} else if err == nil && found != nil && entry.Type().IsRegular() {
			var info os.FileInfo
			if info, err = entry.Info(); err == nil {
				found(path, info)
			}
		}
		switch {
		case err == nil:
			return nil
		case path == root:
			return fmt.Errorf("watch %q: %w", path, err)
		case !removed(path):
			failures = append(failures, fmt.Errorf("watch %q: %w", path, err))
		}
		if entry.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return err
	}
	return errors.Join(failures...)
}

// removed reports whether path no longer exists, in which case failing to
// watch it is no failure. The error alone cannot tell: when a watched
// directory is deleted, kqueue can fail a new watch of it with EBADF because
// fsnotify closes its old descriptor at the same time.
func removed(path string) bool {
	_, err := os.Lstat(path)
	return errors.Is(err, fs.ErrNotExist)
}

// matchPathPattern matches slash-separated relative paths and gives ** its
// conventional recursive meaning. filepath.Match deliberately does not let
// * cross a path separator, so it cannot express workflow patterns such as
// internal/**/*.go by itself.
func matchPathPattern(pattern, rel string) bool {
	patternParts := strings.Split(filepath.ToSlash(pattern), "/")
	pathParts := strings.Split(filepath.ToSlash(rel), "/")
	var match func(int, int) bool
	match = func(patternIndex, pathIndex int) bool {
		if patternIndex == len(patternParts) {
			return pathIndex == len(pathParts)
		}
		if patternParts[patternIndex] == "**" {
			return match(patternIndex+1, pathIndex) ||
				(pathIndex < len(pathParts) && match(patternIndex, pathIndex+1))
		}
		if pathIndex >= len(pathParts) {
			return false
		}
		matched, err := filepath.Match(patternParts[patternIndex], pathParts[pathIndex])
		return err == nil && matched && match(patternIndex+1, pathIndex+1)
	}
	return match(0, 0)
}

func (t *fileTrigger) Check(_ *TriggerContext) (bool, error) {
	t.mu.Lock()
	if err := t.err; err != nil {
		// Each failure is returned once (see the err field).
		t.err = nil
		t.mu.Unlock()
		return false, fmt.Errorf("watch filesystem: %w", err)
	}
	if t.fired {
		t.mu.Unlock()
		return true, nil
	}
	// Stat fallback for writes whose notification was lost (see
	// fileAppeared): compare each pending created file against its
	// creation-time baseline.
	pending := make(map[string]fileBaseline, len(t.pending))
	for name, baseline := range t.pending {
		pending[name] = baseline
	}
	t.mu.Unlock()

	fired := false
	var settled []string
	for name, baseline := range pending {
		info, err := os.Stat(name)
		if err != nil {
			// Removed (or unreadable): a future rewrite produces a fresh
			// Create event, so stop tracking this instance.
			settled = append(settled, name)
			continue
		}
		if info.Size() != baseline.size || info.ModTime().After(baseline.modTime) {
			fired = true
			settled = append(settled, name)
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if fired {
		t.fired = true
	}
	for _, name := range settled {
		// Only drop the entry we actually judged: between the snapshot and
		// this re-lock, run() may have recorded a FRESH baseline for the same
		// name (file deleted and recreated), and deleting that would lose the
		// very tracking the kqueue fallback exists to provide.
		if current, ok := t.pending[name]; ok && current == pending[name] {
			delete(t.pending, name)
		}
	}
	return t.fired, nil
}

func (t *fileTrigger) Stop() error {
	t.mu.Lock()
	watcher := t.watcher
	done := t.done
	t.watcher = nil
	t.done = nil
	t.mu.Unlock()
	if done != nil {
		close(done)
	}
	if watcher != nil {
		if err := watcher.Close(); err != nil {
			return fmt.Errorf("close filesystem watcher: %w", err)
		}
	}
	t.wg.Wait()
	return nil
}

type commandTrigger struct {
	kind        TriggerType
	command     string
	timeout     time.Duration
	wantSuccess bool
}

func newCommandSuccessTrigger(config Trigger) (RuntimeTrigger, error) {
	return &commandTrigger{kind: TriggerCommandSuccess, command: config.Command, wantSuccess: true}, nil
}

func newCommandFailureTrigger(config Trigger) (RuntimeTrigger, error) {
	return &commandTrigger{kind: TriggerCommandFailure, command: config.Command}, nil
}

func (t *commandTrigger) Type() TriggerType { return t.kind }

func (t *commandTrigger) Start(_ *TriggerContext) error { return nil }

func (t *commandTrigger) Stop() error { return nil }

func (t *commandTrigger) Check(ctx *TriggerContext) (bool, error) {
	if ctx == nil || strings.TrimSpace(ctx.ProjectRoot) == "" {
		return false, errors.New("command trigger requires a project root")
	}
	timeout := t.timeout
	if timeout <= 0 {
		timeout = defaultCommandTriggerTimeout
	}
	parent := ctx.Context
	if parent == nil {
		parent = context.Background()
	}
	runCtx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	args := strings.Fields(t.command)
	if len(args) == 0 {
		return false, errors.New("command trigger requires a command")
	}
	cmd := exec.CommandContext(runCtx, args[0], args[1:]...)
	cmd.Dir = ctx.ProjectRoot
	err := cmd.Run()
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return false, fmt.Errorf("command trigger timed out after %s", timeout)
	}
	if runCtx.Err() != nil {
		return false, fmt.Errorf("run command trigger: %w", runCtx.Err())
	}
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return false, fmt.Errorf("run command trigger %q: %w", t.command, err)
		}
	}
	succeeded := err == nil
	return succeeded == t.wantSuccess, nil
}

// SetTimeout changes the command deadline. It is primarily useful to keep
// tests fast; production callers normally use the default timeout.
func (t *commandTrigger) SetTimeout(timeout time.Duration) { t.timeout = timeout }

type agentSaysTrigger struct {
	pattern *regexp.Regexp
	role    string
}

func newAgentSaysTrigger(config Trigger) (RuntimeTrigger, error) {
	pattern, err := regexp.Compile(config.Pattern)
	if err != nil {
		return nil, fmt.Errorf("compile agent_says pattern: %w", err)
	}
	return &agentSaysTrigger{pattern: pattern, role: config.Role}, nil
}

func (t *agentSaysTrigger) Type() TriggerType { return TriggerAgentSays }

func (t *agentSaysTrigger) Start(_ *TriggerContext) error { return nil }

func (t *agentSaysTrigger) Stop() error { return nil }

func (t *agentSaysTrigger) Check(ctx *TriggerContext) (bool, error) {
	if ctx == nil {
		return false, errors.New("agent_says trigger requires context")
	}
	for _, output := range ctx.Outputs {
		if t.role != "" && output.Role != t.role {
			continue
		}
		if t.pattern.MatchString(output.Text) {
			return true, nil
		}
	}
	return false, nil
}

type allAgentsIdleTrigger struct {
	role string
	idle time.Duration
}

func newAllAgentsIdleTrigger(config Trigger) (RuntimeTrigger, error) {
	return &allAgentsIdleTrigger{role: config.Role, idle: time.Duration(config.IdleMinutes) * time.Minute}, nil
}

func (t *allAgentsIdleTrigger) Type() TriggerType { return TriggerAllAgentsIdle }

func (t *allAgentsIdleTrigger) Start(_ *TriggerContext) error { return nil }

func (t *allAgentsIdleTrigger) Stop() error { return nil }

func (t *allAgentsIdleTrigger) Check(ctx *TriggerContext) (bool, error) {
	if ctx == nil {
		return false, errors.New("all_agents_idle trigger requires context")
	}
	matched := 0
	for _, activity := range ctx.Activities {
		if t.role != "" && activity.Role != t.role {
			continue
		}
		matched++
		if activity.LastActivity.IsZero() || ctx.now().Sub(activity.LastActivity) < t.idle {
			return false, nil
		}
	}
	return matched > 0, nil
}

// ManualTrigger moves to true after Fire. Fire is non-blocking and coalesces
// repeated requests so an operator can safely retry a manual transition.
type ManualTrigger struct {
	label string
	fired chan struct{}
}

func newManualTrigger(config Trigger) (RuntimeTrigger, error) {
	return &ManualTrigger{label: config.Label, fired: make(chan struct{}, 1)}, nil
}

func (t *ManualTrigger) Type() TriggerType { return TriggerManual }

func (t *ManualTrigger) Start(_ *TriggerContext) error { return nil }

func (t *ManualTrigger) Stop() error { return nil }

// Label identifies this manual control in a user interface.
func (t *ManualTrigger) Label() string { return t.label }

// Fire requests the transition and returns whether this call added a request.
func (t *ManualTrigger) Fire() bool {
	select {
	case t.fired <- struct{}{}:
		return true
	default:
		return false
	}
}

func (t *ManualTrigger) Check(_ *TriggerContext) (bool, error) {
	select {
	case <-t.fired:
		return true, nil
	default:
		return false, nil
	}
}

type timeElapsedTrigger struct {
	duration time.Duration
	mu       sync.Mutex
	started  time.Time
}

func newTimeElapsedTrigger(config Trigger) (RuntimeTrigger, error) {
	return &timeElapsedTrigger{duration: time.Duration(config.Minutes) * time.Minute}, nil
}

func (t *timeElapsedTrigger) Type() TriggerType { return TriggerTimeElapsed }

func (t *timeElapsedTrigger) Start(ctx *TriggerContext) error {
	t.mu.Lock()
	t.started = ctx.now()
	t.mu.Unlock()
	return nil
}

func (t *timeElapsedTrigger) Stop() error { return nil }

func (t *timeElapsedTrigger) Check(ctx *TriggerContext) (bool, error) {
	t.mu.Lock()
	started := t.started
	t.mu.Unlock()
	if started.IsZero() {
		return false, errors.New("time_elapsed trigger has not been started")
	}
	return !ctx.now().Before(started.Add(t.duration)), nil
}
