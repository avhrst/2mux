package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func supportedCodexVersion(version string) bool {
	return version == "codex-cli 0.159.2" || version == "codex-cli 0.160.0"
}

// An explicit native request must never become a mixed/legacy session silently.
// Keep the experimental protocol gate exact until a new version is validated.
func checkNativeVersions(cfg sessionConfig) error {
	for _, gate := range []struct {
		enabled  bool
		program  string
		expected string
	}{
		{cfg.CodexAPI, "codex", "codex-cli 0.159.2 or 0.160.0"},
		{cfg.ClaudeChannel, "claude", "2.1.289 (Claude Code)"},
	} {
		if !gate.enabled {
			continue
		}
		ctx, cancel := rpcTimeout()
		output, err := exec.CommandContext(ctx, gate.program, "--version").Output()
		cancel()
		if err != nil {
			return fmt.Errorf("cannot verify %s native CLI version: %w; no tmux fallback performed", gate.program, err)
		}
		installed := strings.TrimSpace(string(output))
		supported := installed == gate.expected
		if gate.program == "codex" {
			supported = supportedCodexVersion(installed)
		}
		if !supported {
			return fmt.Errorf("%s native transport does not support installed version %q; validated: %s; no tmux fallback performed. Select legacy transport explicitly or use a validated CLI", gate.program, installed, gate.expected)
		}
	}
	return nil
}

// The backend runs in its own unselected tmux window so it shares the session's
// lifecycle, including abrupt bridge crashes. Stop never kills a shared daemon.
func prepareNative(name, cwd, dir string, cfg sessionConfig) error {
	if err := checkNativeVersions(cfg); err != nil {
		return err
	}
	if err := initializeNativeConfig(dir, &cfg); err != nil {
		return err
	}
	if cfg.ClaudeChannel {
		fmt.Println("Experimental Claude Channel uses --dangerously-load-development-channels server:twomux. Confirm its fullscreen development-channel dialog at each launch and any MCP consent yourself in the reviewer pane. Tool permissions remain enabled.")
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		mcp := map[string]any{"mcpServers": map[string]any{"twomux": map[string]any{"command": exe, "args": []string{"_channel", dir, roleReviewer}}}}
		if err := writeJSON(filepath.Join(dir, "mcp.json"), mcp); err != nil {
			return err
		}
	}
	if !cfg.CodexAPI {
		return nil
	}
	return prepareCodex(name, cwd, dir, cfg)
}

func initializeNativeConfig(dir string, cfg *sessionConfig) error {
	lock, err := waitLock(filepath.Join(dir, "codex-thread.lock"), 5*time.Second, "codex thread setup is still in progress")
	if err != nil {
		return err
	}
	defer lock.Close()
	current, err := readSessionConfig(dir)
	if err != nil {
		return err
	}
	if current.CWD != "" {
		if current.CWD != cfg.CWD {
			return errors.New("codex session directory mismatch")
		}
		// A bridge recovery may have saved a replacement since start read cfg.
		*cfg = current
	}
	if cfg.ClaudeSession == "" {
		id, err := randomID()
		if err != nil {
			return err
		}
		cfg.ClaudeSession = fmt.Sprintf("%s-%s-%s-%s-%s", id[:8], id[8:12], id[12:16], id[16:20], id[20:])
	}
	return writeSessionConfig(dir, *cfg)
}

// prepareCodex starts the app-server backend when needed and guarantees that
// the configured worker thread can be resumed by the native TUI.
func prepareCodex(name, cwd, dir string, cfg sessionConfig) error {
	lock, err := waitLock(filepath.Join(dir, "codex-thread.lock"), 5*time.Second, "codex thread setup is still in progress")
	if err != nil {
		return err
	}
	defer lock.Close()
	// The bridge and respawn can both discover a stale thread. Only one may
	// replace it, and neither may overwrite the other's saved replacement.
	cfg, err = readSessionConfig(dir)
	if err != nil {
		return err
	}
	if cfg.CWD != cwd {
		return errors.New("codex session directory mismatch")
	}
	fmt.Println("Experimental Codex API: G2 multi-client approval routing is not verified. The bridge never answers approvals; use the native TUI and report any missing approval dialog.")
	socket := filepath.Join(dir, "codex.sock")
	ctx, cancel := rpcTimeout()
	rpc, err := dialCodex(ctx, socket)
	cancel()
	if err != nil {
		path, err := exec.LookPath("codex")
		if err != nil {
			return err
		}
		if cfg.BackendPane != "" {
			dead, probeErr := tmux("display-message", "-p", "-t", cfg.BackendPane, "#{pane_dead}")
			if probeErr == nil && dead == "0" {
				return fmt.Errorf("codex backend is running but unavailable; inspect pane %s", cfg.BackendPane)
			}
		}
		pane, err := tmux("new-window", "-d", "-P", "-F", "#{pane_id}", "-t", sessionTarget(name), "-n", "2mux-api", "-c", cwd, path, "app-server", "--listen", "unix://"+socket)
		if err != nil {
			return err
		}
		cfg.BackendPane = pane
		if err := writeSessionConfig(dir, cfg); err != nil {
			return err
		}
		if _, err := tmux("set-option", "-w", "-t", pane, "remain-on-exit", "on"); err != nil {
			return err
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			ctx, cancel := rpcTimeout()
			rpc, err = dialCodex(ctx, socket)
			cancel()
			if err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			return fmt.Errorf("codex native backend unavailable: %w", err)
		}
	}
	defer rpc.close()
	ctx, cancel = rpcTimeout()
	defer cancel()
	_, err = ensureWorkerThread(ctx, rpc, dir, &cfg, func() (string, error) { return rolePrompt(name, roleWorker) })
	if err != nil {
		return err
	}
	return writeAgentState(dir, agentState{Role: roleWorker, State: "starting", Source: "codex-api", SessionID: cfg.CodexThread})
}

