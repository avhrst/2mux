package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Smoke-test installed CLIs without submitting any task or model prompt.
// This checks process identification, not model execution or TUI prompt handling.
func TestInstalledAgentPresence(t *testing.T) {
	if os.Getenv("TWOMUX_NATIVE_SMOKE") != "1" {
		t.Skip("set TWOMUX_NATIVE_SMOKE=1 to check installed Codex and Claude Code")
	}
	paths := map[string]string{}
	for _, program := range []string{"go", "tmux", "codex", "claude"} {
		path, err := exec.LookPath(program)
		if err != nil {
			t.Fatal(err)
		}
		paths[program] = path
	}
	dir, err := os.MkdirTemp("/tmp", "2mux-native-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", dir)
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(binDir, "2mux")
	if output, err := exec.Command(paths["go"], "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %s: %v", output, err)
	}
	wrapper := "#!/bin/sh\nexec " + shellQuote(paths["tmux"]) + " -S " + shellQuote(filepath.Join(dir, "socket")) + " -f /dev/null \"$@\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "tmux"), []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	project := filepath.Join(dir, "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(sessionLifecycleLockPath(project))
	cli := func(args ...string) (string, error) {
		cmd := exec.Command(binary, args...)
		cmd.Dir = project
		output, err := cmd.CombinedOutput()
		return string(output), err
	}
	var runtime string
	defer func() {
		_, _ = cli("stop")
		_, _ = tmux("kill-server")
		if runtime != "" {
			os.RemoveAll(runtime)
		}
	}()
	if output, err := cli("start", "--detach"); err != nil {
		t.Fatalf("start: %s: %v", output, err)
	}
	name := sessionName(project)
	runtime, err = runtimeDirectory(name)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := rolePane(name, "worker")
	if err != nil {
		t.Fatal(err)
	}
	reviewer, err := rolePane(name, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmux("set-option", "-w", "-t", worker, "remain-on-exit", "on"); err != nil {
		t.Fatal(err)
	}
	if _, err := tmux("respawn-pane", "-k", "-t", worker, paths["codex"], "--no-alt-screen"); err != nil {
		t.Fatal(err)
	}
	if _, err := tmux("respawn-pane", "-k", "-t", reviewer, paths["claude"], "--disallowedTools", reviewerDisallowedTools); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if paneCanReceive(worker) == nil && paneCanReceive(reviewer) == nil {
			status, err := cli("status")
			if err != nil || strings.Count(status, "(agent present)") != 2 {
				t.Fatalf("status: %s: %v", status, err)
			}
			t.Log("installed Codex and Claude Code detected; no task or model prompt submitted")
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	status, _ := cli("status")
	t.Fatalf("native agent identification failed:\n%s", status)
}
