package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type sessionConfig struct {
	CWD           string `json:"cwd"`
	CodexAPI      bool   `json:"codex_api,omitempty"`
	ClaudeChannel bool   `json:"claude_channel,omitempty"`
	CodexThread   string `json:"codex_thread,omitempty"`
	ClaudeSession string `json:"claude_session,omitempty"`
	BackendPane   string `json:"backend_pane,omitempty"`
}

func configuredTransports(cfg sessionConfig) map[string]string {
	transports := map[string]string{roleWorker: "tmux", roleReviewer: "tmux"}
	if cfg.CodexAPI {
		transports[roleWorker] = "codex"
	}
	if cfg.ClaudeChannel {
		transports[roleReviewer] = "claude-channel"
	}
	return transports
}

func readSessionConfig(dir string) (sessionConfig, error) {
	var cfg sessionConfig
	data, err := os.ReadFile(filepath.Join(dir, "session.json"))
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	err = json.Unmarshal(data, &cfg)
	return cfg, err
}
func writeSessionConfig(dir string, cfg sessionConfig) error {
	return writeJSON(filepath.Join(dir, "session.json"), cfg)
}

type agentState struct {
	Role      string    `json:"role"`
	State     string    `json:"state"`
	Source    string    `json:"source"`
	SessionID string    `json:"session_id,omitempty"`
	TurnID    string    `json:"turn_id,omitempty"`
	Updated   time.Time `json:"updated"`
}

func validAgentState(s string) bool {
	switch s {
	case "unknown", "starting", "idle", "busy", "awaiting_approval", "awaiting_input", "exited":
		return true
	}
	return false
}
func writeAgentState(dir string, s agentState) error {
	if !validRole(s.Role) || !validAgentState(s.State) {
		return fmt.Errorf("invalid agent state")
	}
	s.Updated = time.Now().UTC()
	return writeJSON(filepath.Join(dir, s.Role+"-state.json"), s)
}
func readAgentState(dir, role string) agentState {
	s := agentState{Role: role, State: "unknown", Source: "none"}
	data, err := os.ReadFile(filepath.Join(dir, role+"-state.json"))
	if err == nil {
		if json.Unmarshal(data, &s) != nil || !validAgentState(s.State) {
			s.State = "unknown"
		}
	}
	return s
}

// The last event remains available for diagnostics, but an absent observer
// cannot assert that an agent is still idle or busy.
func observedAgentState(dir, role string) agentState {
	s := readAgentState(dir, role)
	if s.Source == "codex-api" || s.Source == "codex-tui" {
		h, err := readHealth(dir)
		if err != nil || time.Since(h.Updated) > healthStaleAfter {
			s.State = "unknown"
		}
	} else if s.Source == "claude-hooks" {
		cfg, err := readSessionConfig(dir)
		if err != nil || (cfg.ClaudeChannel && !channelAvailable(dir)) {
			s.State = "unknown"
		}
	}
	return s
}
func acceptMessage(dir, id, role string) error {
	if !validMessageID(id) || !validRole(role) {
		return fmt.Errorf("invalid acknowledgement")
	}
	lock, err := waitLock(filepath.Join(dir, "queue.lock"), 5*time.Second, "queue busy")
	if err != nil {
		return err
	}
	defer lock.Close()
	path := filepath.Join(dir, "messages", id+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	m, err := parseMessage(id+".json", data)
	if err != nil {
		return err
	}
	if m.To != role {
		return fmt.Errorf("acknowledgement belongs to another role")
	}
	if m.Status != statusSubmitted && m.Status != statusUncertain && m.Status != statusAccepted && m.Status != statusDelivered {
		return fmt.Errorf("message was not submitted")
	}
	m.Status, m.Error = statusAccepted, ""
	return writeJSON(path, m)
}
