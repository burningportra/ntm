// Package bv provides integration with the beads_viewer (bv) tool.
// result_cache.go keeps one cache of bv robot results that every ntm process
// shares, keyed on the beads data bv reads.
package bv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// On a large project bv spends seconds to tens of seconds on every robot
// command, and --robot-status, --robot-snapshot and the projection refresh
// each ran it again in their own process (bd-azydz, bd-jv2dq). The cache
// lives on disk, so those processes share one run.
//
// An entry is keyed on the command, the bv binary and the name, size, mtime
// and content of every .beads/*.jsonl file, which is all bv loads. Any write
// to those files changes the key, so an entry can only be served while the
// data is exactly what its run started from. A run started by a caller whose
// budget is shorter than bv's runtime keeps going after the caller gives up,
// writing to a partial file; whichever process looks next adopts the finished
// output, so a short-budget caller still warms the cache for the next one.
const (
	// resultCacheMaxAge bounds how long an entry is served while the beads
	// data stays the same: bv's scores decay with time, so an old result
	// drifts even when no bead changed.
	resultCacheMaxAge = 5 * time.Minute
	// resultCacheRunWindow is how long an unfinished partial file counts as a
	// run in flight. Its process may have outlived the ntm process that
	// started it, so nothing else would notice that it died.
	resultCacheRunWindow = 2 * DefaultTimeout
	// resultCachePollInterval is how often a caller waiting on another
	// process's run checks whether it finished.
	resultCachePollInterval = 100 * time.Millisecond
)

// cachedRobotCommands are the bv robot commands whose output depends only on
// the beads data, apart from the slow time decay resultCacheMaxAge bounds.
var cachedRobotCommands = map[string]struct{}{
	"--robot-triage":   {},
	"--robot-insights": {},
}

// resultCacheStartMu serializes the in-flight check and the start of a run
// within one process, so concurrent callers start one run between them.
var resultCacheStartMu sync.Mutex

// errResultCacheFiles means the cache directory took no new files, so the
// caller runs bv without the cache.
var errResultCacheFiles = errors.New("bv result cache files unavailable")

// resultCacheTestRoot is the cache root inside a test binary. Tests in every
// package reach bv, often with a fake bv against a real checkout, and must
// neither touch the user's cache dir nor get answers from it, so in a test
// binary the cache is off unless a test in this package sets this to a temp
// dir. Tests that set it run sequentially, before any parallel test starts.
var resultCacheTestRoot string

// resultCache addresses one command's entry for one project and data state.
type resultCache struct {
	dir    string // per-project cache directory
	name   string // command without its dashes, e.g. robot-triage
	key    string // digest of the command, the bv binary and the beads data
	bvPath string
}

type resultCacheOutcome struct {
	output string
	err    error
}

// resultCacheRun is a bv process this process started.
type resultCacheRun struct {
	cache   *resultCache
	args    []string
	cmd     *exec.Cmd
	partial string
	stderr  string
	done    chan resultCacheOutcome
}

// runCachedRobotCommand answers a cacheable bv robot command from the shared
// cache, waits for a run another caller has in flight, or starts one. handled
// is false when the cache does not apply, and the caller then runs bv itself.
// When parent ends first the run continues, and its output fills the cache
// for the next caller.
func runCachedRobotCommand(parent context.Context, dir string, timeout time.Duration, args []string) (output string, handled bool, err error) {
	cache, ok := newResultCache(dir, args)
	if !ok {
		return "", false, nil
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	timedOut := fmt.Errorf("bv timed out after %v: %w", timeout, ErrTimeout)

	for {
		if output, ok := cache.load(time.Now()); ok {
			return output, true, nil
		}
		run, inFlight, err := cache.adoptOrStart(dir, args, timeout)
		if errors.Is(err, errResultCacheFiles) {
			return "", false, nil
		}
		if err != nil {
			return "", true, err
		}
		if run != nil {
			select {
			case outcome := <-run.done:
				return outcome.output, true, outcome.err
			case <-parent.Done():
				return "", true, parent.Err()
			case <-deadline.C:
				return "", true, timedOut
			}
		}
		if !inFlight {
			// A finished run was adopted; load it.
			continue
		}
		poll := time.NewTimer(resultCachePollInterval)
		select {
		case <-parent.Done():
			poll.Stop()
			return "", true, parent.Err()
		case <-deadline.C:
			poll.Stop()
			return "", true, timedOut
		case <-poll.C:
		}
	}
}

// newResultCache resolves the entry for args in dir. It reports false when
// the command is not cacheable or the beads data cannot be fingerprinted.
func newResultCache(dir string, args []string) (*resultCache, bool) {
	if len(args) != 1 {
		return nil, false
	}
	if _, ok := cachedRobotCommands[args[0]]; !ok {
		return nil, false
	}
	// BEADS_DIR and BEADS_DB point bv at data outside dir/.beads, which the
	// key below would not cover.
	if strings.TrimSpace(os.Getenv("BEADS_DIR")) != "" || strings.TrimSpace(os.Getenv("BEADS_DB")) != "" {
		return nil, false
	}
	bvPath, err := exec.LookPath("bv")
	if err != nil {
		return nil, false
	}
	bvInfo, err := os.Stat(bvPath)
	if err != nil {
		return nil, false
	}

	digest := sha256.New()
	fmt.Fprintf(digest, "%s\x00%s\x00%d\x00%d\x00", args[0], bvPath, bvInfo.Size(), bvInfo.ModTime().UnixNano())
	if !hashBeadsData(digest, filepath.Join(dir, ".beads")) {
		return nil, false
	}

	root, err := resultCacheRoot()
	if err != nil {
		return nil, false
	}
	projectSum := sha256.Sum256([]byte(dir))
	cacheDir := filepath.Join(root, hex.EncodeToString(projectSum[:8]))
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return nil, false
	}
	return &resultCache{
		dir:    cacheDir,
		name:   strings.TrimLeft(args[0], "-"),
		key:    hex.EncodeToString(digest.Sum(nil))[:32],
		bvPath: bvPath,
	}, true
}