type codexThread struct {
	ID     string          `json:"id"`
	CWD    string          `json:"cwd"`
	Status json.RawMessage `json:"status"`
}

func (t codexThread) validate(id, cwd string) error {
	if t.ID == "" || (id != "" && t.ID != id) || (cwd != "" && t.CWD != cwd) {
		return errors.New("codex thread identity mismatch")
	}
	return nil
}

// ensureWorkerThread returns a worker thread the native TUI can resume. The
// role instructions are added without starting a model turn; user
// model/sandbox/approval settings remain authoritative. Codex writes a thread's
// rollout only after its first turn or a metadata change, and thread/resume
// (including the TUI's) requires that rollout, so a new thread is named to
// persist it immediately.
func ensureWorkerThread(ctx context.Context, rpc *codexRPC, dir string, cfg *sessionConfig, prompt func() (string, error)) (codexThread, error) {
	var response struct {
		Thread codexThread `json:"thread"`
	}
	if cfg.CodexThread != "" {
		err := rpc.call(ctx, "thread/resume", map[string]any{"threadId": cfg.CodexThread}, &response)
		if err == nil {
			return response.Thread, response.Thread.validate(cfg.CodexThread, cfg.CWD)
		}
		if !missingRollout(err) {
			return codexThread{}, err
		}
		fmt.Printf("Codex worker thread %s has no rollout; starting a new one.\n", cfg.CodexThread)
		// Persist the invalidation before doing anything that can fail. A failed
		// replacement must not leave the unresumable ID blocking every restart.
		cfg.CodexThread = ""
		if err := writeSessionConfig(dir, *cfg); err != nil {
			return codexThread{}, err
		}
	}
	instructions, err := prompt()
	if err != nil {
		return codexThread{}, err
	}
	if err := rpc.call(ctx, "thread/start", map[string]any{"cwd": cfg.CWD, "developerInstructions": instructions}, &response); err != nil {
		return codexThread{}, err
	}
	if err := response.Thread.validate("", cfg.CWD); err != nil {
		return codexThread{}, err
	}
	if err := rpc.call(ctx, "thread/name/set", map[string]any{"threadId": response.Thread.ID, "name": "2mux worker"}, nil); err != nil {
		return codexThread{}, fmt.Errorf("persist codex thread: %w", err)
	}
	cfg.CodexThread = response.Thread.ID
	if err := writeSessionConfig(dir, *cfg); err != nil {
		return codexThread{}, err
	}
	return response.Thread, nil
}

// missingRollout reports Codex's refusal to resume a thread it never persisted.
func missingRollout(err error) bool {
	var rpcErr *rpcError
	return errors.As(err, &rpcErr) && rpcErr.Code == -32600 && strings.Contains(rpcErr.Message, "no rollout found")
}

type nativeTransport struct {
	dir   string
	cfg   sessionConfig
	rpc   *codexRPC
	panes paneWatch
}

