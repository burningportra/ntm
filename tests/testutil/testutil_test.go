package testutil

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	processGroupOwnerCrashEnv    = "_NTM_TEST_PROCESS_GROUP_OWNER_CRASH"
	processGroupOwnerCrashPIDEnv = "_NTM_TEST_PROCESS_GROUP_OWNER_CHILD_PID"
)

func TestProcessGroupGuardianKillsDescendantsWhenOwnerExits(t *testing.T) {
	if os.Getenv(processGroupOwnerCrashEnv) == "1" {
		pidPath := os.Getenv(processGroupOwnerCrashPIDEnv)
		cmd := exec.Command("/bin/sh", "-c", `sleep 600 & child=$!; printf '%s\n' "$child" > "$1"; wait "$child"`, "owner-crash", pidPath)
		group, err := NewProcessGroupForTest(context.Background(), cmd)
		if err != nil {
			t.Fatalf("create owner-crash process group: %v", err)
		}
		if err := cmd.Start(); err != nil {
			closeErr := group.Close()
			t.Fatalf("start owner-crash target: %v; close owned process group: %v", err, closeErr)
		}
		for {
			time.Sleep(time.Second)
			runtime.KeepAlive(group)
		}
	}
	if runtime.GOOS != "linux" {
		t.Skip("owner-crash descendant assertion uses Linux /proc")
	}

	pidPath := filepath.Join(t.TempDir(), "descendant.pid")
	owner := exec.Command(os.Args[0], "-test.run=^TestProcessGroupGuardianKillsDescendantsWhenOwnerExits$")
	overrides := map[string]string{
		processGroupOwnerCrashEnv:    "1",
		processGroupOwnerCrashPIDEnv: pidPath,
		processGroupGuardianEnv:      "0",
	}
	owner.Env = make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, replaced := overrides[key]; !replaced {
			owner.Env = append(owner.Env, entry)
		}
	}
	for key, value := range overrides {
		owner.Env = append(owner.Env, key+"="+value)
	}
	var output bytes.Buffer
	owner.Stdout = &output
	owner.Stderr = &output
	owner.WaitDelay = 2 * time.Second
	if err := owner.Start(); err != nil {
		t.Fatalf("start owner-crash helper: %v", err)
	}
	joined := false
	defer func() {
		if joined {
			return
		}
		_ = owner.Process.Kill()
		_ = owner.Wait()
	}()

	var descendantPID int
	readinessDeadline := time.Now().Add(30 * time.Second)
	for {
		pidBytes, err := os.ReadFile(pidPath)
		if err == nil {
			descendantPID, err = strconv.Atoi(strings.TrimSpace(string(pidBytes)))
			if err != nil || descendantPID <= 0 {
				t.Fatalf("parse owner-crash descendant pid %q: %v", pidBytes, err)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read owner-crash descendant pid: %v", err)
		}
		if time.Now().After(readinessDeadline) {
			t.Fatalf("owner-crash helper did not publish descendant pid: output=%s", output.String())
		}
		time.Sleep(25 * time.Millisecond)
	}

	if err := owner.Process.Kill(); err != nil {
		t.Fatalf("kill owner-crash helper: %v", err)
	}
	waitErr := owner.Wait()
	joined = true
	if waitErr == nil {
		t.Fatal("killed owner-crash helper exited successfully")
	}

	statusPath := filepath.Join("/proc", strconv.Itoa(descendantPID), "status")
	terminationDeadline := time.Now().Add(10 * time.Second)
	for {
		status, err := os.ReadFile(statusPath)
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		if err != nil {
			t.Fatalf("read owner-crash descendant status: %v", err)
		}
		for _, line := range strings.Split(string(status), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[0] == "State:" && fields[1] == "Z" {
				return
			}
		}
		if time.Now().After(terminationDeadline) {
			t.Fatalf("descendant process %d survived owner exit", descendantPID)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestNewTestLoggerStdout(t *testing.T) {
	logger := NewTestLoggerStdout(t)
	if logger == nil {
		t.Fatal("expected non-nil logger")
	}
	if logger.testName != t.Name() {
		t.Errorf("expected testName %q, got %q", t.Name(), logger.testName)
	}

	// Test logging doesn't panic
	logger.Log("test message %d", 42)
	logger.LogSection("test section")
}

func TestNewTestLogger(t *testing.T) {
	tmpDir := t.TempDir()
	logger := NewTestLogger(t, tmpDir)
	if logger == nil {
		t.Fatal("expected non-nil logger")
	}

	// Log something
	logger.Log("test log entry")
	logger.LogSection("section")

	// Verify a log file was created
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("failed to read temp dir: %v", err)
	}
	if len(entries) == 0 {
		t.Error("expected log file to be created")
	}

	// Verify file contains content
	logPath := filepath.Join(tmpDir, entries[0].Name())
	content, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("failed to read log file: %v", err)
	}
	if len(content) == 0 {
		t.Error("expected log file to have content")
	}
}

func TestLoggerElapsed(t *testing.T) {
	logger := NewTestLoggerStdout(t)
	elapsed := logger.Elapsed()
	if elapsed < 0 {
		t.Errorf("elapsed time should be non-negative: %v", elapsed)
	}
}

func TestTryWithGlobalTmuxTestLock(t *testing.T) {
	ran := false
	acquired := tryWithGlobalTmuxTestLock(func() {
		ran = true
	})

	if acquired && !ran {
		t.Fatal("expected callback to run when lock acquisition succeeds")
	}
	if !acquired && ran {
		t.Fatal("expected callback to be skipped when lock is busy")
	}
}

func TestRequireTmux(t *testing.T) {
	// This test just verifies RequireTmux doesn't panic
	// It will skip if tmux is not installed
	RequireTmux(t)
	t.Log("tmux is installed")
}

func TestSessionExists(t *testing.T) {
	AcquireGlobalTmuxTestLockForTest(t)

	// Test with a session that definitely doesn't exist
	exists := SessionExists("nonexistent_session_" + t.Name())
	if exists {
		t.Error("session should not exist")
	}
}

func TestIsolateTmuxTestProcess(t *testing.T) {
	t.Setenv("TMUX", "/tmp/ambient-tmux,1,0")
	t.Setenv("TMUX_PANE", "%9")
	t.Setenv("TMUX_TMPDIR", "/tmp/ambient-tmux-root")

	cleanupTmux, err := IsolateTmuxTestProcess()
	if err != nil {
		t.Fatalf("IsolateTmuxTestProcess() error = %v", err)
	}
	t.Cleanup(func() {
		if err := cleanupTmux(); err != nil {
			t.Errorf("cleanup IsolateTmuxTestProcess(): %v", err)
		}
	})
	if got := os.Getenv("TMUX"); got != "" {
		t.Fatalf("TMUX = %q, want empty", got)
	}
	if got := os.Getenv("TMUX_PANE"); got != "" {
		t.Fatalf("TMUX_PANE = %q, want empty", got)
	}
	tmuxTmpDir := os.Getenv("TMUX_TMPDIR")
	if tmuxTmpDir == "" || tmuxTmpDir == "/tmp/ambient-tmux-root" {
		t.Fatalf("TMUX_TMPDIR = %q, want unique test directory", tmuxTmpDir)
	}
	info, err := os.Stat(tmuxTmpDir)
	if err != nil {
		t.Fatalf("stat TMUX_TMPDIR: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("TMUX_TMPDIR = %q, want directory", tmuxTmpDir)
	}
	if err := validateTmuxSocketRoot(tmuxTmpDir); err != nil {
		t.Fatalf("TMUX_TMPDIR = %q cannot host a portable tmux socket: %v", tmuxTmpDir, err)
	}
	if err := cleanupTmux(); err != nil {
		t.Fatalf("cleanup IsolateTmuxTestProcess(): %v", err)
	}
	if _, err := os.Stat(tmuxTmpDir); !os.IsNotExist(err) {
		t.Fatalf("isolated tmux cleanup left %q behind; stat error = %v", tmuxTmpDir, err)
	}
}

func TestIsolateCacheHomeProcess(t *testing.T) {
	ambient := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", ambient)
	t.Setenv("GOCACHE", "")
	wantGoCache := ""
	if base, err := os.UserCacheDir(); err == nil {
		wantGoCache = filepath.Join(base, "go-build")
	}

	cleanupCache, err := IsolateCacheHomeProcess()
	if err != nil {
		t.Fatalf("IsolateCacheHomeProcess() error = %v", err)
	}
	cacheHome := os.Getenv("XDG_CACHE_HOME")
	if cacheHome == "" || cacheHome == ambient {
		t.Fatalf("XDG_CACHE_HOME = %q, want a new private directory", cacheHome)
	}
	entries, err := os.ReadDir(cacheHome)
	if err != nil || len(entries) != 0 {
		t.Fatalf("private cache dir holds %v (err %v), want an empty directory", entries, err)
	}
	if got := os.Getenv("GOCACHE"); got != wantGoCache {
		t.Fatalf("GOCACHE = %q, want the build cache from before isolation %q", got, wantGoCache)
	}
	// Binaries a test runs inherit the private dir.
	out, err := exec.Command("/bin/sh", "-c", `printf %s "$XDG_CACHE_HOME"`).Output()
	if err != nil || string(out) != cacheHome {
		t.Fatalf("child process saw XDG_CACHE_HOME = %q (err %v), want %q", out, err, cacheHome)
	}
	if err := cleanupCache(); err != nil {
		t.Fatalf("cleanup IsolateCacheHomeProcess(): %v", err)
	}
	if _, err := os.Stat(cacheHome); !os.IsNotExist(err) {
		t.Fatalf("cache cleanup left %q behind; stat error = %v", cacheHome, err)
	}
}

func TestIsolateTmuxTestProcessPreservesConfiguredFakeBinaryEnvironment(t *testing.T) {
	fakeDir := t.TempDir()
	invoked := filepath.Join(fakeDir, "invoked")
	fakeBinary := filepath.Join(fakeDir, "tmux")
	ownedTmuxRoot := t.TempDir()
	script := "#!/bin/sh\n: > \"$NTM_TEST_FAKE_TMUX_INVOKED\"\nexit 64\n"
	if err := os.WriteFile(fakeBinary, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake tmux binary: %v", err)
	}
	t.Setenv("NTM_TEST_FAKE_TMUX_INVOKED", invoked)
	t.Setenv("NTM_TEST_TMUX_ENV_OWNED", "1")
	t.Setenv("NTM_TMUX_BINARY", fakeBinary)
	t.Setenv("TMUX", "fake-server,1,0")
	t.Setenv("TMUX_PANE", "%9")
	t.Setenv("TMUX_TMPDIR", ownedTmuxRoot)

	cleanupTmux, err := IsolateTmuxTestProcess()
	if err != nil {
		t.Fatalf("IsolateTmuxTestProcess() error = %v", err)
	}
	if got := os.Getenv("TMUX"); got != "fake-server,1,0" {
		t.Fatalf("TMUX = %q, want caller-owned fake environment", got)
	}
	if got := os.Getenv("TMUX_PANE"); got != "%9" {
		t.Fatalf("TMUX_PANE = %q, want caller-owned fake environment", got)
	}
	if got := os.Getenv("TMUX_TMPDIR"); got != ownedTmuxRoot {
		t.Fatalf("TMUX_TMPDIR = %q, want caller-owned %q", got, ownedTmuxRoot)
	}
	if err := cleanupTmux(); err != nil {
		t.Fatalf("cleanup IsolateTmuxTestProcess(): %v", err)
	}
	if _, err := os.Stat(invoked); !os.IsNotExist(err) {
		t.Fatalf("configured fake tmux was invoked during cleanup; stat error = %v", err)
	}
	if info, err := os.Stat(ownedTmuxRoot); err != nil || !info.IsDir() {
		t.Fatalf("caller-owned tmux root %q was changed during no-op cleanup: info=%v err=%v", ownedTmuxRoot, info, err)
	}
}

func TestIsolateTmuxTestProcessStillIsolatesConfiguredBinary(t *testing.T) {
	fakeDir := t.TempDir()
	invoked := filepath.Join(fakeDir, "invoked")
	fakeBinary := filepath.Join(fakeDir, "tmux")
	script := "#!/bin/sh\n: > \"$NTM_TEST_FAKE_TMUX_INVOKED\"\nexit 64\n"
	if err := os.WriteFile(fakeBinary, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake tmux binary: %v", err)
	}
	t.Setenv("NTM_TEST_FAKE_TMUX_INVOKED", invoked)
	t.Setenv("NTM_TMUX_BINARY", fakeBinary)
	t.Setenv("TMUX", "ambient-server,1,0")
	t.Setenv("TMUX_PANE", "%8")
	t.Setenv("TMUX_TMPDIR", "/tmp/ambient-tmux-root")

	cleanupTmux, err := IsolateTmuxTestProcess()
	if err != nil {
		t.Fatalf("IsolateTmuxTestProcess() error = %v", err)
	}
	tmuxRoot := os.Getenv("TMUX_TMPDIR")
	if os.Getenv("TMUX") != "" || os.Getenv("TMUX_PANE") != "" ||
		tmuxRoot == "" || tmuxRoot == "/tmp/ambient-tmux-root" {
		t.Fatalf("configured binary retained ambient tmux environment: TMUX=%q TMUX_PANE=%q TMUX_TMPDIR=%q",
			os.Getenv("TMUX"), os.Getenv("TMUX_PANE"), tmuxRoot)
	}
	if err := cleanupTmux(); err != nil {
		t.Fatalf("cleanup IsolateTmuxTestProcess(): %v", err)
	}
	if _, err := os.Stat(invoked); !os.IsNotExist(err) {
		t.Fatalf("configured fake tmux was invoked during cleanup; stat error = %v", err)
	}
	if _, err := os.Stat(tmuxRoot); !os.IsNotExist(err) {
		t.Fatalf("isolated tmux cleanup left %q behind; stat error = %v", tmuxRoot, err)
	}
}

func TestShortTmuxTempDirCandidatesOverrideFirstAndDeduplicated(t *testing.T) {
	t.Setenv(tmuxTestTempBaseEnv, os.TempDir()+string(filepath.Separator))

	candidates := shortTmuxTempDirCandidates()
	if len(candidates) == 0 {
		t.Fatal("shortTmuxTempDirCandidates() returned no candidates")
	}
	want, err := filepath.Abs(os.TempDir())
	if err != nil {
		t.Fatalf("filepath.Abs(%q): %v", os.TempDir(), err)
	}
	if got := candidates[0]; got.source != tmuxTestTempBaseEnv || got.path != want {
		t.Fatalf("first candidate = %#v, want source %q path %q", got, tmuxTestTempBaseEnv, want)
	}

	count := 0
	for _, candidate := range candidates {
		if candidate.path == want {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("candidate %q appears %d times, want exactly once: %#v", want, count, candidates)
	}
}

func TestShortTmuxTempDirCandidatesPreferShortUnixBasesToLongTMPDIR(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix fallback ordering does not apply on Windows")
	}

	t.Setenv(tmuxTestTempBaseEnv, "")
	longTempDir := filepath.Join(string(filepath.Separator), strings.Repeat("x", maxTmuxSocketPathBytes))
	t.Setenv("TMPDIR", longTempDir)

	candidates := shortTmuxTempDirCandidates()
	if len(candidates) != 3 {
		t.Fatalf("shortTmuxTempDirCandidates() = %#v, want /var/tmp, /tmp, and long TMPDIR", candidates)
	}
	if candidates[0].path != "/var/tmp" || candidates[1].path != "/tmp" || candidates[2].path != longTempDir {
		t.Fatalf("candidate order = %#v, want /var/tmp, /tmp, then %q", candidates, longTempDir)
	}
	if err := validateTmuxTempBase(candidates[2].path); err == nil {
		t.Fatalf("long TMPDIR candidate %q passed projected socket-path validation", candidates[2].path)
	}
}

func TestValidateTmuxSocketRootRejectsLongUnixPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix-domain socket path limits do not apply on Windows")
	}

	if err := validateTmuxSocketRoot(filepath.Join(string(filepath.Separator), "var", "tmp", "ntm")); err != nil {
		t.Fatalf("short root rejected: %v", err)
	}
	longRoot := filepath.Join(string(filepath.Separator), strings.Repeat("x", maxTmuxSocketPathBytes))
	err := validateTmuxSocketRoot(longRoot)
	if err == nil {
		t.Fatalf("validateTmuxSocketRoot(%q) succeeded, want path-length error", longRoot)
	}
	if !strings.Contains(err.Error(), "projected tmux socket path") ||
		!strings.Contains(err.Error(), "portable limit") {
		t.Fatalf("validateTmuxSocketRoot() error = %q, want actionable path-limit details", err)
	}
}