// resultCacheRoot is resultCacheTestRoot in a test binary and
// userResultCacheRoot everywhere else.
func resultCacheRoot() (string, error) {
	if testing.Testing() {
		if resultCacheTestRoot == "" {
			return "", errors.New("bv result cache is off in tests")
		}
		return resultCacheTestRoot, nil
	}
	return userResultCacheRoot()
}

// userResultCacheRoot honors XDG_CACHE_HOME on every platform, the way the
// ensemble context-pack cache does.
func userResultCacheRoot() (string, error) {
	if xdg := strings.TrimSpace(os.Getenv("XDG_CACHE_HOME")); xdg != "" {
		return filepath.Join(xdg, "ntm", "bv"), nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return "", fmt.Errorf("cache dir unavailable: %w", err)
		}
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "ntm", "bv"), nil
}

// hashBeadsData writes every .beads/*.jsonl file into digest and reports
// whether there was at least one and each could be read whole. bv loads its
// issues from these files only.
func hashBeadsData(digest hash.Hash, beadsDir string) bool {
	// A worktree's redirect file sends bv to another checkout's .beads.
	if _, err := os.Lstat(filepath.Join(beadsDir, "redirect")); !errors.Is(err, fs.ErrNotExist) {
		return false
	}
	entries, err := os.ReadDir(beadsDir)
	if err != nil {
		return false
	}
	found := false
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		if !entry.Type().IsRegular() || !hashBeadsFile(digest, filepath.Join(beadsDir, entry.Name())) {
			return false
		}
		found = true
	}
	return found
}

