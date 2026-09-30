package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestTmuxAutomaticReviewCycle(t *testing.T) {
	if os.Getenv("TWOMUX_INTEGRATION") != "1" {
		t.Skip("set TWOMUX_INTEGRATION=1 to run the isolated tmux integration test")
	}
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
	defer os.RemoveAll(dir)
	t.Setenv("TMPDIR", dir)
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(binDir, "2mux")
	build := func(output, source string) {
		t.Helper()
		if b, err := exec.Command(goBinary, "build", "-o", output, source).CombinedOutput(); err != nil {
			t.Fatalf("build: %s: %v", b, err)
		}
	}
	build(binary, ".")
	build(filepath.Join(binDir, "codex"), "./testdata/agent")
	if err := os.Link(filepath.Join(binDir, "codex"), filepath.Join(binDir, "pi")); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "socket")
	config := filepath.Join(dir, "tmux.conf")
	delayedChecks := filepath.Join(dir, "delay-checks")
	initializationGate := filepath.Join(dir, "initialization-gate")
	initializationReached := filepath.Join(dir, "initialization-reached")
	if err := os.WriteFile(config, []byte("set -g default-shell /bin/sh\nset -g default-command /bin/sh\n"), 0600); err != nil {
		t.Fatal(err)
	}
	invocation := shellQuote(realTmux) + " -S " + shellQuote(socket) + " -f " + shellQuote(config) + " \"$@\""
	wrapper := "#!/bin/sh\n" +
		"if [ \"$1\" = has-session ] && [ -f " + shellQuote(delayedChecks) + " ]; then\n" +
		"  " + invocation + "\n  result=$?\n  sleep 0.2\n  exit \"$result\"\nfi\n" +
		"if [ \"$1\" = split-window ] && [ -f " + shellQuote(initializationGate) + " ]; then\n" +
		"  : > " + shellQuote(initializationReached) + "\n" +
		"  while [ -f " + shellQuote(initializationGate) + " ]; do sleep 0.05; done\nfi\n" +
		"exec " + invocation + "\n"
	if err := os.WriteFile(filepath.Join(binDir, "tmux"), []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("TWOMUX_FIXTURE_DIR", dir)
	t.Setenv("TWOMUX_BINARY", binary)
	project := filepath.Join(dir, "project with 'quotes'")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	project, err = filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	// These files are keyed by a unique test project in the shared namespace.
	// Remove only this project's lock, after all start/stop commands have ended.
	defer os.Remove(sessionLifecycleLockPath(project))
	name := sessionName(project)
	cliWithTempDir := func(tempDir string, args ...string) (string, error) {
		cmd := exec.Command(binary, args...)
		cmd.Dir = project
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
	cli := func(args ...string) (string, error) { return cliWithTempDir("", args...) }
	mustCLI := func(args ...string) string {
		t.Helper()
		b, err := cli(args...)
		if err != nil {
			t.Fatalf("2mux %v: %s: %v", args, b, err)
		}
		return b
	}
	var runtime string
	defer func() {
		_, _ = cli("stop")
		_, _ = tmux("kill-server")
		if runtime != "" {
			os.RemoveAll(runtime)
		}
	}()
	waitFor := func(label string, check func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if check() {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		status, _ := cli("status")
		log, _ := os.ReadFile(filepath.Join(runtime, "bridge.log"))
		panes, _ := tmux("list-panes", "-s", "-t", "="+name+":", "-F", "#{pane_id} #{pane_current_command} #{pane_tty} #{pane_dead}")
		var captures string
		for _, role := range []string{"worker", "reviewer"} {
			if pane, err := rolePane(name, role); err == nil {
				capture, _ := tmux("capture-pane", "-p", "-t", pane)
				tty, _ := tmux("display-message", "-p", "-t", pane, "#{pane_tty}")
				processes, _ := exec.Command("ps", "-ww", "-t", strings.TrimPrefix(tty, "/dev/"), "-o", "pid=,pgid=,tpgid=,stat=,comm=,args=").CombinedOutput()
				captures += role + ": " + capture + "\nprocesses: " + string(processes) + "\n"
			}
		}
		t.Fatalf("timeout waiting for %s\n%s\nbridge log: %s\npanes: %s\ncaptures: %s", label, status, log, panes, captures)
	}
	readLog := func(role string) []string {
		b, _ := os.ReadFile(filepath.Join(dir, role+".jsonl"))
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
	// Concurrent first starts must see one completely initialized session.
	// Delay has-session results so the original TMPDIR-dependent lock exposes
	// two absent-session checks, rather than relying on scheduler timing.
	if err := os.WriteFile(delayedChecks, nil, 0600); err != nil {
		t.Fatal(err)
	}
	var startTempDirs []string
	for i := 0; i < 2; i++ {
		tempDir := filepath.Join(dir, fmt.Sprintf("caller-tmp-%d", i))
		if err := os.Mkdir(tempDir, 0700); err != nil {
			t.Fatal(err)
		}
		startTempDirs = append(startTempDirs, tempDir)
	}
	var initialStarts sync.WaitGroup
	for i := 0; i < 6; i++ {
		tempDir := startTempDirs[i%len(startTempDirs)]
		initialStarts.Add(1)
		go func() {
			defer initialStarts.Done()
			if b, err := cliWithTempDir(tempDir, "start", "--detach"); err != nil {
				t.Errorf("concurrent first start: %s: %v", b, err)
			}
		}()
	}
	initialStarts.Wait()
	if err := os.Remove(delayedChecks); err != nil {
		t.Fatal(err)
	}
	runtime, err = runtimeDirectory(name)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"worker", "reviewer"} {
		if _, err := rolePane(name, role); err != nil {
			t.Fatal(err)
		}
	}
	panes, err := listSessionPanes(name)
	if err != nil || len(panes) != 2 {
		t.Fatalf("concurrent starts created unexpected panes: %+v: %v", panes, err)
	}
	mustCLI("stop")
	if err := os.RemoveAll(runtime); err != nil {
		t.Fatal(err)
	}
	runtime = ""

	// Hold initialization after the ownership marker but before the second
	// pane/runtime metadata exists. A concurrent stop must wait for start.
	if err := os.WriteFile(initializationGate, nil, 0600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(initializationGate)
	type cliResult struct {
		output string
		err    error
	}
	starting := make(chan cliResult, 1)
	go func() {
		b, err := cliWithTempDir(startTempDirs[0], "start", "--detach")
		starting <- cliResult{b, err}
	}()
	waitFor("paused initialization", func() bool {
		_, err := os.Stat(initializationReached)
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
			if runtime != "" {
				t.Fatal("multiple runtimes for paused initialization")
			}
			runtime = candidate
		}
	}
	if runtime == "" {
		t.Fatal("paused initialization has no runtime")
	}
	stopping := make(chan cliResult, 1)
	go func() {
		b, err := cliWithTempDir(startTempDirs[1], "stop")
		stopping <- cliResult{b, err}
	}()
	var earlyStop *cliResult
	select {
	case result := <-stopping:
		earlyStop = &result
	case <-time.After(200 * time.Millisecond):
	}
	if err := os.Remove(initializationGate); err != nil {
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
	if exists, err := sessionExists(name); err != nil || exists {
		t.Fatalf("waiting stop did not remove initialized session: %v, %v", exists, err)
	}
	if err := os.RemoveAll(runtime); err != nil {
		t.Fatal(err)
	}
	runtime = ""

	var agentStarts sync.WaitGroup
	for i := 0; i < 6; i++ {
		agentStarts.Add(1)
		go func() {
			defer agentStarts.Done()
			if b, err := cli("start", "--agents", "--detach"); err != nil {
				t.Errorf("concurrent agent start: %s: %v", b, err)
			}
		}()
	}
	agentStarts.Wait()
	runtime, err = runtimeDirectory(name)
	if err != nil {
		t.Fatal(err)
	}
	waitFor("agents", func() bool {
		_, w := os.Stat(filepath.Join(dir, "worker.ready"))
		_, r := os.Stat(filepath.Join(dir, "reviewer.ready"))
		return w == nil && r == nil
	})
	worker, err := rolePane(name, "worker")
	if err != nil {
		t.Fatal(err)
	}
	reviewer, err := rolePane(name, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	// Move roles and change the active window before any delivery.
	if _, err := tmux("swap-pane", "-s", worker, "-t", reviewer); err != nil {
		t.Fatal(err)
	}
	if _, err := tmux("new-window", "-t", "="+name+":", "-c", project); err != nil {
		t.Fatal(err)
	}
	mustCLI("send", "worker", "TASK: implement and request review")
	waitFor("approval", func() bool {
		messages := readLog("worker")
		return len(messages) == 3 && strings.HasSuffix(messages[2], "APPROVED")
	})
	waitFor("delivery receipts", func() bool {
		messages, err := readMessages(runtime)
		if err != nil || len(messages) != 5 {
			return false
		}
		for _, m := range messages {
			if m.Status != "delivered" {
				return false
			}
		}
		return true
	})
	feedback := readLog("worker")[1]
	if !strings.HasSuffix(feedback, "CORRECTIONS:\nFix the retry case.\nПеревір UTF-8.") {
		t.Fatalf("multiline feedback corrupted: %q", feedback)
	}
	// Killing and concurrently restarting the detached bridge must not replay.
	h, err := readHealth(runtime)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(h.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitFor("released bridge lock", func() bool { running, _ := bridgeRunning(runtime); return !running })
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b, err := cli("start", "--detach"); err != nil {
				t.Errorf("restart: %s: %v", b, err)
			}
		}()
	}
	wg.Wait()
	time.Sleep(700 * time.Millisecond)
	if len(readLog("worker")) != 3 || len(readLog("reviewer")) != 2 {
		t.Fatal("bridge restart replayed messages")
	}
	mustCLI("start", "--agents", "--detach")
	// Copy mode and disabled input hold messages until the pane can receive.
	if _, err := tmux("copy-mode", "-t", reviewer); err != nil {
		t.Fatal(err)
	}
	mustCLI("send", "reviewer", "COPYMODE: hold until copy mode ends")
	waitFor("copy mode holds message", func() bool {
		messages, _ := readMessages(runtime)
		return len(messages) == 6 && messages[5].Status == "queued" && messages[5].Error != ""
	})
	if len(readLog("reviewer")) != 2 {
		t.Fatal("copy mode consumed the message")
	}
	if _, err := tmux("send-keys", "-X", "-t", reviewer, "cancel"); err != nil {
		t.Fatal(err)
	}
	waitFor("delivery after copy mode", func() bool { return len(readLog("reviewer")) == 3 })
	if _, err := tmux("select-pane", "-d", "-t", worker); err != nil {
		t.Fatal(err)
	}
	mustCLI("send", "worker", "INPUT: hold while input is disabled")
	waitFor("disabled input holds message", func() bool {
		messages, _ := readMessages(runtime)
		return len(messages) == 7 && messages[6].Status == "queued" && messages[6].Error != ""
	})
	if len(readLog("worker")) != 3 {
		t.Fatal("disabled pane consumed the message")
	}
	if _, err := tmux("select-pane", "-e", "-t", worker); err != nil {
		t.Fatal(err)
	}
	waitFor("delivery after input enabled", func() bool { return len(readLog("worker")) == 4 })
	// After an agent exits, its shell must not receive the queued text.
	if _, err := tmux("respawn-pane", "-k", "-t", reviewer, "/bin/sh"); err != nil {
		t.Fatal(err)
	}
	mustCLI("send", "reviewer", "MANUAL: literal `echo x` $(echo y)\nдругий рядок")
	waitFor("queued while agent absent", func() bool {
		messages, _ := readMessages(runtime)
		return len(messages) == 8 && messages[7].Status == "queued" && messages[7].Error != ""
	})
	text, err := tmux("capture-pane", "-p", "-t", reviewer)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, "MANUAL:") {
		t.Fatal("message was pasted into a shell")
	}
	if _, err := tmux("respawn-pane", "-k", "-t", reviewer, filepath.Join(binDir, "pi"), "fixture"); err != nil {
		t.Fatal(err)
	}
	waitFor("queued message delivered after restart", func() bool {
		messages := readLog("reviewer")
		return len(messages) == 4 && strings.Contains(messages[3], "MANUAL:")
	})
	status := mustCLI("status")
	if !strings.Contains(status, "Bridge: running") || !strings.Contains(status, "worker pane: "+worker) || !strings.Contains(status, "reviewer pane: "+reviewer) {
		t.Fatal(status)
	}
	// New panes do not silently inherit an old role.
	if _, err := tmux("kill-pane", "-t", reviewer); err != nil {
		t.Fatal(err)
	}
	if _, err := tmux("split-window", "-h", "-t", worker, "-c", project); err != nil {
		t.Fatal(err)
	}
	if b, err := cli("send", "reviewer", "must fail"); err == nil || !strings.Contains(b, "missing or dead") {
		t.Fatalf("missing role accepted: %s: %v", b, err)
	}
	mustCLI("stop")
	waitFor("bridge stop", func() bool { running, _ := bridgeRunning(runtime); return !running })
	if exists, err := sessionExists(name); err != nil || exists {
		t.Fatal(exists, err)
	}
	// A conflicting unowned session is never killed by 2mux stop.
	if _, err := tmux("new-session", "-d", "-s", name, "-c", project); err != nil {
		t.Fatal(err)
	}
	if b, err := cli("stop"); err == nil || !strings.Contains(b, "not owned") {
		t.Fatalf("unowned session accepted: %s: %v", b, err)
	}
	if exists, _ := sessionExists(name); !exists {
		t.Fatal("foreign session was removed")
	}
	t.Log(fmt.Sprintf("verified concurrent first starts across TMPDIRs, stop during initialization, concurrent agent starts, 8 deliveries, multiline UTF-8, detached cycle, pane swaps, extra windows, bridge restart, copy mode, disabled input, shell guard, missing roles and session ownership"))
}
