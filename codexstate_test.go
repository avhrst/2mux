package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodexScreenState(t *testing.T) {
	idle := "› Ask Codex to do anything\n\n  GPT-6.1-Sol · ~/Code/2mux\n  ← for agents · ? for shortcuts\n"
	for _, tc := range []struct{ name, screen, want string }{
		{"idle", idle, "idle"},
		{"typed input", strings.Replace(idle, "Ask Codex to do anything", "Implement this task", 1), "idle"},
		{"working with editable prompt", "• Working (2m 05s • esc to interrupt)\n" + idle, "busy"},
		{"tool status", "◦ Running tests (3s • esc to interrupt)\n" + idle, "busy"},
		{"long turn", "• Working (1h 2m 03s • esc to interrupt)\n" + idle, "busy"},
		{"approval", "Would you like to run the following command?\n\n  $ go test ./...\n› 1. Yes, proceed (y)\n  2. No, and tell Codex what to do differently (esc)\n", "awaiting_approval"},
		{"edit approval", "Would you like to make the following edits?\n› 1. Yes, proceed (y)\n", "awaiting_approval"},
		{"approval precedes old busy", "• Working (3s • esc to interrupt)\nWould you like to run the following command?\n› 1. Yes, proceed (y)\n", "awaiting_approval"},
		{"unrecognized dialog", "Please sign in to continue\n", "unknown"},
		{"empty", "", "unknown"},
		{"prompt without footer", "› Ask Codex to do anything\n", "unknown"},
		{"footer without prompt", "? for shortcuts\n", "unknown"},
		{"quoted transcript", "The agent is Working (3s • esc to interrupt)\n", "unknown"},
		{"approval prose", "Would you like to run the following command?\n", "unknown"},
		{"old working outside controls", "• Working (3s • esc to interrupt)\n" + strings.Repeat("output\n", 20) + idle, "idle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexScreenState(tc.screen); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestForegroundCodexStateObserver(t *testing.T) {
	for _, tc := range []struct {
		process string
		want    bool
	}{
		{"S+ codex /Users/user/.local/bin/codex", true},
		{"S+ /usr/local/bin/codex codex", true},
		{"S codex codex", false},
		{"S+ zsh zsh -c codex", false},
		{"S+ claude claude", false},
		{"S+ node node codex.js", false},
	} {
		if got := hasForegroundCodex(tc.process); got != tc.want {
			t.Fatalf("%q: got %v, want %v", tc.process, got, tc.want)
		}
	}
}

func TestCodexTUIStateRequiresHealthyBridge(t *testing.T) {
	dir := queueDir(t)
	if err := writeAgentState(dir, agentState{Role: roleWorker, State: "busy", Source: "codex-tui"}); err != nil {
		t.Fatal(err)
	}
	if got := observedAgentState(dir, roleWorker).State; got != "unknown" {
		t.Fatalf("missing bridge: %s", got)
	}
	if err := writeJSON(filepath.Join(dir, "health.json"), bridgeHealth{PID: os.Getpid(), Updated: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if got := observedAgentState(dir, roleWorker).State; got != "busy" {
		t.Fatalf("healthy observer: %s", got)
	}
	if err := writeJSON(filepath.Join(dir, "health.json"), bridgeHealth{PID: os.Getpid(), Updated: time.Now().Add(-2 * healthStaleAfter)}); err != nil {
		t.Fatal(err)
	}
	if got := observedAgentState(dir, roleWorker).State; got != "unknown" {
		t.Fatalf("stale bridge: %s", got)
	}
}

func TestTmuxLegacyWorkerStates(t *testing.T) {
	if os.Getenv("TWOMUX_INTEGRATION") != "1" {
		t.Skip("set TWOMUX_INTEGRATION=1 to run the isolated tmux state test")
	}
	e := setupIntegration(t)
	e.mustCLI(t, "start", "--agents", "--detach")
	var err error
	e.runtime, err = runtimeDirectory(e.name)
	if err != nil {
		t.Fatal(err)
	}
	pane := e.pane(t, roleWorker)
	e.waitFor(t, "worker fixture ready", func() bool {
		_, err := os.Stat(filepath.Join(e.dir, "worker.ready"))
		return err == nil && paneCanReceive(pane) == nil
	})
	for _, step := range []struct{ frame, want string }{
		{"idle", "idle"}, {"busy", "busy"}, {"idle", "idle"},
		{"approval", "awaiting_approval"}, {"unknown", "unknown"},
	} {
		if err := sendText(pane, "STATE:"+step.frame); err != nil {
			t.Fatal(err)
		}
		e.waitFor(t, "worker state "+step.want, func() bool {
			s := observedAgentState(e.runtime, roleWorker)
			line, err := tmux("show-option", "-v", "-t", sessionTarget(e.name), "status-right")
			return s.State == step.want && s.Source == "codex-tui" && err == nil && strings.Contains(line, "W:"+step.want+" ")
		})
		if output := e.mustCLI(t, "status", "--json"); !strings.Contains(output, `"source":"codex-tui"`) || !strings.Contains(output, `"state":"`+step.want+`"`) {
			t.Fatalf("JSON status did not report state: %s", output)
		}
	}
	// Copy mode must not retain the last recognized state, and returning to
	// an ordinary shell must not treat its prompt as an idle Codex instance.
	if err := sendText(pane, "STATE:busy"); err != nil {
		t.Fatal(err)
	}
	e.waitFor(t, "busy before copy mode", func() bool { return readAgentState(e.runtime, roleWorker).State == "busy" })
	if _, err := tmux("copy-mode", "-t", pane); err != nil {
		t.Fatal(err)
	}
	e.waitFor(t, "copy mode invalidates worker state", func() bool { return readAgentState(e.runtime, roleWorker).State == "unknown" })
	if _, err := tmux("send-keys", "-X", "-t", pane, "cancel"); err != nil {
		t.Fatal(err)
	}
	if _, err := tmux("respawn-pane", "-k", "-t", pane, "/bin/sh"); err != nil {
		t.Fatal(err)
	}
	e.waitFor(t, "shell worker is unknown", func() bool {
		return readAgentState(e.runtime, roleWorker).State == "unknown" && paneCanReceive(pane) != nil
	})
	if _, err := tmux("kill-pane", "-t", pane); err != nil {
		t.Fatal(err)
	}
	e.waitFor(t, "removed worker exited", func() bool { return readAgentState(e.runtime, roleWorker).State == "exited" })
}
