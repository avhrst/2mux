package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This opt-in check uses the installed Codex, an isolated home, and no model
// turns, credentials, or approval-policy overrides.
func TestRealCodexEmptyThreadSurvivesBootstrapDisconnectAndBackendRestart(t *testing.T) {
	if os.Getenv("TWOMUX_CODEX_LIVE") != "1" {
		t.Skip("set TWOMUX_CODEX_LIVE=1 to check persistence against installed Codex")
	}
	path, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	version, err := exec.Command(path, "--version").Output()
	if err != nil || !supportedCodexVersion(strings.TrimSpace(string(version))) {
		t.Fatalf("unsupported live Codex: %s: %v", version, err)
	}
	t.Log(strings.TrimSpace(string(version)))
	dir, err := os.MkdirTemp("/tmp", "2mux-codex-live-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("CODEX_HOME", dir)
	cwd, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "socket")
	start := func() func() {
		t.Helper()
		log, err := os.Create(filepath.Join(dir, "backend.log"))
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(path, "app-server", "--listen", "unix://"+socket)
		cmd.Dir, cmd.Stdout, cmd.Stderr = cwd, log, log
		if err := cmd.Start(); err != nil {
			log.Close()
			t.Fatal(err)
		}
		stopped := false
		stop := func() {
			if !stopped {
				stopped = true
				cmd.Process.Kill()
				cmd.Wait()
				log.Close()
			}
		}
		t.Cleanup(stop)
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			ctx, cancel := rpcTimeout()
			r, err := dialCodex(ctx, socket)
			cancel()
			if err == nil {
				r.close()
				return stop
			}
			time.Sleep(50 * time.Millisecond)
		}
		data, _ := os.ReadFile(filepath.Join(dir, "backend.log"))
		t.Fatalf("Codex did not listen: %s", data)
		return stop
	}
	stop := start()
	ctx, cancel := rpcTimeout()
	defer cancel()
	r, err := dialCodex(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	var unsaved struct{ Thread codexThread }
	if err := r.call(ctx, "thread/start", map[string]string{"cwd": cwd}, &unsaved); err != nil {
		r.close()
		t.Fatal(err)
	}
	r.close()
	r, err = dialCodex(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.call(ctx, "thread/resume", map[string]string{"threadId": unsaved.Thread.ID}, nil); !missingRollout(err) {
		r.close()
		t.Fatalf("unpersisted thread did not reproduce the missing-rollout error: %v", err)
	}
	t.Log("thread/start alone reproduced no rollout found after creating connection closed")
	cfg := sessionConfig{CWD: cwd, CodexAPI: true}
	thread, err := ensureWorkerThread(ctx, r, dir, &cfg, rolePromptStub)
	r.close()
	if err != nil {
		t.Fatal(err)
	}
	id := thread.ID
	// A distinct connection exercises the same resume the remote TUI performs.
	resume := func() {
		t.Helper()
		ctx, cancel := rpcTimeout()
		defer cancel()
		r, err := dialCodex(ctx, socket)
		if err != nil {
			t.Fatal(err)
		}
		defer r.close()
		var got struct {
			Thread struct {
				ID, CWD string
				Turns   []any
			}
		}
		if err := r.call(ctx, "thread/resume", map[string]string{"threadId": id}, &got); err != nil {
			t.Fatal(err)
		}
		if got.Thread.ID != id || got.Thread.CWD != cwd || len(got.Thread.Turns) != 0 {
			t.Fatalf("incorrect resumed empty thread: %+v", got)
		}
	}
	resume()
	stop()
	start()
	resume()
	t.Log("empty thread resumed after bootstrap disconnect and backend restart; no model turns")
}

func TestRealCodexNativeTUIStartsAndRespawnsEmptyThread(t *testing.T) {
	if os.Getenv("TWOMUX_CODEX_LIVE") != "1" {
		t.Skip("set TWOMUX_CODEX_LIVE=1 for the installed native TUI startup check")
	}
	path, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	e := setupIntegration(t)
	// Keep the deterministic reviewer; replace only the worker/backend CLI.
	fixture := filepath.Join(e.binDir, "codex")
	if err := os.Remove(fixture); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, fixture); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(e.dir, "codex-home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	// Trust only this disposable project, keeping approval and sandbox defaults.
	config := fmt.Sprintf("[projects.%q]\ntrust_level = \"trusted\"\n", e.project)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	// A nonfunctional test credential skips the login wizard without reading
	// the user's credentials. This test never submits a prompt or model turn.
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"OPENAI_API_KEY":"2mux-unused-no-inference-fixture"}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", home)
	e.mustCLI(t, "start", "--codex-api", "--detach")
	e.runtime, err = runtimeDirectory(e.name)
	if err != nil {
		t.Fatal(err)
	}
	waitPrompt := func() {
		t.Helper()
		e.waitFor(t, "real native Codex TUI prompt", func() bool {
			worker := e.pane(t, roleWorker)
			screen, err := tmux("capture-pane", "-p", "-J", "-t", worker)
			return err == nil && strings.Contains(screen, "OpenAI Codex") && codexScreenState(screen) == "idle" && !strings.Contains(screen, "thread/resume failed")
		})
	}
	waitPrompt()
	first, err := readSessionConfig(e.runtime)
	if err != nil {
		t.Fatal(err)
	}
	e.stopPaneProcess(t, e.pane(t, roleWorker))
	e.stopPaneProcess(t, first.BackendPane)
	e.mustCLI(t, "respawn", roleWorker)
	waitPrompt()
	after, err := readSessionConfig(e.runtime)
	if err != nil || after.CodexThread != first.CodexThread {
		t.Fatalf("real TUI did not keep its empty thread across restart: %+v: %v", after, err)
	}
	ctx, cancel := rpcTimeout()
	defer cancel()
	r, err := dialCodex(ctx, filepath.Join(e.runtime, "codex.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	var resumed struct {
		Thread struct {
			ID    string
			Turns []any
		}
	}
	if err := r.call(ctx, "thread/resume", map[string]string{"threadId": after.CodexThread}, &resumed); err != nil || resumed.Thread.ID != first.CodexThread || len(resumed.Thread.Turns) != 0 {
		t.Fatalf("TUI startup changed thread or ran a turn: %+v: %v", resumed, err)
	}
	t.Log("installed native Codex TUI reached its prompt on first launch and respawn after backend restart; no model turns")
}
