package main

// Pane readiness: whether it is safe to paste into a pane and press Enter.
// The bridge delivers only to a live, input-enabled pane whose foreground
// process is a recognized agent, whose screen has been quiet for a moment,
// and which shows no known confirmation dialog.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// A pane must show unchanged content this long before a paste, so text is
// not mixed into output that is still streaming or a prompt being typed.
const paneQuietPeriod = time.Second

type paneSnapshot struct {
	hash  [sha256.Size]byte
	since time.Time
}

// paneWatch remembers each pane's visible content across bridge ticks, so
// readiness can require a quiet screen without sleeping inside the loop.
type paneWatch map[string]paneSnapshot

func (w paneWatch) ready(pane string, now time.Time) error {
	if err := paneCanReceive(pane); err != nil {
		return err
	}
	screen, err := tmux("capture-pane", "-p", "-t", pane)
	if err != nil {
		return err
	}
	return w.observe(pane, screen, now)
}

func (w paneWatch) observe(pane, screen string, now time.Time) error {
	// Enter would answer a native approval dialog rather than submit a prompt.
	if confirmationVisible(screen) {
		return fmt.Errorf("pane %s shows a confirmation dialog; answer it before messages are delivered", pane)
	}
	hash := sha256.Sum256([]byte(screen))
	previous, seen := w[pane]
	if !seen || previous.hash != hash {
		w[pane] = paneSnapshot{hash, now}
		return fmt.Errorf("waiting for pane %s to become idle", pane)
	}
	if now.Sub(previous.since) < paneQuietPeriod {
		return fmt.Errorf("waiting for pane %s to become idle", pane)
	}
	return nil
}

// Phrases from Codex approval and trust dialogs and generic terminal
// confirmations. Only the bottom of the screen is checked, where dialogs and
// prompts render, so older transcript text rarely matches.
var confirmationPhrases = []string{
	"would you like to run the following command",
	"would you like to make the following edits",
	"yes, proceed",
	"don't ask again",
	"press enter to confirm",
	"enter to confirm",
	"do you trust",
	"(y/n)",
	"[y/n]",
}

func confirmationVisible(screen string) bool {
	var lines []string
	for _, line := range strings.Split(screen, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, strings.ToLower(line))
		}
	}
	if len(lines) > 15 {
		lines = lines[len(lines)-15:]
	}
	bottom := strings.Join(lines, "\n")
	for _, phrase := range confirmationPhrases {
		if strings.Contains(bottom, phrase) {
			return true
		}
	}
	return false
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