func (t *nativeTransport) close() {
	if t.rpc != nil {
		t.rpc.close()
		t.rpc = nil
	}
}
func (t *nativeTransport) connect() error {
	lock, err := waitLock(filepath.Join(t.dir, "codex-thread.lock"), 5*time.Second, "codex thread setup is still in progress")
	if err != nil {
		return err
	}
	defer lock.Close()
	cfg, err := readSessionConfig(t.dir)
	if err != nil {
		return err
	}
	if t.cfg.CWD != "" && cfg.CWD != t.cfg.CWD {
		return errors.New("codex session directory mismatch")
	}
	if cfg.CodexThread != t.cfg.CodexThread {
		t.close()
	}
	t.cfg = cfg
	if t.rpc != nil {
		select {
		case <-t.rpc.done:
			t.close()
		default:
			return nil
		}
	}
	ctx, cancel := rpcTimeout()
	defer cancel()
	r, err := dialCodex(ctx, filepath.Join(t.dir, "codex.sock"))
	if err != nil {
		return err
	}
	thread, err := ensureWorkerThread(ctx, r, t.dir, &t.cfg, func() (string, error) { return rolePrompt(sessionName(t.cfg.CWD), roleWorker) })
	if err != nil {
		r.close()
		return err
	}
	t.rpc = r
	if err := t.reconcile(); err != nil {
		t.close()
		return fmt.Errorf("codex receipt reconciliation: %w", err)
	}
	data, _ := json.Marshal(map[string]any{"threadId": t.cfg.CodexThread, "status": thread.Status})
	t.event(rpcEnvelope{Method: "thread/status/changed", Params: data})
	return nil
}
func (t *nativeTransport) choose(m message) (string, error) {
	if m.To == roleWorker && t.cfg.CodexAPI {
		return "codex", nil
	}
	if m.To == roleReviewer && t.cfg.ClaudeChannel {
		return "claude-channel", nil
	}
	return "tmux", nil
}
func (t *nativeTransport) ready(name string, m message) error {
	switch m.Transport {
	case "claude-channel":
		return errors.New("reviewer delivery is owned by its MCP Channel")
	case "codex":
		if err := t.connect(); err != nil {
			return err
		}
		s := readAgentState(t.dir, roleWorker)
		if s.State == "awaiting_approval" || s.State == "awaiting_input" {
			return fmt.Errorf("worker is %s", s.State)
		}
		return nil
	default:
		pane, err := rolePane(name, m.To)
		if err != nil {
			return err
		}
		// A managed hook can positively identify a busy/approval state. Missing
		// hook data on legacy sessions retains the existing process safeguards.
		s := readAgentState(t.dir, m.To)
		if s.Source == "claude-hooks" && s.State == "awaiting_approval" && time.Since(s.Updated) < hookApprovalTTL {
			return fmt.Errorf("%s is %s", m.To, s.State)
		}
		return t.panes.ready(pane, time.Now())
	}
}

const hookApprovalTTL = 15 * time.Second

func (t *nativeTransport) owns(m message) bool {
	return m.Transport != "claude-channel" && !(m.Transport == "" && m.To == roleReviewer && t.cfg.ClaudeChannel)
}
func (t *nativeTransport) submit(name string, m message) error {
	if m.Transport == "codex" {
		ctx, cancel := rpcTimeout()
		defer cancel()
		method := "thread/queue/add"
		params := map[string]any{"threadId": t.cfg.CodexThread, "clientUserMessageId": m.ID, "input": []any{map[string]string{"type": "text", "text": formatMessage(m)}}}
		if m.Steer {
			method = "turn/steer"
			params["expectedTurnId"] = m.TurnRef
		}
		if err := t.rpc.call(ctx, method, params, nil); err != nil {
			var rejected *rpcError
			if errors.As(err, &rejected) {
				return rejectedDelivery{err}
			}
			return uncertainDelivery{err}
		}
		return nil
	}
	pane, err := rolePane(name, m.To)
	if err != nil {
		return err
	}
	return sendText(pane, formatMessage(m))
}

type rejectedDelivery struct{ error }

func (t *nativeTransport) poll() {
	if t.rpc == nil {
		return
	}
	for {
		select {
		case e := <-t.rpc.events:
			t.event(e)
		default:
			return
		}
	}
}
func (t *nativeTransport) event(e rpcEnvelope) {
	var p struct {
		ThreadID string          `json:"threadId"`
		Status   json.RawMessage `json:"status"`
		Turn     struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"turn"`
		Item json.RawMessage `json:"item"`
	}
	if json.Unmarshal(e.Params, &p) != nil || p.ThreadID != t.cfg.CodexThread {
		return
	}
	s := readAgentState(t.dir, roleWorker)
	s.Source = "codex-api"
	s.SessionID = p.ThreadID
	switch e.Method {
	case "turn/started":
		s.State = "busy"
		s.TurnID = p.Turn.ID
	case "turn/completed":
		s.State = "idle"
		s.TurnID = ""
	case "thread/closed":
		s.State = "exited"
		s.TurnID = ""
	case "thread/status/changed":
		var status struct {
			Type  string   `json:"type"`
			Flags []string `json:"activeFlags"`
		}
		if json.Unmarshal(p.Status, &status) == nil {
			switch status.Type {
			case "idle":
				s.State = "idle"
			case "active":
				s.State = "busy"
				for _, flag := range status.Flags {
					if flag == "waitingOnApproval" {
						s.State = "awaiting_approval"
					}
					if flag == "waitingOnUserInput" {
						s.State = "awaiting_input"
					}
				}
			case "notLoaded", "systemError":
				s.State = "unknown"
			}
		}
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/permissions/requestApproval":
		s.State = "awaiting_approval"
	case "item/tool/requestUserInput":
		s.State = "awaiting_input"
	case "item/started", "item/completed":
		var item struct {
			Type     string `json:"type"`
			ClientID string `json:"clientId"`
			Content  []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(p.Item, &item) == nil && item.Type == "userMessage" {
			id := item.ClientID
			if id == "" {
				for _, c := range item.Content {
					if match := headerMessageID.FindStringSubmatch(c.Text); len(match) > 1 {
						id = match[1]
					}
				}
			}
			if validMessageID(id) {
				_ = acceptMessage(t.dir, id, roleWorker)
			}
		}
	default:
		return
	}
	_ = writeAgentState(t.dir, s)
}
