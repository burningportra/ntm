package bv

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// resultCacheFixture is a project with beads data and a fake bv that counts
// its runs and reports the run number and the checksum of the data it read.
type resultCacheFixture struct {
	dir       string
	issues    string
	runsFile  string
	cacheRoot string
}

func newResultCacheFixture(t *testing.T) *resultCacheFixture {
	t.Helper()
	f := &resultCacheFixture{
		dir:       t.TempDir(),
		runsFile:  filepath.Join(t.TempDir(), "runs"),
		cacheRoot: t.TempDir(),
	}
	if err := os.Mkdir(filepath.Join(f.dir, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	f.issues = filepath.Join(f.dir, ".beads", "issues.jsonl")
	f.writeIssues(t, `{"id":"bd-1","status":"open"}`+"\n")

	binDir := t.TempDir()
	script := "#!/bin/sh\n" +
		"echo run >> '" + f.runsFile + "'\n" +
		"sleep \"${FAKE_BV_SLEEP:-0}\"\n" +
		"if [ -n \"$FAKE_BV_EXIT\" ]; then echo 'fake bv failed' >&2; exit \"$FAKE_BV_EXIT\"; fi\n" +
		"printf '{\"data_hash\":\"%s\",\"run\":%s}\\n' \"$(cksum < .beads/issues.jsonl | cut -d' ' -f1)\" \"$(wc -l < '" + f.runsFile + "' | tr -d ' ')\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "bv"), []byte(script), 0o700); err != nil {
		t.Fatalf("write fake bv: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("XDG_CACHE_HOME", f.cacheRoot)
	t.Setenv("BEADS_DIR", "")
	t.Setenv("BEADS_DB", "")
	t.Setenv("FAKE_BV_SLEEP", "0")
	t.Setenv("FAKE_BV_EXIT", "")
	return f
}

func (f *resultCacheFixture) writeIssues(t *testing.T, content string) {
	t.Helper()
	if err := os.WriteFile(f.issues, []byte(content), 0o644); err != nil {
		t.Fatalf("write issues.jsonl: %v", err)
	}
}

func (f *resultCacheFixture) runs(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(f.runsFile)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatalf("read runs file: %v", err)
	}
	return strings.Count(string(data), "run\n")
}

// insights runs bv --robot-insights the way every caller does and returns the
// run number that produced the output.
func (f *resultCacheFixture) insights(t *testing.T, ctx context.Context) (int, error) {
	t.Helper()
	output, err := runWithContextTimeout(ctx, f.dir, DefaultTimeout, "--robot-insights")
	if err != nil {
		return 0, err
	}
	var resp struct {
		Run int `json:"run"`
	}
	if err := json.Unmarshal([]byte(output), &resp); err != nil {
		t.Fatalf("parse fake bv output %q: %v", output, err)
	}
	return resp.Run, nil
}

func (f *resultCacheFixture) mustInsights(t *testing.T) int {
	t.Helper()
	run, err := f.insights(t, context.Background())
	if err != nil {
		t.Fatalf("insights: %v", err)
	}
	return run
}

func waitForRuns(t *testing.T, f *resultCacheFixture, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for f.runs(t) < want {
		if time.Now().After(deadline) {
			t.Fatalf("fake bv ran %d times, want %d", f.runs(t), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func cachePartials(t *testing.T, f *resultCacheFixture) []string {
	t.Helper()
	partials, err := filepath.Glob(filepath.Join(f.cacheRoot, "ntm", "bv", "*", "*.partial"))
	if err != nil {
		t.Fatalf("glob partials: %v", err)
	}
	return partials
}

// bd-azydz: status, snapshot and the projection refresh each paid for their
// own bv run. The cache lives on disk, so a second caller reuses the first run.
func TestResultCacheReusesOneRunWhileBeadsDataIsUnchanged(t *testing.T) {
	f := newResultCacheFixture(t)

	if run := f.mustInsights(t); run != 1 {
		t.Fatalf("first call served run %d, want 1", run)
	}
	if run := f.mustInsights(t); run != 1 {
		t.Fatalf("second call served run %d, want the cached run 1", run)
	}
	if got := f.runs(t); got != 1 {
		t.Fatalf("bv ran %d times for unchanged data, want 1", got)
	}

	// Triage has its own entry.
	if _, err := runWithContextTimeout(context.Background(), f.dir, DefaultTimeout, "--robot-triage"); err != nil {
		t.Fatalf("triage: %v", err)
	}
	if got := f.runs(t); got != 2 {
		t.Fatalf("bv ran %d times after a first triage call, want 2", got)
	}
}

func TestResultCacheNeverServesAnEntryAfterTheBeadsDataChanges(t *testing.T) {
	f := newResultCacheFixture(t)
	info, err := os.Stat(f.issues)
	if err != nil {
		t.Fatalf("stat issues.jsonl: %v", err)
	}
	if run := f.mustInsights(t); run != 1 {
		t.Fatalf("first call served run %d, want 1", run)
	}

	// Same size and the original mtime: only the content tells them apart.
	f.writeIssues(t, `{"id":"bd-1","status":"done"}`+"\n")
	if err := os.Chtimes(f.issues, info.ModTime(), info.ModTime()); err != nil {
		t.Fatalf("restore mtime: %v", err)
	}
	if run := f.mustInsights(t); run != 2 {
		t.Fatalf("call after a content change served run %d, want a fresh run 2", run)
	}

	// Any other .beads/*.jsonl file is part of the key too.
	if err := os.WriteFile(filepath.Join(f.dir, ".beads", "beads.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write beads.jsonl: %v", err)
	}
	if run := f.mustInsights(t); run != 3 {
		t.Fatalf("call after adding beads.jsonl served run %d, want a fresh run 3", run)
	}
}

// A change that lands while bv runs leaves the run keyed on the data it
// started from, which no later call can match.
func TestResultCacheDropsARunWhoseDataChangedMidway(t *testing.T) {
	f := newResultCacheFixture(t)
	t.Setenv("FAKE_BV_SLEEP", "0.5")

	done := make(chan error, 1)
	go func() {
		_, err := f.insights(t, context.Background())
		done <- err
	}()
	waitForRuns(t, f, 1)
	f.writeIssues(t, `{"id":"bd-1","status":"open"}`+"\n"+`{"id":"bd-2","status":"open"}`+"\n")
	if err := <-done; err != nil {
		t.Fatalf("first call: %v", err)
	}

	t.Setenv("FAKE_BV_SLEEP", "0")
	if run := f.mustInsights(t); run != 2 {
		t.Fatalf("call after a mid-run change served run %d, want a fresh run 2", run)
	}
}

func TestResultCacheExpiresEntriesAfterMaxAge(t *testing.T) {
	f := newResultCacheFixture(t)
	if run := f.mustInsights(t); run != 1 {
		t.Fatalf("first call served run %d, want 1", run)
	}
	entries, err := filepath.Glob(filepath.Join(f.cacheRoot, "ntm", "bv", "*", "robot-insights-*.json"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("cache entries = %v (err %v), want one", entries, err)
	}
	old := time.Now().Add(-resultCacheMaxAge - time.Minute)
	if err := os.Chtimes(entries[0], old, old); err != nil {
		t.Fatalf("age entry: %v", err)
	}
	if run := f.mustInsights(t); run != 2 {
		t.Fatalf("call after max age served run %d, want a fresh run 2", run)
	}
}

// bd-jv2dq: the projection refresh gives bv 3 s, less than bv needs on a large
// project, so a run killed with its caller never filled the cache. The run
// now outlives the caller, the next caller waits for it instead of starting
// another, and a later caller is served its output.
func TestResultCacheRunOutlivesItsCallerAndFillsTheCache(t *testing.T) {
	f := newResultCacheFixture(t)
	t.Setenv("FAKE_BV_SLEEP", "0.6")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := f.insights(t, ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("short-budget call error = %v, want deadline exceeded", err)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	if _, err := f.insights(t, ctx2); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second short-budget call error = %v, want deadline exceeded", err)
	}
	if got := f.runs(t); got != 1 {
		t.Fatalf("bv ran %d times while one run was in flight, want 1", got)
	}

	if run := f.mustInsights(t); run != 1 {
		t.Fatalf("call after the run finished served run %d, want run 1", run)
	}
	if got := f.runs(t); got != 1 {
		t.Fatalf("bv ran %d times, want the one run that outlived its caller", got)
	}
}

// A run whose ntm process exited leaves its output in a partial file; the
// next caller adopts it instead of running bv again.
func TestResultCacheAdoptsAFinishedRunFromAnotherProcess(t *testing.T) {
	f := newResultCacheFixture(t)
	cache, ok := newResultCache(f.dir, []string{"--robot-insights"})
	if !ok {
		t.Fatal("newResultCache: cache does not apply to the fixture")
	}
	partial := filepath.Join(cache.dir, cache.name+"-"+cache.key+".12345.partial")
	if err := os.WriteFile(partial, []byte(`{"data_hash":"abc","run":7}`), 0o600); err != nil {
		t.Fatalf("write partial: %v", err)
	}
	if run := f.mustInsights(t); run != 7 {
		t.Fatalf("call served run %d, want the adopted run 7", run)
	}
	if got := f.runs(t); got != 0 {
		t.Fatalf("bv ran %d times, want 0 after adopting a finished run", got)
	}
	if partials := cachePartials(t, f); len(partials) != 0 {
		t.Fatalf("partials left after adoption: %v", partials)
	}
}

func TestResultCacheRerunsAfterAnAbandonedRun(t *testing.T) {
	f := newResultCacheFixture(t)
	cache, ok := newResultCache(f.dir, []string{"--robot-insights"})
	if !ok {
		t.Fatal("newResultCache: cache does not apply to the fixture")
	}
	partial := filepath.Join(cache.dir, cache.name+"-"+cache.key+".12345.partial")
	if err := os.WriteFile(partial, []byte(`{"data_hash":`), 0o600); err != nil {
		t.Fatalf("write partial: %v", err)
	}
	old := time.Now().Add(-resultCacheRunWindow - time.Second)
	if err := os.Chtimes(partial, old, old); err != nil {
		t.Fatalf("age partial: %v", err)
	}
	if run := f.mustInsights(t); run != 1 {
		t.Fatalf("call served run %d, want a fresh run 1", run)
	}
	if _, err := os.Stat(partial); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("abandoned partial still present (stat err %v)", err)
	}
}

func TestResultCacheDoesNotCacheFailuresOrIncompleteOutput(t *testing.T) {
	f := newResultCacheFixture(t)
	t.Setenv("FAKE_BV_EXIT", "3")
	if _, err := f.insights(t, context.Background()); err == nil || !strings.Contains(err.Error(), "fake bv failed") {
		t.Fatalf("failing bv error = %v, want its stderr", err)
	}
	t.Setenv("FAKE_BV_EXIT", "")
	if run := f.mustInsights(t); run != 2 {
		t.Fatalf("call after a failure served run %d, want a fresh run 2", run)
	}
	if partials := cachePartials(t, f); len(partials) != 0 {
		t.Fatalf("partials left behind: %v", partials)
	}
}

func TestResultCacheAppliesOnlyWithBeadsDataAndCachedCommands(t *testing.T) {
	f := newResultCacheFixture(t)

	if _, err := runWithContextTimeout(context.Background(), f.dir, DefaultTimeout, "--robot-plan"); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if _, err := runWithContextTimeout(context.Background(), f.dir, DefaultTimeout, "--robot-plan"); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if got := f.runs(t); got != 2 {
		t.Fatalf("bv ran %d times for two uncached commands, want 2", got)
	}

	if _, ok := newResultCache(f.dir, []string{"--robot-insights"}); !ok {
		t.Fatal("cache should apply with beads data")
	}
	t.Setenv("BEADS_DIR", "/elsewhere/.beads")
	if _, ok := newResultCache(f.dir, []string{"--robot-insights"}); ok {
		t.Fatal("cache applied with BEADS_DIR pointing outside the project")
	}
	t.Setenv("BEADS_DIR", "")
	if err := os.WriteFile(filepath.Join(f.dir, ".beads", "redirect"), []byte("../other/.beads\n"), 0o644); err != nil {
		t.Fatalf("write redirect: %v", err)
	}
	if _, ok := newResultCache(f.dir, []string{"--robot-insights"}); ok {
		t.Fatal("cache applied to a worktree redirected to another .beads")
	}

	empty := t.TempDir()
	if err := os.Mkdir(filepath.Join(empty, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	if _, ok := newResultCache(empty, []string{"--robot-insights"}); ok {
		t.Fatal("cache applied without any .beads/*.jsonl file")
	}
}
