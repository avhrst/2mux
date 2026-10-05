package main

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
)

var headerMessageID = regexp.MustCompile(`(?m)^\[2mux message ([0-9a-f]{32}) from (?:user|worker|reviewer) to (?:worker|reviewer)\]`)

// Hook input is deliberately reduced to session IDs, event names and message
// IDs. Tool arguments, prompts and transcript paths are never persisted here.
func runHook(dir, role string, input io.Reader) error {
	if !validRole(role) {
		return fmt.Errorf("invalid hook role")
	}
	if err := validatePrivateDirectory(dir); err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(input, 2<<20))
	if err != nil {
		return err
	}
	if len(data) >= 2<<20 {
		return fmt.Errorf("hook payload too large")
	}
	var hook struct {
		Event        string `json:"hook_event_name"`
		Session      string `json:"session_id"`
		Notification string `json:"notification_type"`
		Prompt       string `json:"prompt"`
	}
	if err = json.Unmarshal(data, &hook); err != nil {
		return err
	}
	cfg, err := readSessionConfig(dir)
	if err != nil {
		return err
	}
	if role == roleReviewer && cfg.ClaudeSession != "" && hook.Session != cfg.ClaudeSession {
		return fmt.Errorf("hook belongs to another session")
	}
	s := readAgentState(dir, role)
	s.Source = "claude-hooks"
	s.SessionID = hook.Session
	switch hook.Event {
	case "SessionStart":
		s.State = "starting"
	case "UserPromptSubmit":
		s.State = "busy"
		if match := headerMessageID.FindStringSubmatch(hook.Prompt); len(match) > 1 {
			// Missing/archived receipts must not prevent recording a real busy
			// event or make a user's prompt fail because of an observing hook.
			_ = acceptMessage(dir, match[1], role)
		}
	case "PreToolUse":
		s.State = "busy"
	case "PostToolUse", "PostToolUseFailure":
		s.State = "busy"
	case "Stop":
		s.State = "idle"
	case "SessionEnd":
		s.State = "exited"
	case "Notification":
		if strings.Contains(hook.Notification, "permission") {
			s.State = "awaiting_approval"
		} else if hook.Notification == "idle_prompt" {
			s.State = "idle"
		} else {
			return nil
		}
	default:
		return nil
	}
	return writeAgentState(dir, s)
}
func hookSettings(binary, dir string) map[string]any {
	hooks := map[string]any{}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "PostToolUseFailure", "Notification", "Stop", "SessionEnd"} {
		hooks[event] = []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": shellQuote(binary) + " _hook " + shellQuote(dir) + " reviewer"}}}}
	}
	return map[string]any{"hooks": hooks}
}
