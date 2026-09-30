package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type bridgeHealth struct {
	PID       int       `json:"pid"`
	Updated   time.Time `json:"updated"`
	LastError string    `json:"last_error,omitempty"`
}

// flock is released by the kernel even if a bridge is killed abruptly.
func tryLock(path string) (*os.File, bool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, false, err
	}
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		f.Close()
		return nil, false, nil
	}
	if err != nil {
		f.Close()
		return nil, false, err
	}
	return f, true, nil
}

func bridgeRunning(dir string) (bool, error) {
	f, locked, err := tryLock(filepath.Join(dir, "bridge.lock"))
	if f != nil {
		f.Close()
	}
	return !locked, err
}

func readHealth(dir string) (bridgeHealth, error) {
	var h bridgeHealth
	b, err := os.ReadFile(filepath.Join(dir, "health.json"))
	if err == nil {
		err = json.Unmarshal(b, &h)
	}
	return h, err
}

func ensureBridge(name, cwd, dir string) error {
	// Serialize concurrent starts, including the period before the child locks.
	var lock *os.File
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f, ok, err := tryLock(filepath.Join(dir, "start.lock"))
		if err != nil {
			return err
		}
		if ok {
			lock = f
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if lock == nil {
		return errors.New("another 2mux start is still in progress")
	}
	defer lock.Close()
	running, err := bridgeRunning(dir)
	if err != nil {
		return err
	}
	if running {
		h, err := readHealth(dir)
		if err != nil || time.Since(h.Updated) > 5*time.Second {
			return errors.New("bridge is locked but unresponsive; inspect '2mux status'")
		}
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	log, err := os.OpenFile(filepath.Join(dir, "bridge.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command(exe, "_bridge", name, cwd, dir)
	cmd.Dir = cwd
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start bridge: %w", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h, err := readHealth(dir)
		if err == nil && h.PID == pid && time.Since(h.Updated) < 5*time.Second {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("bridge did not become ready; inspect %s", filepath.Join(dir, "bridge.log"))
}

func runBridge(name, cwd, dir string) error {
	if err := verifyRuntime(name, cwd, dir); err != nil {
		return err
	}
	lock, ok, err := tryLock(filepath.Join(dir, "bridge.lock"))
	if err != nil || !ok {
		return err
	}
	defer lock.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	h := bridgeHealth{PID: os.Getpid()}
	h.Updated = time.Now().UTC()
	if err := writeJSON(filepath.Join(dir, "health.json"), h); err != nil {
		return err
	}
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		// The directory token distinguishes a recreated session with the same name.
		if err := verifyRuntime(name, cwd, dir); err != nil {
			return nil
		}
		h.LastError = ""
		if err := deliverQueue(dir, func(m message) error {
			pane, err := rolePane(name, m.To)
			if err != nil {
				return err
			}
			return sendText(pane, formatMessage(m))
		}); err != nil {
			h.LastError = err.Error()
		}
		h.Updated = time.Now().UTC()
		if err := writeJSON(filepath.Join(dir, "health.json"), h); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

type uncertainDelivery struct{ err error }

func (e uncertainDelivery) Error() string { return e.err.Error() }
func (e uncertainDelivery) Unwrap() error { return e.err }

func sendText(paneID, text string) error {
	// Do not paste agent output into a shell, editor, dead pane or copy mode.
	if err := paneCanReceive(paneID); err != nil {
		return err
	}
	id, err := randomID()
	if err != nil {
		return err
	}
	buffer := "2mux-" + id
	if _, err := tmuxInput(strings.NewReader(text), "load-buffer", "-b", buffer, "-"); err != nil {
		return err
	}
	defer tmux("delete-buffer", "-b", buffer)
	if err := paneCanReceive(paneID); err != nil {
		return err
	}
	// Bracketed paste preserves multiline feedback as one prompt. Keep LF bytes.
	if _, err := tmux("paste-buffer", "-d", "-p", "-r", "-b", buffer, "-t", paneID); err != nil {
		return uncertainDelivery{err}
	}
	// TUIs must finish handling the paste before the submit key arrives.
	time.Sleep(150 * time.Millisecond)
	if err := paneCanReceive(paneID); err != nil {
		return uncertainDelivery{err}
	}
	if _, err := tmux("send-keys", "-t", paneID, "Enter"); err != nil {
		return uncertainDelivery{err}
	}
	return nil
}

func paneCanReceive(paneID string) error {
	output, err := tmux("display-message", "-p", "-t", paneID,
		"#{pane_dead}\t#{pane_in_mode}\t#{pane_input_off}\t#{pane_tty}")
	if err != nil {
		return err
	}
	fields := strings.Split(output, "\t")
	if len(fields) != 4 || fields[0] != "0" || fields[1] != "0" || fields[2] != "0" {
		return fmt.Errorf("pane %s is dead, in copy mode, or has input disabled", paneID)
	}
	// node alone is not evidence of pi. Inspect only foreground processes on
	// this terminal; a shell that remains after the agent exits is rejected.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-ww", "-t", strings.TrimPrefix(fields[3], "/dev/"), "-o", "stat=,ucomm=,args=")
	processes, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("inspect foreground agent in pane %s: %w", paneID, err)
	}
	if !hasForegroundAgent(string(processes)) {
		return fmt.Errorf("pane %s is waiting for an interactive Codex or pi agent", paneID)
	}
	return nil
}

func hasForegroundAgent(processes string) bool {
	for _, line := range strings.Split(processes, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || !strings.Contains(fields[0], "+") {
			continue
		}
		program := filepath.Base(fields[1])
		if program == "codex" || program == "pi" {
			return true
		}
		if program == "node" || program == "bun" {
			// Current pi sets process.title to "pi", replacing its argv on Unix.
			if len(fields) == 3 && fields[2] == "pi" {
				return true
			}
			// Only the script entry point identifies the agent. A pi path in
			// app arguments or runtime options must not authorize a paste.
			scriptIndex := 3
			if program == "bun" && len(fields) > scriptIndex && fields[scriptIndex] == "run" {
				scriptIndex++
			}
			if len(fields) > scriptIndex && isPiEntrypoint(fields[scriptIndex]) {
				return true
			}
		}
	}
	return false
}

func isPiEntrypoint(path string) bool {
	if strings.HasPrefix(path, "-") {
		return false
	}
	if filepath.IsAbs(path) && filepath.Base(path) == "pi" {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return false
		}
		path = resolved
	}
	return strings.Contains(path, "/pi-coding-agent/") && strings.HasSuffix(path, ".js")
}

func stopBridge(dir string) error {
	running, err := bridgeRunning(dir)
	if err != nil || !running {
		return err
	}
	h, err := readHealth(dir)
	if err != nil {
		return err
	}
	if h.PID <= 0 || time.Since(h.Updated) > 5*time.Second {
		return errors.New("cannot safely identify bridge process; bridge exits when session disappears")
	}
	p, err := os.FindProcess(h.PID)
	if err != nil {
		return err
	}
	defer p.Release()
	if err := p.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		running, err = bridgeRunning(dir)
		if err != nil || !running {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("bridge did not stop in time")
}

func bridgeDescription(dir string) string {
	running, err := bridgeRunning(dir)
	if err != nil {
		return "error: " + err.Error()
	}
	if !running {
		return "stopped (run '2mux start' to restart)"
	}
	h, err := readHealth(dir)
	if err != nil || time.Since(h.Updated) > 5*time.Second {
		return "unresponsive"
	}
	s := "running (PID " + strconv.Itoa(h.PID) + ")"
	if h.LastError != "" {
		s += "; " + h.LastError
	}
	return s
}
