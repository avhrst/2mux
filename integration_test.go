package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// integration holds one isolated tmux server, project and 2mux binary.
// Phases share it in order, so each phase helper takes its own *testing.T.
type integration struct {
	dir, binDir, binary, project, name, runtime string
	delayedChecks, initializationGate           string
	initializationReached                       string
}

func (e *integration) cliWithTempDir(tempDir string, args ...string) (string, error) {
	cmd := exec.Command(e.binary, args...)
	cmd.Dir = e.project
	if tempDir != "" {
		for _, value := range os.Environ() {
			if !strings.HasPrefix(value, "TMPDIR=") {
				cmd.Env = append(cmd.Env, value)
			}
		}
		cmd.Env = append(cmd.Env, "TMPDIR="+tempDir)
	}
	b, err := cmd.CombinedOutput()
	return string(b), err
}

func (e *integration) cli(args ...string) (string, error) { return e.cliWithTempDir("", args...) }

func (e *integration) mustCLI(t *testing.T, args ...string) string {
	t.Helper()
	b, err := e.cli(args...)
	if err != nil {
		t.Fatalf("2mux %v: %s: %v", args, b, err)
	}
	return b
}

func (e *integration) waitFor(t *testing.T, label string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	status, _ := e.cli("status")
	log, _ := os.ReadFile(filepath.Join(e.runtime, "bridge.log"))
	panes, _ := tmux("list-panes", "-s", "-t", "="+e.name+":", "-F", "#{pane_id} #{pane_current_command} #{pane_tty} #{pane_dead}")
	var captures string
	for _, role := range []string{"worker", "reviewer"} {
		if pane, err := rolePane(e.name, role); err == nil {
			capture, _ := tmux("capture-pane", "-p", "-t", pane)
			tty, _ := tmux("display-message", "-p", "-t", pane, "#{pane_tty}")
			processes, _ := exec.Command("ps", "-ww", "-t", strings.TrimPrefix(tty, "/dev/"), "-o", "pid=,pgid=,tpgid=,stat=,comm=,args=").CombinedOutput()
			captures += role + ": " + capture + "\nprocesses: " + string(processes) + "\n"
		}
	}
	t.Fatalf("timeout waiting for %s\n%s\nbridge log: %s\npanes: %s\ncaptures: %s", label, status, log, panes, captures)
}

func (e *integration) readLog(role string) []string {
	b, _ := os.ReadFile(filepath.Join(e.dir, role+".jsonl"))
	var messages []string
	decoder := json.NewDecoder(strings.NewReader(string(b)))
	for {
		var s string
		if decoder.Decode(&s) != nil {
			break
		}
		messages = append(messages, s)
	}
	return messages
}

func (e *integration) messages() []message {
	messages, _ := readMessages(e.runtime)
	return messages
}

// heldWith waits until the newest record is still queued with an error
// containing want, proving the bridge refused to paste it.
func (e *integration) heldWith(t *testing.T, label string, count int, want string) {
	t.Helper()
	e.waitFor(t, label, func() bool {
		messages := e.messages()
		return len(messages) == count && messages[count-1].Status == "queued" && strings.Contains(messages[count-1].Error, want)
	})
}

func (e *integration) pane(t *testing.T, role string) string {
	t.Helper()
	pane, err := rolePane(e.name, role)
	if err != nil {
		t.Fatal(err)
	}
	return pane
}

