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
	"syscall"
	"time"
)

const (
	// Health is written after every delivery attempt; one attempt runs several
	// bounded tmux and ps calls, so allow a few of them before calling it stale.
	healthStaleAfter = 15 * time.Second
	// The bridge exits at once when its session is gone, but tolerates tmux
	// errors that do not prove that, such as a timeout under load.
	transientFailureLimit = 30 * time.Second
	archiveAfter          = 24 * time.Hour
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

// waitLock retries a nonblocking flock until timeout, for locks whose
// holders finish quickly, such as a delivery batch or another start.
func waitLock(path string, timeout time.Duration, busy string) (*os.File, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		f, ok, err := tryLock(path)
		if err != nil {
			return nil, err
		}
		if ok {
			return f, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, errors.New(busy)
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
	lock, err := waitLock(filepath.Join(dir, "start.lock"), 5*time.Second, "another 2mux start is still in progress")
	if err != nil {
		return err
	}
	defer lock.Close()
	running, err := bridgeRunning(dir)
	if err != nil {
		return err
	}
	if running {
		h, err := readHealth(dir)
		if err != nil || time.Since(h.Updated) > healthStaleAfter {
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
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h, err := readHealth(dir)
		if err == nil && h.PID == pid && time.Since(h.Updated) < healthStaleAfter {
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
	writeHealth := func() {
		h.Updated = time.Now().UTC()
		// A failed write makes the bridge look unresponsive to the CLI, which
		// is reported to the operator; it is not a reason to stop delivering.
		if err := writeJSON(filepath.Join(dir, "health.json"), h); err != nil {
			fmt.Fprintln(os.Stderr, "write health:", err)
		}
	}
	h.Updated = time.Now().UTC()
	if err := writeJSON(filepath.Join(dir, "health.json"), h); err != nil {
		return err
	}
	cfg, err := readSessionConfig(dir)
	if err != nil {
		return err
	}
	transport := &nativeTransport{dir: dir, cfg: cfg, panes: paneWatch{}}
	defer transport.close()
	var failingSince, lastArchive, lastStateCheck time.Time
	statusLine := ""
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		// The directory token distinguishes a recreated session with the same name.
		if err := verifyRuntime(name, cwd, dir); err != nil {
			if runtimeGone(name, cwd, dir) {
				return nil
			}
			if failingSince.IsZero() {
				failingSince = time.Now()
			}
			if time.Since(failingSince) > transientFailureLimit {
				return fmt.Errorf("cannot verify session for %s: %w", transientFailureLimit, err)
			}
			h.LastError = "cannot verify session: " + err.Error()
			writeHealth()
		} else {
			failingSince = time.Time{}
			h.LastError = ""
			if cfg.CodexAPI {
				if err := transport.connect(); err != nil {
					_ = writeAgentState(dir, agentState{Role: roleWorker, State: "unknown", Source: "codex-api", SessionID: transport.cfg.CodexThread})
					h.LastError = err.Error()
				}
			}
			transport.poll()
			if time.Since(lastStateCheck) >= time.Second {
				lastStateCheck = time.Now()
				if !cfg.CodexAPI {
					observeLegacyWorker(name, dir)
				}
				// A dead pane cannot retain a previous hook's idle/busy state.
				for _, role := range roles {
					state := readAgentState(dir, role)
					if state.Source == "none" {
						continue
					}
					_, exists, alive, probeErr := registeredPane(name, role)
					if probeErr == nil && (!exists || !alive) && state.State != "exited" {
						state.State, state.TurnID = "exited", ""
						_ = writeAgentState(dir, state)
					}
				}
				if cfg.CodexAPI || cfg.ClaudeSession != "" || cfg.ClaudeChannel || readAgentState(dir, roleWorker).Source != "none" {
					line := "2mux W:" + observedAgentState(dir, roleWorker).State + " R:" + observedAgentState(dir, roleReviewer).State
					if line != statusLine {
						if _, err := tmux("set-option", "-t", sessionTarget(name), "status-right", line); err == nil {
							statusLine = line
						}
					}
				}
			}
			err := deliverQueueTransport(dir, func(m message) error {
				return transport.ready(name, m)
			}, func(m message) error {
				// Keep health fresh within a batch of several deliveries.
				writeHealth()
				return transport.submit(name, m)
			}, transport.choose, transport.owns)
			if err != nil {
				h.LastError = err.Error()
			}
			writeHealth()
			if time.Since(lastArchive) > time.Minute {
				lastArchive = time.Now()
				if err := archiveDelivered(dir, archiveAfter); err != nil {
					fmt.Fprintln(os.Stderr, "archive delivered records:", err)
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// runtimeGone reports whether this bridge's session definitely no longer
// exists or now belongs to a different runtime. tmux errors that do not
// prove either, such as timeouts, return false.
func runtimeGone(name, cwd, dir string) bool {
	exists, err := sessionExists(name)
	if err != nil {
		return false
	}
	if !exists {
		return true
	}
	// The session answered, so a repeated marker mismatch is not transient.
	return verifyRuntime(name, cwd, dir) != nil
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
	if h.PID <= 0 || time.Since(h.Updated) > healthStaleAfter {
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
	if err != nil || time.Since(h.Updated) > healthStaleAfter {
		return "unresponsive"
	}
	s := "running (PID " + strconv.Itoa(h.PID) + ")"
	if h.LastError != "" {
		s += "; " + h.LastError
	}
	return s
}
