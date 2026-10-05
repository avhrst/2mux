package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeNativeVersions(t *testing.T, codex, claude string) {
	t.Helper()
	dir := t.TempDir()
	for program, version := range map[string]string{"codex": codex, "claude": claude} {
		if err := os.WriteFile(filepath.Join(dir, program), []byte("#!/bin/sh\nprintf '%s\\n' "+shellQuote(version)+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
}

func TestNativeVersionGate(t *testing.T) {
	for _, tc := range []struct {
		name, codex, claude string
		cfg                 sessionConfig
		wantError           bool
	}{
		{"legacy ignores versions", "unsupported", "unsupported", sessionConfig{}, false},
		{"codex 159", "codex-cli 0.159.2", "unsupported", sessionConfig{CodexAPI: true}, false},
		{"codex 160", "codex-cli 0.160.0", "unsupported", sessionConfig{CodexAPI: true}, false},
		{"both native", "codex-cli 0.160.0", "2.1.289 (Claude Code)", sessionConfig{CodexAPI: true, ClaudeChannel: true}, false},
		{"channel only", "unsupported", "2.1.289 (Claude Code)", sessionConfig{ClaudeChannel: true}, false},
		{"unknown codex", "codex-cli 0.161.0", "2.1.289 (Claude Code)", sessionConfig{CodexAPI: true, ClaudeChannel: true}, true},
		{"nonexact codex", "codex-cli 0.160.0 modified", "unsupported", sessionConfig{CodexAPI: true}, true},
		{"unknown claude", "codex-cli 0.160.0", "2.1.290 (Claude Code)", sessionConfig{CodexAPI: true, ClaudeChannel: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeNativeVersions(t, tc.codex, tc.claude)
			err := checkNativeVersions(tc.cfg)
			if (err != nil) != tc.wantError {
				t.Fatalf("unexpected version result: %v", err)
			}
			if err != nil && !strings.Contains(err.Error(), "no tmux fallback performed") {
				t.Fatalf("error hides effective mode: %v", err)
			}
		})
	}
	t.Run("unavailable executable", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		if err := checkNativeVersions(sessionConfig{CodexAPI: true}); err == nil || !strings.Contains(err.Error(), "no tmux fallback performed") {
			t.Fatalf("unavailable CLI downgraded: %v", err)
		}
	})
}

func TestNativeMismatchPreservesSessionAndAttemptedReceipts(t *testing.T) {
	dir := queueDir(t)
	cfg := sessionConfig{CWD: "/project", CodexAPI: true, CodexThread: "thread", ClaudeChannel: true, ClaudeSession: "session"}
	if err := writeSessionConfig(dir, cfg); err != nil {
		t.Fatal(err)
	}
	m := queued(t, dir, roleWorker, "existing native message")
	m.Transport, m.Status, m.Attempt = "codex", statusSubmitted, 1
	path := filepath.Join(dir, "messages", m.ID+".json")
	if err := writeJSON(path, m); err != nil {
		t.Fatal(err)
	}
	beforeConfig, _ := os.ReadFile(filepath.Join(dir, "session.json"))
	beforeMessage, _ := os.ReadFile(path)
	fakeNativeVersions(t, "codex-cli 0.161.0", "2.1.289 (Claude Code)")
	if err := prepareNative("session", "/project", dir, cfg); err == nil {
		t.Fatal("unsupported native version accepted")
	}
	afterConfig, _ := os.ReadFile(filepath.Join(dir, "session.json"))
	afterMessage, _ := os.ReadFile(path)
	if !bytes.Equal(beforeConfig, afterConfig) || !bytes.Equal(beforeMessage, afterMessage) {
		t.Fatal("version mismatch changed config or attempted receipt")
	}
}

func TestStatusReportsConfiguredTransports(t *testing.T) {
	for _, cfg := range []sessionConfig{{}, {CodexAPI: true}, {ClaudeChannel: true}, {CodexAPI: true, ClaudeChannel: true}} {
		dir := queueDir(t)
		if err := writeSessionConfig(dir, cfg); err != nil {
			t.Fatal(err)
		}
		snapshot, err := statusSnapshot(dir)
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Transports map[string]string `json:"transports"`
		}
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		worker, reviewer := "tmux", "tmux"
		if cfg.CodexAPI {
			worker = "codex"
		}
		if cfg.ClaudeChannel {
			reviewer = "claude-channel"
		}
		if len(got.Transports) != 2 || got.Transports[roleWorker] != worker || got.Transports[roleReviewer] != reviewer {
			t.Fatalf("wrong effective transports: %s", data)
		}
	}
}

func TestNativeVersionFailureDoesNotCreateTmuxSession(t *testing.T) {
	if os.Getenv("TWOMUX_INTEGRATION") != "1" {
		t.Skip("set TWOMUX_INTEGRATION=1 for the isolated native preflight test")
	}
	e := setupIntegration(t)
	// Replace the worker hard link without changing the Claude fixture.
	path := filepath.Join(e.binDir, "unsupported-codex")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' 'codex-cli 0.161.0'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, filepath.Join(e.binDir, "codex")); err != nil {
		t.Fatal(err)
	}
	output, err := e.cli("start", "--codex-api", "--detach")
	if err == nil || !strings.Contains(output, "no tmux fallback performed") {
		t.Fatalf("native preflight silently downgraded: %s: %v", output, err)
	}
	if exists, err := sessionExists(e.name); err != nil || exists {
		t.Fatalf("failed native request created a session: %v, %v", exists, err)
	}
}