func setupIntegration(t *testing.T) *integration {
	realTmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("/tmp", "2mux-integration-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("TMPDIR", dir)
	e := &integration{dir: dir, binDir: filepath.Join(dir, "bin")}
	if err := os.Mkdir(e.binDir, 0700); err != nil {
		t.Fatal(err)
	}
	e.binary = filepath.Join(e.binDir, "2mux")
	build := func(output, source string) {
		t.Helper()
		if b, err := exec.Command(goBinary, "build", "-o", output, source).CombinedOutput(); err != nil {
			t.Fatalf("build: %s: %v", b, err)
		}
	}
	build(e.binary, ".")
	build(filepath.Join(e.binDir, "codex"), "./testdata/agent")
	if err := os.Link(filepath.Join(e.binDir, "codex"), filepath.Join(e.binDir, "claude")); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "socket")
	config := filepath.Join(dir, "tmux.conf")
	e.delayedChecks = filepath.Join(dir, "delay-checks")
	e.initializationGate = filepath.Join(dir, "initialization-gate")
	e.initializationReached = filepath.Join(dir, "initialization-reached")
	if err := os.WriteFile(config, []byte("set -g default-shell /bin/sh\nset -g default-command /bin/sh\n"), 0600); err != nil {
		t.Fatal(err)
	}
	invocation := shellQuote(realTmux) + " -S " + shellQuote(socket) + " -f " + shellQuote(config) + " \"$@\""
	wrapper := "#!/bin/sh\n" +
		"if [ \"$1\" = has-session ] && [ -f " + shellQuote(e.delayedChecks) + " ]; then\n" +
		"  " + invocation + "\n  result=$?\n  sleep 0.2\n  exit \"$result\"\nfi\n" +
		"if [ \"$1\" = split-window ] && [ -f " + shellQuote(e.initializationGate) + " ]; then\n" +
		"  : > " + shellQuote(e.initializationReached) + "\n" +
		"  while [ -f " + shellQuote(e.initializationGate) + " ]; do sleep 0.05; done\nfi\n" +
		"exec " + invocation + "\n"
	if err := os.WriteFile(filepath.Join(e.binDir, "tmux"), []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", e.binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("TWOMUX_FIXTURE_DIR", dir)
	t.Setenv("TWOMUX_BINARY", e.binary)
	project := filepath.Join(dir, "project with 'quotes'")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	e.project, err = filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	e.name = sessionName(e.project)
	// Cleanups run last-in first-out: stop everything before removing the
	// project's lock in the shared namespace, after all commands have ended.
	t.Cleanup(func() { os.Remove(sessionLifecycleLockPath(e.project)) })
	t.Cleanup(func() {
		_, _ = e.cli("stop")
		_, _ = tmux("kill-server")
		if e.runtime != "" {
			os.RemoveAll(e.runtime)
		}
	})
	return e
}

func (e *integration) stopPaneProcess(t *testing.T, pane string) {
	t.Helper()
	value, err := tmux("display-message", "-p", "-t", pane, "#{pane_pid}")
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	e.waitFor(t, "pane process exited", func() bool {
		dead, err := tmux("display-message", "-p", "-t", pane, "#{pane_dead}")
		return err == nil && dead == "1"
	})
}

func TestNativeCodexFirstLaunchAndRestart(t *testing.T) {
	if os.Getenv("TWOMUX_INTEGRATION") != "1" {
		t.Skip("set TWOMUX_INTEGRATION=1 for native thread lifecycle integration")
	}
	e := setupIntegration(t)
	e.mustCLI(t, "start", "--codex-api", "--detach")
	var err error
	e.runtime, err = runtimeDirectory(e.name)
	if err != nil {
		t.Fatal(err)
	}
	waitWorker := func() {
		t.Helper()
		e.waitFor(t, "native worker resumed", func() bool {
			_, err := os.Stat(filepath.Join(e.dir, "worker.ready"))
			return err == nil
		})
	}
	config := func() sessionConfig {
		t.Helper()
		cfg, err := readSessionConfig(e.runtime)
		if err != nil || cfg.CodexThread == "" {
			t.Fatalf("no saved native thread: %+v: %v", cfg, err)
		}
		return cfg
	}
	killWorker := func() {
		t.Helper()
		e.stopPaneProcess(t, e.pane(t, roleWorker))
		if err := os.Remove(filepath.Join(e.dir, "worker.ready")); err != nil {
			t.Fatal(err)
		}
	}
	waitWorker()
	first := config()
	stored, err := os.ReadFile(filepath.Join(e.dir, "thread-"+first.CodexThread+".json"))
	if err != nil || !strings.Contains(string(stored), `"turns":[]`) {
		t.Fatalf("bootstrap did not persist an empty thread: %s: %v", stored, err)
	}
	// The fixture must reproduce the original failure, not let thread/start
	// alone survive a closed creating connection.
	ctx, cancel := rpcTimeout()
	r, err := dialCodex(ctx, filepath.Join(e.runtime, "codex.sock"))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var unsaved struct{ Thread codexThread }
	err = r.call(ctx, "thread/start", map[string]any{"cwd": e.project}, &unsaved)
	r.close()
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	e.waitFor(t, "fixture discards unpersisted thread", func() bool {
		ctx, cancel := rpcTimeout()
		defer cancel()
		r, err := dialCodex(ctx, filepath.Join(e.runtime, "codex.sock"))
		if err != nil {
			return false
		}
		defer r.close()
		return missingRollout(r.call(ctx, "thread/resume", map[string]string{"threadId": unsaved.Thread.ID}, nil))
	})
	// Restart both worker and backend before the first turn. The thread ID
	// must stay the same, proving durable persistence rather than a live owner.
	killWorker()
	e.stopPaneProcess(t, first.BackendPane)
	e.mustCLI(t, "respawn", roleWorker)
	waitWorker()
	after := config()
	if after.CodexThread != first.CodexThread || after.BackendPane == first.BackendPane {
		t.Fatalf("empty thread was replaced or backend not restarted: %+v -> %+v", first, after)
	}
	// Stop the background bridge so the test owns missing-rollout recovery.
	if err := stopBridge(e.runtime); err != nil {
		t.Fatal(err)
	}
	m := queued(t, e.runtime, roleWorker, "unresolved old thread receipt")
	m.Transport, m.Status, m.Attempt = "codex", statusSubmitted, 1
	if err := writeJSON(filepath.Join(e.runtime, "messages", m.ID+".json"), m); err != nil {
		t.Fatal(err)
	}
	after.CodexThread = "missing-bridge-rollout"
	if err := writeSessionConfig(e.runtime, after); err != nil {
		t.Fatal(err)
	}
	transport := &nativeTransport{dir: e.runtime, cfg: after}
	if err := transport.connect(); err != nil {
		t.Fatal(err)
	}
	transport.close()
	recovered := config()
	if recovered.CodexThread == after.CodexThread || recovered.CWD != after.CWD || recovered.ClaudeSession != after.ClaudeSession || recovered.BackendPane != after.BackendPane {
		t.Fatalf("bridge did not save scoped replacement: %+v", recovered)
	}
	messages := e.messages()
	if len(messages) != 1 || messages[0].Status != statusUncertain || messages[0].Attempt != 1 {
		t.Fatalf("lost-thread receipt replayed or misreported: %+v", messages)
	}
	// The bootstrap path also repairs stale saved IDs before launching a TUI.
	killWorker()
	recovered.CodexThread = "missing-bootstrap-rollout"
	if err := writeSessionConfig(e.runtime, recovered); err != nil {
		t.Fatal(err)
	}
	e.mustCLI(t, "respawn", roleWorker)
	waitWorker()
	if got := config(); got.CodexThread == recovered.CodexThread {
		t.Fatal("bootstrap kept the missing rollout ID")
	}
}

func TestTmuxAutomaticReviewCycle(t *testing.T) {
	if os.Getenv("TWOMUX_INTEGRATION") != "1" {
		t.Skip("set TWOMUX_INTEGRATION=1 to run the isolated tmux integration test")
	}
	e := setupIntegration(t)
	var startTempDirs []string
	for i := 0; i < 2; i++ {
		tempDir := filepath.Join(e.dir, fmt.Sprintf("caller-tmp-%d", i))
		if err := os.Mkdir(tempDir, 0700); err != nil {
			t.Fatal(err)
		}
		startTempDirs = append(startTempDirs, tempDir)
	}
	// Later phases depend on the state earlier ones leave behind.
	phases := []struct {
		name string
		run  func(*testing.T)
	}{
		{"ConcurrentFirstStarts", func(t *testing.T) {
			// Concurrent first starts must see one completely initialized session.
			// Delay has-session results so the original TMPDIR-dependent lock exposes
			// two absent-session checks, rather than relying on scheduler timing.
			if err := os.WriteFile(e.delayedChecks, nil, 0600); err != nil {
				t.Fatal(err)
			}
			var starts sync.WaitGroup
			for i := 0; i < 6; i++ {
				tempDir := startTempDirs[i%len(startTempDirs)]
				starts.Add(1)
				go func() {
					defer starts.Done()
					if b, err := e.cliWithTempDir(tempDir, "start", "--detach"); err != nil {
						t.Errorf("concurrent first start: %s: %v", b, err)
					}
				}()
			}
			starts.Wait()
			if err := os.Remove(e.delayedChecks); err != nil {
				t.Fatal(err)
			}
			var err error
			e.runtime, err = runtimeDirectory(e.name)
			if err != nil {
				t.Fatal(err)
			}
			e.pane(t, "worker")
			e.pane(t, "reviewer")
			panes, err := listSessionPanes(e.name)
			if err != nil || len(panes) != 2 {
				t.Fatalf("concurrent starts created unexpected panes: %+v: %v", panes, err)
			}
			e.mustCLI(t, "stop")
			if err := os.RemoveAll(e.runtime); err != nil {
				t.Fatal(err)
			}
			e.runtime = ""
		}},
		{"StopWaitsForInitialization", func(t *testing.T) {
			// Hold initialization after the ownership marker but before the second
			// pane/runtime metadata exists. A concurrent stop must wait for start.
			if err := os.WriteFile(e.initializationGate, nil, 0600); err != nil {
				t.Fatal(err)
			}
			defer os.Remove(e.initializationGate)
			type cliResult struct {
				output string
				err    error
			}
			starting := make(chan cliResult, 1)
			go func() {
				b, err := e.cliWithTempDir(startTempDirs[0], "start", "--detach")
				starting <- cliResult{b, err}
			}()
			e.waitFor(t, "paused initialization", func() bool {
				_, err := os.Stat(e.initializationReached)
				return err == nil
			})
			// The runtime is already created on disk even though its tmux marker is
			// not yet published. Find it within this caller's isolated temporary tree.
			candidates, err := filepath.Glob(filepath.Join(startTempDirs[0], "2mux-*"))
			if err != nil {
				t.Fatal(err)
			}
			for _, candidate := range candidates {
				if info, err := os.Stat(filepath.Join(candidate, "messages")); err == nil && info.IsDir() {
					if e.runtime != "" {
						t.Fatal("multiple runtimes for paused initialization")
					}
					e.runtime = candidate
				}
			}
			if e.runtime == "" {
				t.Fatal("paused initialization has no runtime")
			}
			stopping := make(chan cliResult, 1)
			go func() {
				b, err := e.cliWithTempDir(startTempDirs[1], "stop")
				stopping <- cliResult{b, err}
			}()
			var earlyStop *cliResult
			select {
			case result := <-stopping:
				earlyStop = &result
			case <-time.After(200 * time.Millisecond):
			}
			if err := os.Remove(e.initializationGate); err != nil {
				t.Fatal(err)
			}
			startResult := <-starting
			if startResult.err != nil {
				t.Fatalf("paused start: %s: %v", startResult.output, startResult.err)
			}
			var stopResult cliResult
			if earlyStop != nil {
				stopResult = *earlyStop
				t.Error("stop returned before session initialization was released")
			} else {
				stopResult = <-stopping
			}
			if stopResult.err != nil {
				t.Fatalf("waiting stop: %s: %v", stopResult.output, stopResult.err)
			}
			if exists, err := sessionExists(e.name); err != nil || exists {
				t.Fatalf("waiting stop did not remove initialized session: %v, %v", exists, err)
			}
			if err := os.RemoveAll(e.runtime); err != nil {
				t.Fatal(err)
			}
			e.runtime = ""
		}},
		{"ReviewCycle", func(t *testing.T) {
			var starts sync.WaitGroup
			for i := 0; i < 6; i++ {
				starts.Add(1)
				go func() {
					defer starts.Done()
					if b, err := e.cli("start", "--agents", "--detach"); err != nil {
						t.Errorf("concurrent agent start: %s: %v", b, err)
					}
				}()
			}
			starts.Wait()
			var err error
			e.runtime, err = runtimeDirectory(e.name)
			if err != nil {
				t.Fatal(err)
			}
			e.waitFor(t, "agents", func() bool {
				_, w := os.Stat(filepath.Join(e.dir, "worker.ready"))
				_, r := os.Stat(filepath.Join(e.dir, "reviewer.ready"))
				return w == nil && r == nil
			})
			worker, reviewer := e.pane(t, "worker"), e.pane(t, "reviewer")
			for role, pane := range map[string]string{roleWorker: worker, roleReviewer: reviewer} {
				label, err := tmux("show-option", "-p", "-v", "-t", pane, "@twomux_role")
				if err != nil || label != role {
					t.Fatalf("wrong role in pane header: %q: %v", label, err)
				}
				prompt := e.mustCLI(t, "prompt", role)
				if !strings.Contains(prompt, communicationUIInstruction) {
					t.Fatal("role prompt does not make peer communication visible")
				}
			}
			// Move roles and change the active window before any delivery.
			if _, err := tmux("swap-pane", "-s", worker, "-t", reviewer); err != nil {
				t.Fatal(err)
			}
			if _, err := tmux("new-window", "-t", "="+e.name+":", "-c", e.project); err != nil {
				t.Fatal(err)
			}
			e.mustCLI(t, "send", "worker", "TASK: implement and request review")
			e.waitFor(t, "approval", func() bool {
				messages := e.readLog("worker")
				return len(messages) == 3 && strings.Contains(messages[2], "APPROVED")
			})
			e.waitFor(t, "delivery receipts", func() bool {
				messages := e.messages()
				if len(messages) != 5 {
					return false
				}
				for _, m := range messages {
					if m.Status != "delivered" {
						return false
					}
				}
				return true
			})
			feedback := e.readLog("worker")[1]
			if !strings.HasPrefix(feedback, "╭─ 2mux · REVIEWER → WORKER") {
				t.Fatalf("incoming feedback has no visible role card: %q", feedback)
			}
			if !strings.Contains(feedback, "CORRECTIONS:\nFix the retry case.\nПеревір UTF-8.\n[2mux end of message ") {
				t.Fatalf("multiline feedback corrupted: %q", feedback)
			}
		}},
		{"LegacyHooksOnlyObserve", func(t *testing.T) {
			transport := &nativeTransport{dir: e.runtime, panes: paneWatch{}}
			defer os.Remove(filepath.Join(e.runtime, roleReviewer+"-state.json"))
			for _, state := range []agentState{
				{Role: roleReviewer, State: "starting", Source: "claude-hooks", Updated: time.Now()},
				{Role: roleReviewer, State: "busy", Source: "claude-hooks", Updated: time.Now().Add(-time.Hour)},
				{Role: roleReviewer, State: "awaiting_approval", Source: "claude-hooks", Updated: time.Now().Add(-hookApprovalTTL - time.Second)},
			} {
				if err := writeJSON(filepath.Join(e.runtime, roleReviewer+"-state.json"), state); err != nil {
					t.Fatal(err)
				}
				e.waitFor(t, "legacy readiness despite observing hook "+state.State, func() bool { return transport.ready(e.name, message{To: roleReviewer, Transport: "tmux"}) == nil })
			}
			writeAgentState(e.runtime, agentState{Role: roleReviewer, State: "awaiting_approval", Source: "claude-hooks"})
			if err := transport.ready(e.name, message{To: roleReviewer, Transport: "tmux"}); err == nil || !strings.Contains(err.Error(), "awaiting_approval") {
				t.Fatal("fresh approval observation did not hold delivery", err)
			}
		}},
		{"BridgeRestartDoesNotReplay", func(t *testing.T) {
			// Killing and concurrently restarting the detached bridge must not replay.
			h, err := readHealth(e.runtime)
			if err != nil {
				t.Fatal(err)
			}
			if err := syscall.Kill(h.PID, syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
			e.waitFor(t, "released bridge lock", func() bool { running, _ := bridgeRunning(e.runtime); return !running })
			// An agent's explicit-queue send must warn when nothing will deliver it.
			cmd := exec.Command(e.binary, "send", "--queue", e.runtime, "--from", "worker", "reviewer", "BRIDGEDOWN: queued while the bridge is stopped")
			if b, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(b), "Warning: 2mux bridge is stopped") {
				t.Fatalf("send without bridge gave no warning: %s: %v", b, err)
			}
			var restarts sync.WaitGroup
			for i := 0; i < 3; i++ {
				restarts.Add(1)
				go func() {
					defer restarts.Done()
					if b, err := e.cli("start", "--detach"); err != nil {
						t.Errorf("restart: %s: %v", b, err)
					}
				}()
			}
			restarts.Wait()
			e.waitFor(t, "queued message after restart", func() bool { return len(e.readLog("reviewer")) == 3 })
			time.Sleep(700 * time.Millisecond)
			if len(e.readLog("worker")) != 3 || len(e.readLog("reviewer")) != 3 {
				t.Fatal("bridge restart replayed messages")
			}
			e.mustCLI(t, "start", "--agents", "--detach")
		}},
		{"DeliveryGuards", func(t *testing.T) {
			worker, reviewer := e.pane(t, "worker"), e.pane(t, "reviewer")
			// Copy mode and disabled input hold messages until the pane can receive.
			if _, err := tmux("copy-mode", "-t", reviewer); err != nil {
				t.Fatal(err)
			}
			e.mustCLI(t, "send", "reviewer", "COPYMODE: hold until copy mode ends")
			e.heldWith(t, "copy mode holds message", 7, "copy mode")
			if len(e.readLog("reviewer")) != 3 {
				t.Fatal("copy mode consumed the message")
			}
			if _, err := tmux("send-keys", "-X", "-t", reviewer, "cancel"); err != nil {
				t.Fatal(err)
			}
			e.waitFor(t, "delivery after copy mode", func() bool { return len(e.readLog("reviewer")) == 4 })
			if _, err := tmux("select-pane", "-d", "-t", worker); err != nil {
				t.Fatal(err)
			}
			e.mustCLI(t, "send", "worker", "INPUT: hold while input is disabled")
			e.heldWith(t, "disabled input holds message", 8, "input disabled")
			if len(e.readLog("worker")) != 3 {
				t.Fatal("disabled pane consumed the message")
			}
			if _, err := tmux("select-pane", "-e", "-t", worker); err != nil {
				t.Fatal(err)
			}
			e.waitFor(t, "delivery after input enabled", func() bool { return len(e.readLog("worker")) == 4 })
			// A visible approval prompt must not receive the paste and Enter.
			e.mustCLI(t, "send", "reviewer", "DIALOG: show a confirmation prompt")
			e.waitFor(t, "dialog shown", func() bool { return len(e.readLog("reviewer")) == 5 })
			e.mustCLI(t, "send", "reviewer", "HELD: wait until the dialog is gone")
			e.heldWith(t, "dialog holds message", 10, "confirmation dialog")
			time.Sleep(700 * time.Millisecond)
			if len(e.readLog("reviewer")) != 5 {
				t.Fatal("message was submitted into a confirmation dialog")
			}
			// Reset the emulated screen without sending input to the fixture.
			if _, err := tmux("send-keys", "-R", "-t", reviewer); err != nil {
				t.Fatal(err)
			}
			if _, err := tmux("clear-history", "-t", reviewer); err != nil {
				t.Fatal(err)
			}
			e.waitFor(t, "delivery after dialog", func() bool { return len(e.readLog("reviewer")) == 6 })
			// After an agent exits, its shell must not receive the queued text.
			if _, err := tmux("respawn-pane", "-k", "-t", reviewer, "/bin/sh"); err != nil {
				t.Fatal(err)
			}
			e.mustCLI(t, "send", "reviewer", "MANUAL: literal `echo x` $(echo y)\nдругий рядок")
			e.heldWith(t, "queued while agent absent", 11, "waiting for an interactive")
			text, err := tmux("capture-pane", "-p", "-t", reviewer)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(text, "MANUAL:") {
				t.Fatal("message was pasted into a shell")
			}
			if _, err := tmux("respawn-pane", "-k", "-t", reviewer, filepath.Join(e.binDir, "claude"), "fixture"); err != nil {
				t.Fatal(err)
			}
			e.waitFor(t, "queued message delivered after restart", func() bool {
				messages := e.readLog("reviewer")
				return len(messages) == 7 && strings.Contains(messages[6], "MANUAL:")
			})
			status := e.mustCLI(t, "status")
			if !strings.Contains(status, "Bridge: running") || !strings.Contains(status, "worker pane: "+worker) || !strings.Contains(status, "reviewer pane: "+reviewer) {
				t.Fatal(status)
			}
		}},
		{"ExitedAgentRespawn", func(t *testing.T) {
			reviewer := e.pane(t, "reviewer")
			pid, err := tmux("display-message", "-p", "-t", reviewer, "#{pane_pid}")
			if err != nil {
				t.Fatal(err)
			}
			n, err := strconv.Atoi(pid)
			if err != nil {
				t.Fatal(err)
			}
			if err := syscall.Kill(n, syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			e.waitFor(t, "dead reviewer pane", func() bool {
				_, exists, alive, err := registeredPane(e.name, "reviewer")
				return err == nil && exists && !alive
			})
			// A dead pane warns but no longer prevents reattachment.
			if b, err := e.cli("start", "--detach"); err != nil || !strings.Contains(b, "Warning: reviewer pane") {
				t.Fatalf("start with a dead pane: %s: %v", b, err)
			}
			cmd := exec.Command(e.binary, "send", "--queue", e.runtime, "--from", "worker", "reviewer", "RESPAWN: delivered after respawn")
			if b, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("send to exited reviewer: %s: %v", b, err)
			}
			e.heldWith(t, "queued while reviewer exited", 12, "missing or dead")
			if b, err := e.cli("respawn", "worker"); err == nil || !strings.Contains(b, "still running") {
				t.Fatalf("respawn replaced a live pane: %s: %v", b, err)
			}
			e.mustCLI(t, "respawn", "reviewer")
			if e.pane(t, "reviewer") != reviewer {
				t.Fatal("respawn of a dead pane changed its registration")
			}
			e.waitFor(t, "delivery after respawn", func() bool {
				messages := e.readLog("reviewer")
				return len(messages) == 8 && strings.Contains(messages[7], "RESPAWN:")
			})
		}},
		{"RolesAreNotInherited", func(t *testing.T) {
			worker, reviewer := e.pane(t, "worker"), e.pane(t, "reviewer")
			// New panes do not silently inherit an old role.
			if _, err := tmux("kill-pane", "-t", reviewer); err != nil {
				t.Fatal(err)
			}
			stray, err := tmux("split-window", "-h", "-P", "-F", "#{pane_id}", "-t", worker, "-c", e.project)
			if err != nil {
				t.Fatal(err)
			}
			if b, err := e.cli("send", "reviewer", "must fail"); err == nil || !strings.Contains(b, "missing or dead") {
				t.Fatalf("missing role accepted: %s: %v", b, err)
			}
			// An explicit respawn registers a new pane, never the stray one.
			e.mustCLI(t, "respawn", "reviewer")
			replacement := e.pane(t, "reviewer")
			if replacement == reviewer || replacement == stray {
				t.Fatalf("unexpected replacement reviewer pane %s", replacement)
			}
			label, err := tmux("show-option", "-p", "-v", "-t", replacement, "@twomux_role")
			if err != nil || label != roleReviewer {
				t.Fatalf("respawn did not restore the reviewer header: %q: %v", label, err)
			}
			strayHeader, err := tmux("display-message", "-p", "-t", stray, "#{E:pane-border-format}")
			if err != nil || strings.Contains(strayHeader, "REVIEWER · Claude") {
				t.Fatalf("stray pane inherited a reviewer header: %q: %v", strayHeader, err)
			}
		}},
		{"StopAndOwnership", func(t *testing.T) {
			e.mustCLI(t, "stop")
			e.waitFor(t, "bridge stop", func() bool { running, _ := bridgeRunning(e.runtime); return !running })
			if exists, err := sessionExists(e.name); err != nil || exists {
				t.Fatal(exists, err)
			}
			// A conflicting unowned session is never killed by 2mux stop.
			if _, err := tmux("new-session", "-d", "-s", e.name, "-c", e.project); err != nil {
				t.Fatal(err)
			}
			if b, err := e.cli("stop"); err == nil || !strings.Contains(b, "not owned") {
				t.Fatalf("unowned session accepted: %s: %v", b, err)
			}
			if exists, _ := sessionExists(e.name); !exists {
				t.Fatal("foreign session was removed")
			}
		}},
	}
	for _, phase := range phases {
		if !t.Run(phase.name, phase.run) {
			return
		}
	}
}