// hashBeadsFile reports false when the file changed while it was read, since
// the digest would then describe no state bv could have seen.
func hashBeadsFile(digest hash.Hash, path string) bool {
	before, err := os.Stat(path)
	if err != nil {
		return false
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	fmt.Fprintf(digest, "%s\x00%d\x00%d\x00", filepath.Base(path), before.Size(), before.ModTime().UnixNano())
	n, err := io.Copy(digest, file)
	if err != nil || n != before.Size() {
		return false
	}
	after, err := os.Stat(path)
	if err != nil {
		return false
	}
	return os.SameFile(before, after) && after.Size() == before.Size() && after.ModTime().Equal(before.ModTime())
}

// completeRobotOutput reports whether data is a whole bv robot response.
// Every bv robot response carries the data_hash of the beads it read, and a
// run that is still writing or died midway leaves invalid JSON.
func completeRobotOutput(data []byte) bool {
	var envelope struct {
		DataHash string `json:"data_hash"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return false
	}
	return strings.TrimSpace(envelope.DataHash) != ""
}

func (c *resultCache) entryPath() string {
	return filepath.Join(c.dir, c.name+"-"+c.key+".json")
}

func partialStderrPath(partial string) string {
	return strings.TrimSuffix(partial, ".partial") + ".stderr"
}

// load returns the entry for the current data if it is younger than
// resultCacheMaxAge.
func (c *resultCache) load(now time.Time) (string, bool) {
	path := c.entryPath()
	info, err := os.Stat(path)
	if err != nil || now.Sub(info.ModTime()) > resultCacheMaxAge {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil || !completeRobotOutput(data) {
		return "", false
	}
	return strings.TrimSpace(string(data)), true
}

// adoptOrStart moves a finished run's output into place, reports a run still
// in flight, or starts one. It returns a nil run and inFlight false after an
// adoption.
func (c *resultCache) adoptOrStart(dir string, args []string, timeout time.Duration) (run *resultCacheRun, inFlight bool, err error) {
	resultCacheStartMu.Lock()
	defer resultCacheStartMu.Unlock()

	now := time.Now()
	partials, _ := filepath.Glob(filepath.Join(c.dir, c.name+"-"+c.key+".*.partial"))
	for _, partial := range partials {
		info, statErr := os.Stat(partial)
		if statErr != nil {
			continue
		}
		data, readErr := os.ReadFile(partial)
		if readErr != nil {
			continue
		}
		age := now.Sub(info.ModTime())
		if completeRobotOutput(data) {
			if age <= resultCacheMaxAge && os.Rename(partial, c.entryPath()) == nil {
				_ = os.Remove(partialStderrPath(partial))
				return nil, false, nil
			}
			continue
		}
		if age < resultCacheRunWindow {
			inFlight = true
			continue
		}
		_ = os.Remove(partial)
		_ = os.Remove(partialStderrPath(partial))
	}
	if inFlight {
		return nil, true, nil
	}
	run, err = c.start(dir, args, timeout)
	return run, false, err
}

// start launches bv with its output in a partial file, so the run can finish
// and be adopted after this process exits.
func (c *resultCache) start(dir string, args []string, timeout time.Duration) (*resultCacheRun, error) {
	stdout, err := os.CreateTemp(c.dir, c.name+"-"+c.key+".*.partial")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errResultCacheFiles, err)
	}
	stderrPath := partialStderrPath(stdout.Name())
	stderr, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		stdout.Close()
		_ = os.Remove(stdout.Name())
		return nil, fmt.Errorf("%w: %v", errResultCacheFiles, err)
	}

	cmd := exec.Command(c.bvPath, args...)
	cmd.Dir = dir
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	startErr := cmd.Start()
	// The child holds its own copies of both descriptors.
	stdout.Close()
	stderr.Close()
	if startErr != nil {
		_ = os.Remove(stdout.Name())
		_ = os.Remove(stderrPath)
		return nil, fmt.Errorf("bv %s: %w", strings.Join(args, " "), startErr)
	}

	run := &resultCacheRun{
		cache:   c,
		args:    append([]string(nil), args...),
		cmd:     cmd,
		partial: stdout.Name(),
		stderr:  stderrPath,
		done:    make(chan resultCacheOutcome, 1),
	}
	go run.finish(timeout)
	return run, nil
}

// finish waits for the run, kills it at timeout while this process lives, and
// caches complete output.
func (r *resultCacheRun) finish(timeout time.Duration) {
	var killed atomic.Bool
	kill := time.AfterFunc(timeout, func() {
		killed.Store(true)
		_ = r.cmd.Process.Kill()
	})
	waitErr := r.cmd.Wait()
	kill.Stop()
	r.done <- r.settle(waitErr, killed.Load(), timeout)
}

func (r *resultCacheRun) settle(waitErr error, killed bool, timeout time.Duration) resultCacheOutcome {
	stderr, _ := os.ReadFile(r.stderr)
	_ = os.Remove(r.stderr)
	if waitErr != nil {
		_ = os.Remove(r.partial)
		if killed {
			return resultCacheOutcome{err: fmt.Errorf("bv timed out after %v: %w", timeout, ErrTimeout)}
		}
		if strings.Contains(string(stderr), "No baseline found") {
			return resultCacheOutcome{err: ErrNoBaseline}
		}
		return resultCacheOutcome{err: fmt.Errorf("bv %s: %w: %s", strings.Join(r.args, " "), waitErr, stderr)}
	}

	data, err := os.ReadFile(r.partial)
	if errors.Is(err, fs.ErrNotExist) {
		// Another caller adopted the finished output already.
		data, err = os.ReadFile(r.cache.entryPath())
	}
	if err != nil {
		return resultCacheOutcome{err: fmt.Errorf("bv %s: read result cache file: %w", strings.Join(r.args, " "), err)}
	}
	if !completeRobotOutput(data) {
		// Hand bv's output back as the uncached path would, without caching it.
		_ = os.Remove(r.partial)
		return resultCacheOutcome{output: strings.TrimSpace(string(data))}
	}
	if err := os.Rename(r.partial, r.cache.entryPath()); err == nil {
		r.cache.prune(time.Now())
	}
	return resultCacheOutcome{output: strings.TrimSpace(string(data))}
}

// prune drops this command's entries for earlier data states and partial
// files whose runs were abandoned.
func (c *resultCache) prune(now time.Time) {
	entries, _ := filepath.Glob(filepath.Join(c.dir, c.name+"-*.json"))
	for _, entry := range entries {
		if entry != c.entryPath() {
			_ = os.Remove(entry)
		}
	}
	partials, _ := filepath.Glob(filepath.Join(c.dir, c.name+"-*.partial"))
	for _, partial := range partials {
		if info, err := os.Stat(partial); err == nil && now.Sub(info.ModTime()) >= resultCacheRunWindow {
			_ = os.Remove(partial)
			_ = os.Remove(partialStderrPath(partial))
		}
	}
}