func TestNativeConnectPreservesThreadOnResumeErrorsAndIdentityMismatch(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     *rpcError
		resumed *codexThread
	}{
		{name: "backend failure", err: &rpcError{Code: -32603, Message: "backend busy"}},
		{name: "cwd RPC error", err: &rpcError{Code: -32600, Message: "CWD mismatch"}},
		{name: "foreign ID", resumed: &codexThread{ID: "foreign", CWD: "/project"}},
		{name: "foreign CWD", resumed: &codexThread{ID: "thread", CWD: "/other"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := shortNativeQueueDir(t)
			cfg := sessionConfig{CWD: "/project", CodexAPI: true, CodexThread: "thread"}
			if err := writeSessionConfig(dir, cfg); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(filepath.Join(dir, "session.json"))
			f := &fakeThreads{persisted: map[string]bool{"thread": true}, resumeErr: tc.err, resumed: tc.resumed}
			if err := os.Symlink(fakeCodex(t, f.handle), filepath.Join(dir, "codex.sock")); err != nil {
				t.Fatal(err)
			}
			tr := &nativeTransport{dir: dir, cfg: cfg, prompt: rolePromptStub}
			defer tr.close()
			if err := tr.connect(); err == nil {
				t.Fatal("invalid resume accepted")
			}
			after, _ := os.ReadFile(filepath.Join(dir, "session.json"))
			f.mu.Lock()
			defer f.mu.Unlock()
			if !bytes.Equal(before, after) || tr.cfg.CodexThread != "thread" || len(f.started) != 0 || tr.rpc != nil {
				t.Fatal("resume failure changed thread/config or kept invalid connection")
			}
		})
	}
}

func TestNativeConnectFollowsSavedReplacementWhileConnected(t *testing.T) {
	dir := shortNativeQueueDir(t)
	cfg := sessionConfig{CWD: "/project", CodexAPI: true, CodexThread: "first"}
	if err := writeSessionConfig(dir, cfg); err != nil {
		t.Fatal(err)
	}
	f := &fakeThreads{persisted: map[string]bool{"first": true, "replacement": true}}
	if err := os.Symlink(fakeCodex(t, f.handle), filepath.Join(dir, "codex.sock")); err != nil {
		t.Fatal(err)
	}
	tr := &nativeTransport{dir: dir, cfg: cfg, prompt: rolePromptStub}
	defer tr.close()
	if err := tr.connect(); err != nil {
		t.Fatal(err)
	}
	old := tr.rpc
	cfg.CodexThread = "replacement"
	if err := writeSessionConfig(dir, cfg); err != nil {
		t.Fatal(err)
	}
	if err := tr.connect(); err != nil {
		t.Fatal(err)
	}
	if tr.cfg.CodexThread != cfg.CodexThread || tr.rpc == old {
		t.Fatal("bridge kept the old thread connection after saved replacement")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// After a backend restart the bridge's resume restores the role instructions.
	for _, p := range f.resumes {
		if p["developerInstructions"] != "role" {
			t.Fatalf("bridge resumed without role instructions: %v", p)
		}
	}
	if len(f.resumes) != 2 {
		t.Fatalf("resumes: %d", len(f.resumes))
	}
}

func shortNativeQueueDir(t *testing.T) string {
	t.Helper()
	// Unix sockets cannot use the long default macOS TempDir test paths.
	dir, err := os.MkdirTemp("/tmp", "2mux-connect-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.Mkdir(filepath.Join(dir, "messages"), 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestNativeInitializationKeepsSavedThreadReplacement(t *testing.T) {
	dir := queueDir(t)
	stale := sessionConfig{CWD: "/project", CodexAPI: true, CodexThread: "stale"}
	current := stale
	current.CodexThread, current.ClaudeSession, current.BackendPane = "replacement", "reviewer", "%3"
	if err := writeSessionConfig(dir, current); err != nil {
		t.Fatal(err)
	}
	if err := initializeNativeConfig(dir, &stale); err != nil {
		t.Fatal(err)
	}
	saved, err := readSessionConfig(dir)
	if err != nil || saved != current || stale != current {
		t.Fatalf("native setup overwrote a saved replacement: %+v: %v", saved, err)
	}
}
