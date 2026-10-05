package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// A legacy worker has no app-server observer. Recognize explicit TUI controls,
// never screen activity or transcript text, and leave unfamiliar screens unknown.
var codexWorking = regexp.MustCompile(`^[•◦∙·] .+ \([0-9]+(?:h [0-9]+m [0-9]+s|m [0-9]+s|s) [•·] esc to interrupt\)$`)

func codexScreenState(screen string) string {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	if len(lines) > 18 {
		lines = lines[len(lines)-18:]
	}
	var prompt, shortcuts, working, approval, approvalChoice bool
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if codexWorking.MatchString(line) {
			working = true
		}
		if line == "Would you like to run the following command?" || line == "Would you like to make the following edits?" {
			approval = true
		}
		if strings.HasPrefix(line, "› 1. Yes, proceed") {
			approvalChoice = true
		}
		if strings.HasPrefix(line, "›") {
			prompt = true
		}
		if strings.HasSuffix(line, "? for shortcuts") {
			shortcuts = true
		}
	}
	if approval && approvalChoice {
		return "awaiting_approval"
	}
	if working {
		return "busy"
	}
	if prompt && shortcuts {
		return "idle"
	}
	return "unknown"
}

func hasForegroundCodex(processes string) bool {
	for _, line := range strings.Split(processes, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && strings.Contains(fields[0], "+") && filepath.Base(fields[1]) == "codex" {
			return true
		}
	}
	return false
}

func observeLegacyWorker(name, dir string) {
	s := agentState{Role: roleWorker, State: "unknown", Source: "codex-tui"}
	pane, exists, alive, err := registeredPane(name, roleWorker)
	if err == nil && (!exists || !alive) {
		s.State = "exited"
	} else if err == nil {
		info, err := tmux("display-message", "-p", "-t", pane, "#{pane_in_mode}\t#{pane_input_off}\t#{pane_tty}")
		fields := strings.Split(info, "\t")
		if err == nil && len(fields) == 3 && fields[0] == "0" && fields[1] == "0" {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			processes, err := exec.CommandContext(ctx, "ps", "-ww", "-t", strings.TrimPrefix(fields[2], "/dev/"), "-o", "stat=,ucomm=,args=").Output()
			cancel()
			if err == nil && hasForegroundCodex(string(processes)) {
				// Join terminal wraps so narrow panes retain complete controls.
				if screen, err := tmux("capture-pane", "-p", "-J", "-t", pane); err == nil {
					s.State = codexScreenState(screen)
				}
			}
		}
	}
	previous := readAgentState(dir, roleWorker)
	if previous.State != s.State || previous.Source != s.Source || previous.SessionID != "" || previous.TurnID != "" {
		_ = writeAgentState(dir, s)
	}
}