func TestCreateShortTmuxTempDirReportsAllCandidateFailures(t *testing.T) {
	missingRoot := filepath.Join(t.TempDir(), "missing")
	candidates := []tmuxTempDirCandidate{
		{source: "explicit override", path: filepath.Join(missingRoot, "one")},
		{source: "portable fallback", path: filepath.Join(missingRoot, "two")},
	}

	_, err := createShortTmuxTempDirFromCandidates(candidates)
	if err == nil {
		t.Fatal("createShortTmuxTempDirFromCandidates() succeeded, want aggregate error")
	}
	for _, want := range []string{
		"explicit override",
		candidates[0].path,
		"portable fallback",
		candidates[1].path,
		tmuxTestTempBaseEnv,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("aggregate error %q does not contain %q", err, want)
		}
	}
}

func TestShortTmuxTempDirCreatesAndCleansDirectory(t *testing.T) {
	var dir string
	t.Run("create", func(t *testing.T) {
		dir = ShortTmuxTempDir(t)
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat ShortTmuxTempDir(): %v", err)
		}
		if !info.IsDir() {
			t.Fatalf("ShortTmuxTempDir() = %q, want directory", dir)
		}
		if err := validateTmuxSocketRoot(dir); err != nil {
			t.Fatalf("ShortTmuxTempDir() = %q cannot host a portable tmux socket: %v", dir, err)
		}
	})

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("ShortTmuxTempDir cleanup left %q behind; stat error = %v", dir, err)
	}
}

func TestAgentConfig(t *testing.T) {
	config := AgentConfig{
		Claude: 2,
		Codex:  1,
		Gemini: 1,
	}

	if config.Claude != 2 {
		t.Errorf("expected Claude=2, got %d", config.Claude)
	}
	if config.Codex != 1 {
		t.Errorf("expected Codex=1, got %d", config.Codex)
	}
	if config.Gemini != 1 {
		t.Errorf("expected Gemini=1, got %d", config.Gemini)
	}
}

func TestSkipConditions(t *testing.T) {
	// These are just sanity checks that the skip functions exist and don't panic
	t.Run("RequireUnix", func(t *testing.T) {
		RequireUnix(t)
	})

	t.Run("IntegrationPrecheck", func(t *testing.T) {
		// This will skip because NTM_INTEGRATION_TESTS is not set
		// Just verify it doesn't panic
		if os.Getenv("NTM_INTEGRATION_TESTS") != "" {
			IntegrationTestPrecheck(t)
		}
	})
}
