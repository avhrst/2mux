package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type paneInfo struct {
	ID   string
	Left int
	Dead bool
}

func tmuxAvailable() error {
	if _, err := exec.LookPath("tmux"); err != nil {
		return errors.New("tmux is required but was not found in PATH. Install tmux and try again")
	}
	if _, err := tmux("-V"); err != nil {
		return fmt.Errorf("tmux is installed but could not start: %w", err)
	}
	return nil
}

func tmux(args ...string) (string, error) {
	return tmuxInput(nil, args...)
}

func tmuxInput(input io.Reader, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tmux", args...)
	cmd.Stdin = input
	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("tmux %s timed out: %w", args[0], ctx.Err())
		}
		reason := strings.TrimSpace(string(output))
		if reason == "" {
			reason = err.Error()
		}
		return "", fmt.Errorf("tmux %s: %s", args[0], reason)
	}
	return strings.TrimSpace(string(output)), nil
}

func sessionExists(name string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tmux", "has-session", "-t", "="+name)
	output, err := cmd.CombinedOutput()
	if err == nil {
		actual, err := tmux("display-message", "-p", "-t", "="+name+":", "#{session_name}")
		if err != nil {
			return false, err
		}
		return actual == name, nil
	}
	var exitErr *exec.ExitError
	if ctx.Err() != nil {
		return false, fmt.Errorf("cannot check tmux session: %w", ctx.Err())
	}
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("cannot check tmux session: %s: %w", strings.TrimSpace(string(output)), err)
}

func sessionLifecycleLockPath(cwd string) string {
	// /tmp is shared by callers on supported Linux/macOS hosts. Unlike
	// os.TempDir(), it does not change with a shell's TMPDIR environment.
	return filepath.Join("/tmp", fmt.Sprintf("2mux-control-%d", os.Getuid()), sessionName(cwd)+".lock")
}

func lockSessionLifecycle(cwd string) (*os.File, error) {
	// A stable directory is needed before the session's unique runtime exists.
	// Validate an existing directory rather than changing someone else's mode.
	path := sessionLifecycleLockPath(cwd)
	dir := filepath.Dir(path)
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return nil, fmt.Errorf("create session control directory: %w", err)
	}
	if err := validatePrivateDirectory(dir); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		lock, ok, err := tryLock(path)
		if err != nil {
			return nil, err
		}
		if ok {
			return lock, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, errors.New("another 2mux start or stop is still in progress; try again")
}

func createTwoPaneSession(name, cwd string) (err error) {
	dir, err := os.MkdirTemp("", "2mux-")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(dir)
		}
	}()
	if err = os.Mkdir(filepath.Join(dir, "messages"), 0700); err != nil {
		return err
	}
	worker, err := tmux("new-session", "-d", "-P", "-F", "#{pane_id}", "-s", name, "-c", cwd)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = killSession(name)
		}
	}()

	if _, err = tmux("set-option", "-t", "="+name+":", "@twomux_cwd", cwd); err != nil {
		return err
	}
	var reviewer string
	reviewer, err = tmux("split-window", "-h", "-p", "50", "-P", "-F", "#{pane_id}", "-t", worker, "-c", cwd)
	if err != nil {
		return err
	}
	for option, value := range map[string]string{"@twomux_worker": worker, "@twomux_reviewer": reviewer, "@twomux_runtime": dir} {
		if _, err = tmux("set-option", "-t", "="+name+":", option, value); err != nil {
			return err
		}
	}
	if _, err = tmux("select-layout", "-t", worker, "even-horizontal"); err != nil {
		return err
	}
	if _, err = tmux("set-option", "-w", "-t", worker, "pane-border-status", "top"); err != nil {
		return err
	}
	if _, err = tmux("set-option", "-w", "-t", worker, "pane-border-format", " #{?#{==:#{pane_id},#{@twomux_worker}},WORKER,#{?#{==:#{pane_id},#{@twomux_reviewer}},REVIEWER,#{pane_title}}} "); err != nil {
		return err
	}
	if _, err = tmux("set-option", "-t", "="+name+":", "mouse", "on"); err != nil {
		return err
	}
	if _, err = tmux("select-pane", "-t", worker, "-T", "WORKER"); err != nil {
		return err
	}
	if _, err = tmux("select-pane", "-t", reviewer, "-T", "REVIEWER"); err != nil {
		return err
	}
	if _, err = tmux("select-pane", "-t", worker); err != nil {
		return err
	}
	return nil
}

func verifySessionDirectory(name, cwd string) error {
	actual, err := tmux("show-option", "-v", "-t", "="+name+":", "@twomux_cwd")
	if err != nil || actual != cwd {
		return fmt.Errorf("session %q already exists but is not owned by 2mux in this directory", name)
	}
	return nil
}

func attachSession(name string) error {
	args := []string{"attach-session", "-t", "=" + name}
	if os.Getenv("TMUX") != "" {
		args = []string{"switch-client", "-t", "=" + name}
	}
	cmd := exec.Command("tmux", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to attach to 2mux session: %w", err)
	}
	return nil
}

func killSession(name string) error {
	_, err := tmux("kill-session", "-t", "="+name)
	return err
}

func listSessionPanes(name string) ([]paneInfo, error) {
	output, err := tmux("list-panes", "-s", "-t", "="+name+":", "-F", "#{pane_id}\t#{pane_left}\t#{pane_dead}")
	if err != nil {
		return nil, err
	}
	var panes []paneInfo
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			return nil, fmt.Errorf("unexpected tmux pane output: %q", line)
		}
		left, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, fmt.Errorf("unexpected tmux pane position: %q", fields[1])
		}
		panes = append(panes, paneInfo{ID: fields[0], Left: left, Dead: fields[2] == "1"})
	}
	sort.Slice(panes, func(i, j int) bool { return panes[i].Left < panes[j].Left })
	return panes, nil
}

func rolePane(name, role string) (string, error) {
	if !validRole(role) {
		return "", fmt.Errorf("unknown role %q", role)
	}
	id, err := tmux("show-option", "-v", "-t", "="+name+":", "@twomux_"+role)
	if err != nil {
		return "", fmt.Errorf("%s pane is not registered; save work and recreate this session with '2mux stop' and '2mux start'", role)
	}
	panes, err := listSessionPanes(name)
	if err != nil {
		return "", err
	}
	for _, pane := range panes {
		if pane.ID == id && !pane.Dead {
			return id, nil
		}
	}
	return "", fmt.Errorf("%s pane %s is missing or dead", role, id)
}

func runtimeDirectory(name string) (string, error) {
	dir, err := tmux("show-option", "-v", "-t", "="+name+":", "@twomux_runtime")
	if err != nil {
		return "", errors.New("session has no bridge runtime; save work and recreate it with '2mux stop' and '2mux start'")
	}
	if err := validatePrivateDirectory(dir); err != nil {
		return "", err
	}
	return dir, nil
}

func validatePrivateDirectory(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil || !filepath.IsAbs(dir) || !info.IsDir() || info.Mode().Perm() != 0700 {
		return fmt.Errorf("invalid or missing private bridge directory %q", dir)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return errors.New("bridge directory belongs to another user")
	}
	return nil
}

func verifyRuntime(name, cwd, dir string) error {
	if err := verifySessionDirectory(name, cwd); err != nil {
		return err
	}
	actual, err := runtimeDirectory(name)
	if err != nil {
		return err
	}
	if actual != dir {
		return errors.New("session was recreated with a different bridge runtime")
	}
	return nil
}
