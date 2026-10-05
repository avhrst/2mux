package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

const communicationPrefix = "╭─ 2mux ·"

const communicationUIInstruction = `Keep peer communication visible in your normal CLI conversation, not only in tool calls. For every incoming peer message, including notes, post one visible entry headed "2mux · SENDER → RECIPIENT · TYPE", then a short summary in the user's language. For Claude Channel, call ack first, then show this entry before other work. Before sending a request or verdict, show the same compact heading and a useful summary. Keep exact IDs in transport/tool calls; do not dump protocol metadata into the conversation. A queued receipt is not delivery or approval. Report an APPROVED verdict to the user once and stop; never send an acknowledgement back to the peer.`

func messageLabel(m message) string {
	switch m.Kind {
	case "review_request":
		return "REVIEW REQUEST"
	case "verdict":
		return m.Verdict
	default:
		return "MESSAGE"
	}
}

func communicationHeading(m message, state string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s → %s\n│ %s", communicationPrefix, strings.ToUpper(m.From), strings.ToUpper(m.To), messageLabel(m))
	if state != "" {
		fmt.Fprintf(&b, " · %s", state)
	}
	if m.ReplyTo != "" {
		fmt.Fprintf(&b, "\n│ Reply to: %s", m.ReplyTo)
	}
	b.WriteString("\n╰──────────────────────────────────────\n")
	return b.String()
}

func communicationCard(m message, state string, preview bool) string {
	text := m.Text
	if preview {
		text = messagePreview(text)
	}
	return communicationHeading(m, state) + text
}

// Receipts stay compact even for a 64 KiB review. Delivery keeps the full body.
func messagePreview(text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	truncated := len(lines) > 8
	if truncated {
		lines = lines[:8]
	}
	runes := []rune(strings.Join(lines, "\n"))
	if len(runes) > 480 {
		runes, truncated = runes[:480], true
		for i := len(runes) - 1; i >= 360; i-- {
			if unicode.IsSpace(runes[i]) {
				runes = runes[:i]
				break
			}
		}
	}
	text = strings.TrimRightFunc(string(runes), unicode.IsSpace)
	if truncated {
		text += "\n… (full message retained in the queue)"
	}
	return text
}

func queuedReceipt(m message) string {
	// Keep the established machine-readable receipt line for shell callers.
	return "Queued message: " + m.ID + "\n\n" + communicationCard(m, "queued", true)
}

func acceptedReceipt(dir, id string) string {
	receipt := "Message accepted: " + id
	data, err := os.ReadFile(filepath.Join(dir, "messages", id+".json"))
	if err == nil {
		if m, err := parseMessage(id+".json", data); err == nil {
			receipt += "\n\n" + communicationCard(m, "accepted", true)
		}
	}
	return receipt
}

func paneMessageBadge(messages []message, role string, width int) string {
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.From != role && m.To != role {
			continue
		}
		direction, peer := "→", m.To
		if m.To == role {
			direction, peer = "←", m.From
		}
		if width > 0 && width < 60 {
			label := map[string]string{"REVIEW REQUEST": "review", "APPROVED": "approved", "CORRECTIONS": "fixes", "MESSAGE": "note"}[messageLabel(m)]
			peer := map[string]string{roleWorker: "W", roleReviewer: "R", senderUser: "U"}[peer]
			return fmt.Sprintf("%s%s · %s · %s", direction, peer, m.Status, label)
		}
		return fmt.Sprintf("%s %s · %s · %s", direction, strings.ToUpper(peer), m.Status, messageLabel(m))
	}
	if role == roleReviewer {
		return "Waiting for WORKER"
	}
	return "Waiting for task"
}

// This read-only tmux format job works independently of the bridge version
// and never includes message bodies, queue paths or shell text in the badge.
func printPaneBadge(dir, role string, width int) error {
	if !validRole(role) {
		return fmt.Errorf("unknown badge role %q", role)
	}
	if err := validatePrivateDirectory(dir); err != nil {
		return err
	}
	if err := validatePrivateDirectory(filepath.Join(dir, "messages")); err != nil {
		return err
	}
	messages, problems, err := scanMessages(dir)
	if err != nil || len(problems) > 0 {
		fmt.Println("Queue needs attention")
		return nil
	}
	fmt.Println(paneMessageBadge(messages, role, width))
	return nil
}

func paneBorderFormat(binary, dir string) string {
	// Escape tmux expansions as well as shell metacharacters in literal paths.
	quote := func(s string) string { return strings.ReplaceAll(shellQuote(s), "#", "##") }
	job := "#(" + quote(binary) + " _badge " + quote(dir) + " #{@twomux_role} #{pane_width})"
	label := "#{?#{>=:#{pane_width},60},#{@twomux_label},#{@twomux_short_label}}"
	header := "#{?#{==:#{@twomux_role},worker},#[fg=colour81],#[fg=colour141]}#[bold]" + label + "#[default] │ " + job
	return " #{?@twomux_role," + header + ",#{pane_title}} "
}

func configureCommunicationUI(name, dir string) error {
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	for _, role := range roles {
		pane, exists, _, err := registeredPane(name, role)
		if err != nil || !exists {
			continue // Preserve start's recovery behavior for a missing role pane.
		}
		label := strings.ToUpper(role) + " · " + map[string]string{roleWorker: "Codex", roleReviewer: "Claude"}[role]
		for _, option := range [][2]string{{"@twomux_role", role}, {"@twomux_label", label}, {"@twomux_short_label", strings.ToUpper(role)}} {
			if _, err := tmux("set-option", "-p", "-t", pane, option[0], option[1]); err != nil {
				return err
			}
		}
		for _, option := range [][2]string{{"pane-border-status", "top"}, {"pane-border-format", paneBorderFormat(binary, dir)}} {
			if _, err := tmux("set-option", "-w", "-t", pane, option[0], option[1]); err != nil {
				return err
			}
		}
	}
	return nil
}
